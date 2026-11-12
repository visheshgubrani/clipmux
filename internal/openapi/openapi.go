// Package openapi holds the OpenAPI 3.1 description of the integration API
// (D112): what an app's backend, its uploader and its player may call, and
// the webhooks it receives. Serve publishes it unauthenticated, so an agent
// or a code generator reads the version the instance actually runs.
package openapi

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

//go:embed openapi.json
var spec []byte

// Handler serves the spec as application/json.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=300")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		_, _ = w.Write(spec) // a failed write means the client went away
	})
}

// Operation is one method and path the spec describes.
type Operation struct {
	ID     string
	Method string
	// Path is the spec's template, e.g. /v1/assets/{id}.
	Path string
	// Media is true for delivery paths, served from the media URL.
	Media bool
	// Public is true when the operation needs no credential.
	Public bool
}

// Operations returns every operation in the spec, sorted by path then
// method.
func Operations() ([]Operation, error) {
	var doc struct {
		Security []map[string][]string `json:"security"`
		Paths    map[string]map[string]struct {
			OperationID string                 `json:"operationId"`
			Security    *[]map[string][]string `json:"security"`
			Servers     []json.RawMessage      `json:"servers"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(spec, &doc); err != nil {
		return nil, fmt.Errorf("parse openapi.json: %w", err)
	}
	var ops []Operation
	for path, item := range doc.Paths {
		for method, o := range item {
			sec := doc.Security
			if o.Security != nil {
				sec = *o.Security
			}
			ops = append(ops, Operation{
				ID: o.OperationID, Method: strings.ToUpper(method), Path: path,
				Media: len(o.Servers) > 0, Public: len(sec) == 0,
			})
		}
	}
	slices.SortFunc(ops, func(a, b Operation) int {
		return strings.Compare(a.Path+" "+a.Method, b.Path+" "+b.Method)
	})
	return ops, nil
}
