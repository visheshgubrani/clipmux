package openapi

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The public API reference (clipmux.com/docs/reference/api) is rendered from
// this spec, so it must read as documentation: every operation described and
// tagged, every tag described and grouped, every JSON body with an example
// that matches its schema, and nothing internal in the prose.

type doc struct {
	Tags      []struct{ Name, Description string } `json:"tags"`
	TagGroups []struct {
		Name string
		Tags []string
	} `json:"x-tagGroups"`
	Paths      map[string]map[string]json.RawMessage `json:"paths"`
	Webhooks   map[string]map[string]json.RawMessage `json:"webhooks"`
	Components struct {
		Schemas map[string]any `json:"schemas"`
	} `json:"components"`
}

type operation struct {
	OperationID string   `json:"operationId"`
	Summary     string   `json:"summary"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	RequestBody *struct {
		Content map[string]media `json:"content"`
	} `json:"requestBody"`
	Responses map[string]struct {
		Content map[string]media `json:"content"`
	} `json:"responses"`
}

type media struct {
	Schema  map[string]any `json:"schema"`
	Example any            `json:"example"`
}

func load(t *testing.T) doc {
	t.Helper()
	var d doc
	if err := json.Unmarshal(spec, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func eachOperation(t *testing.T, items map[string]map[string]json.RawMessage, fn func(where string, op operation)) {
	t.Helper()
	for path, item := range items {
		for method, raw := range item {
			if method == "parameters" {
				continue
			}
			var op operation
			if err := json.Unmarshal(raw, &op); err != nil {
				t.Fatalf("%s %s: %v", method, path, err)
			}
			fn(strings.ToUpper(method)+" "+path, op)
		}
	}
}

func TestEveryOperationIsDescribedAndTagged(t *testing.T) {
	d := load(t)
	declared := map[string]bool{}
	for _, tag := range d.Tags {
		if tag.Description == "" {
			t.Errorf("tag %q has no description", tag.Name)
		}
		declared[tag.Name] = true
	}
	grouped := map[string]bool{}
	for _, g := range d.TagGroups {
		for _, name := range g.Tags {
			if !declared[name] {
				t.Errorf("x-tagGroups %q names undeclared tag %q", g.Name, name)
			}
			grouped[name] = true
		}
	}
	for name := range declared {
		if !grouped[name] {
			t.Errorf("tag %q is in no x-tagGroups group", name)
		}
	}
	eachOperation(t, d.Paths, func(where string, op operation) {
		if op.Summary == "" || op.Description == "" {
			t.Errorf("%s (%s): needs a summary and a description", where, op.OperationID)
		}
		if len(op.Tags) != 1 || !declared[op.Tags[0]] {
			t.Errorf("%s (%s): tags %v, want one declared tag", where, op.OperationID, op.Tags)
		}
	})
	eachOperation(t, d.Webhooks, func(where string, op operation) {
		if op.Description == "" {
			t.Errorf("webhook %s: no description", where)
		}
	})
}

func TestEveryJSONBodyHasAnExampleThatMatchesItsSchema(t *testing.T) {
	d := load(t)
	v := validator{schemas: d.Components.Schemas}
	check := func(where string, m media) {
		if m.Example == nil {
			t.Errorf("%s: no example", where)
			return
		}
		if err := v.check(m.Schema, m.Example, "example"); err != nil {
			t.Errorf("%s: %v", where, err)
		}
	}
	both := func(where string, op operation) {
		if op.RequestBody != nil {
			if m, ok := op.RequestBody.Content["application/json"]; ok {
				check(where+" request", m)
			}
		}
		for code, r := range op.Responses {
			if m, ok := r.Content["application/json"]; ok && strings.HasPrefix(code, "2") {
				check(where+" "+code, m)
			}
		}
	}
	eachOperation(t, d.Paths, both)
	eachOperation(t, d.Webhooks, both)
}

func TestThePublicProseHasNothingInternal(t *testing.T) {
	internal := regexp.MustCompile(`\bD\d{3}\b|\bP\d+-T\d+|docs/[a-z-]+\.md|internal/[a-z]`)
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, x := range v {
				if k == "description" || k == "summary" {
					if s, ok := x.(string); ok && internal.MatchString(s) {
						t.Errorf("%s.%s: %q mentions %q", path, k, s, internal.FindString(s))
					}
				}
				walk(path+"."+k, x)
			}
		case []any:
			for i, x := range v {
				walk(fmt.Sprintf("%s[%d]", path, i), x)
			}
		}
	}
	var all any
	if err := json.Unmarshal(spec, &all); err != nil {
		t.Fatal(err)
	}
	walk("$", all)
}

func TestTheValidatorRefusesWhatTheSchemaRefuses(t *testing.T) {
	v := validator{schemas: map[string]any{
		"Thing": map[string]any{
			"type":       "object",
			"required":   []any{"id", "status"},
			"properties": map[string]any{"id": map[string]any{"type": "string"}, "status": map[string]any{"type": "string", "enum": []any{"ready"}}, "n": map[string]any{"oneOf": []any{map[string]any{"type": "integer"}, map[string]any{"type": "null"}}}},
		},
	}}
	ref := map[string]any{"$ref": "#/components/schemas/Thing"}
	for _, tc := range []struct {
		name string
		v    any
		ok   bool
	}{
		{"valid", map[string]any{"id": "a", "status": "ready", "n": nil}, true},
		{"missing required", map[string]any{"id": "a"}, false},
		{"bad enum", map[string]any{"id": "a", "status": "gone"}, false},
		{"unknown property", map[string]any{"id": "a", "status": "ready", "nope": 1}, false},
		{"wrong type", map[string]any{"id": 1.0, "status": "ready"}, false},
		{"fraction for integer", map[string]any{"id": "a", "status": "ready", "n": 1.5}, false},
	} {
		if err := v.check(ref, tc.v, "x"); (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok %v", tc.name, err, tc.ok)
		}
	}
}

// validator checks a JSON value against the subset of JSON Schema the spec
// uses: $ref, type, enum, required, properties (no unknown keys), items,
// oneOf, allOf.
type validator struct{ schemas map[string]any }

func (v validator) check(schema map[string]any, value any, at string) error {
	return v.checkOpen(schema, value, at, false)
}

// resolve follows a $ref.
func (v validator) resolve(schema map[string]any) map[string]any {
	if ref, ok := schema["$ref"].(string); ok {
		if s, ok := v.schemas[strings.TrimPrefix(ref, "#/components/schemas/")].(map[string]any); ok {
			return s
		}
	}
	return schema
}

// checkOpen checks value against schema; open allows properties the schema
// doesn't list, for the branches of an allOf, whose union is checked once.
func (v validator) checkOpen(schema map[string]any, value any, at string, open bool) error {
	if ref, ok := schema["$ref"].(string); ok {
		s := v.resolve(schema)
		if _, known := v.schemas[strings.TrimPrefix(ref, "#/components/schemas/")]; !known {
			return fmt.Errorf("%s: unknown $ref %s", at, ref)
		}
		return v.checkOpen(s, value, at, open)
	}
	if all, ok := schema["allOf"].([]any); ok {
		known := map[string]bool{}
		for _, s := range all {
			b := v.resolve(s.(map[string]any))
			if err := v.checkOpen(b, value, at, true); err != nil {
				return err
			}
			for k := range asMap(b["properties"]) {
				known[k] = true
			}
		}
		if obj, ok := value.(map[string]any); ok && !open {
			for k := range obj {
				if !known[k] {
					return fmt.Errorf("%s: unknown property %q", at, k)
				}
			}
		}
	}
	if one, ok := schema["oneOf"].([]any); ok {
		matches := 0
		for _, s := range one {
			if v.checkOpen(s.(map[string]any), value, at, open) == nil {
				matches++
			}
		}
		if matches != 1 {
			return fmt.Errorf("%s: matches %d of oneOf, want 1", at, matches)
		}
	}
	if enum, ok := schema["enum"].([]any); ok && !slices.Contains(enum, value) {
		return fmt.Errorf("%s: %v is not one of %v", at, value, enum)
	}
	switch schema["type"] {
	case "string":
		if _, ok := value.(string); !ok {
			return fmt.Errorf("%s: want a string, got %T", at, value)
		}
	case "integer":
		if n, ok := value.(float64); !ok || n != float64(int64(n)) {
			return fmt.Errorf("%s: want an integer, got %v", at, value)
		}
	case "number":
		if _, ok := value.(float64); !ok {
			return fmt.Errorf("%s: want a number, got %T", at, value)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s: want a boolean, got %T", at, value)
		}
	case "null":
		if value != nil {
			return fmt.Errorf("%s: want null, got %v", at, value)
		}
	case "array":
		items, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%s: want an array, got %T", at, value)
		}
		if is, ok := schema["items"].(map[string]any); ok {
			for i, x := range items {
				if err := v.check(is, x, fmt.Sprintf("%s[%d]", at, i)); err != nil {
					return err
				}
			}
		}
	}
	props, hasProps := schema["properties"].(map[string]any)
	if schema["type"] == "object" || hasProps {
		obj, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: want an object, got %T", at, value)
		}
		for _, r := range asSlice(schema["required"]) {
			if _, ok := obj[r.(string)]; !ok {
				return fmt.Errorf("%s: missing required %q", at, r)
			}
		}
		if hasProps {
			for k, x := range obj {
				ps, ok := props[k].(map[string]any)
				if !ok {
					if _, extra := schema["additionalProperties"]; extra || open {
						continue
					}
					return fmt.Errorf("%s: unknown property %q", at, k)
				}
				if err := v.check(ps, x, at+"."+k); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}
