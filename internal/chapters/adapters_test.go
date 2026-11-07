package chapters

// Both adapters against httptest fakes: the request each sends, what each
// answer becomes, and which failures are worth another try. The Claude fake
// is reached through the SDK's option.WithBaseURL.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const llmKey = "sk-LLM_KEY_must_never_show"

// recorder is an httptest server that records requests and answers with
// the next scripted reply.
type recorder struct {
	srv     *httptest.Server
	mu      sync.Mutex
	reqs    []recorded
	replies []reply
}

type recorded struct {
	method, path string
	header       http.Header
	body         map[string]any
}

type reply struct {
	status int
	body   string
}

func newRecorder(t *testing.T, replies ...reply) *recorder {
	rec := &recorder{replies: replies}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		rec.mu.Lock()
		rec.reqs = append(rec.reqs, recorded{r.Method, r.URL.Path, r.Header.Clone(), body})
		rp := rec.replies[min(len(rec.reqs), len(rec.replies))-1]
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rp.status)
		_, _ = io.WriteString(w, rp.body)
	}))
	t.Cleanup(rec.srv.Close)
	return rec
}

func (r *recorder) requests() []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recorded(nil), r.reqs...)
}

func chatReply(content string) reply {
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{
		"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": content},
	}}})
	return reply{200, string(b)}
}

func TestOpenAISendsAStrictSchemaAndReadsTheMessage(t *testing.T) {
	rec := newRecorder(t, chatReply(outlineJSON))
	m := &OpenAI{BaseURL: rec.srv.URL + "/v1/", Model: "gpt-5-mini", Key: llmKey}
	got, err := Generate(context.Background(), m, transcript)
	if err != nil || len(got) != 2 || got[1].Title != "Topic" {
		t.Fatalf("Generate = %+v, %v", got, err)
	}
	req := rec.requests()[0]
	if req.method != "POST" || req.path != "/v1/chat/completions" || req.header.Get("Authorization") != "Bearer "+llmKey {
		t.Fatalf("request %s %s auth %q", req.method, req.path, req.header.Get("Authorization"))
	}
	msgs := req.body["messages"].([]any)
	if req.body["model"] != "gpt-5-mini" || len(msgs) != 2 ||
		msgs[0].(map[string]any)["role"] != "system" || msgs[0].(map[string]any)["content"] != SystemPrompt ||
		msgs[1].(map[string]any)["role"] != "user" || msgs[1].(map[string]any)["content"] != BuildPrompt(transcript) {
		t.Fatalf("body = %v", req.body)
	}
	rf := req.body["response_format"].(map[string]any)
	js := rf["json_schema"].(map[string]any)
	if rf["type"] != "json_schema" || js["strict"] != true || js["name"] != "chapter_outline" || js["schema"] == nil {
		t.Fatalf("response_format = %v", rf)
	}
}

func TestOpenAIJSONModeAndFencedAnswers(t *testing.T) {
	rec := newRecorder(t, chatReply("```json\n"+outlineJSON+"\n```"))
	m := &OpenAI{BaseURL: rec.srv.URL, Model: "llama3.3", Format: FormatJSONObject}
	if got, err := Generate(context.Background(), m, transcript); err != nil || len(got) != 2 {
		t.Fatalf("Generate = %+v, %v", got, err)
	}
	req := rec.requests()[0]
	if rf := req.body["response_format"].(map[string]any); rf["type"] != "json_object" || len(rf) != 1 {
		t.Fatalf("response_format = %v", rf)
	}
	if req.header.Get("Authorization") != "" {
		t.Fatal("a keyless server got an Authorization header")
	}
}

func TestOpenAIFailuresAreTransientOrPermanent(t *testing.T) {
	for _, c := range []struct {
		status    int
		transient bool
		code      string
	}{
		{429, true, CodeUnavailable}, {500, true, CodeUnavailable}, {503, true, CodeUnavailable}, {408, true, CodeUnavailable},
		{400, false, CodeRejected}, {401, false, CodeRejected}, {404, false, CodeRejected},
	} {
		rec := newRecorder(t, reply{c.status, `{"error":{"message":"nope"}}`})
		_, err := (&OpenAI{BaseURL: rec.srv.URL, Model: "m", Key: llmKey}).Complete(context.Background(), Prompt{System: "s", User: "u", Schema: Schema})
		var e *Error
		if !errors.As(err, &e) || e.Transient != c.transient || e.Code != c.code || e.Status != c.status {
			t.Errorf("HTTP %d: %#v", c.status, err)
		}
		if strings.Contains(err.Error(), llmKey) {
			t.Errorf("HTTP %d: the error holds the key", c.status)
		}
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	_, err := (&OpenAI{BaseURL: closed.URL, Model: "m", Key: llmKey}).Complete(context.Background(), Prompt{})
	var e *Error
	if !errors.As(err, &e) || !e.Transient || e.Code != CodeUnavailable {
		t.Fatalf("network error = %#v", err)
	}
}

func TestOpenAIRefusalIsPermanent(t *testing.T) {
	rec := newRecorder(t, reply{200, `{"choices":[{"message":{"role":"assistant","content":null,"refusal":"I can't help with that."}}]}`})
	_, err := Generate(context.Background(), &OpenAI{BaseURL: rec.srv.URL, Model: "m"}, transcript)
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeRefused || e.Transient || len(rec.requests()) != 1 {
		t.Fatalf("Generate = %#v after %d calls", err, len(rec.requests()))
	}
}

func TestOpenAICredentialsPassWithoutAModelsRoute(t *testing.T) {
	if err := (&OpenAI{BaseURL: newRecorder(t, reply{404, ``}).srv.URL}).CheckCredentials(context.Background()); err != nil {
		t.Fatalf("404 = %v", err)
	}
	err := (&OpenAI{BaseURL: newRecorder(t, reply{401, `{"error":{"message":"bad key"}}`}).srv.URL}).CheckCredentials(context.Background())
	if err == nil || !strings.Contains(err.Error(), "bad key") {
		t.Fatalf("401 = %v", err)
	}
}

func claudeReply(stop, text string) reply {
	b, _ := json.Marshal(map[string]any{
		"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-opus-5-5",
		"content":     []any{map[string]any{"type": "text", "text": text}},
		"stop_reason": stop, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": 10, "output_tokens": 20},
	})
	return reply{200, string(b)}
}

func TestClaudeSendsStructuredOutputAtLowEffortWithTheOwnersKey(t *testing.T) {
	// serve's own environment never reaches the request.
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-from-serves-environment")
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:1")
	rec := newRecorder(t, claudeReply("end_turn", outlineJSON))
	m := NewClaude(llmKey, "", rec.srv.URL, nil)
	got, err := Generate(context.Background(), m, transcript)
	if err != nil || len(got) != 2 || got[0].Title != "Intro" {
		t.Fatalf("Generate = %+v, %v", got, err)
	}
	reqs := rec.requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests", len(reqs))
	}
	req := reqs[0]
	if req.method != "POST" || req.path != "/v1/messages" || req.header.Get("Authorization") != "Bearer "+llmKey || req.header.Get("Anthropic-Version") == "" {
		t.Fatalf("request %s %s auth %q version %q", req.method, req.path, req.header.Get("Authorization"), req.header.Get("Anthropic-Version"))
	}
	b := req.body
	if b["model"] != "claude-opus-5-5" || b["max_tokens"] == nil {
		t.Fatalf("body = %v", b)
	}
	for _, banned := range []string{"thinking", "temperature", "top_p", "top_k"} {
		if _, ok := b[banned]; ok {
			t.Errorf("the request sets %s", banned)
		}
	}
	oc := b["output_config"].(map[string]any)
	format := oc["format"].(map[string]any)
	if oc["effort"] != "low" || format["type"] != "json_schema" || format["schema"] == nil {
		t.Fatalf("output_config = %v", oc)
	}
	msgs := b["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["role"] != "user" {
		t.Fatalf("messages = %v (a prefill?)", msgs)
	}
	sys := b["system"].([]any)
	if sys[0].(map[string]any)["text"] != SystemPrompt {
		t.Fatalf("system = %v", sys)
	}
}

func TestClaudeRefusalIsPermanent(t *testing.T) {
	r := claudeReply("refusal", "")
	r.body = strings.Replace(r.body, `"stop_sequence":null`, `"stop_sequence":null,"stop_details":{"type":"refusal","category":"cyber","explanation":"x"}`, 1)
	rec := newRecorder(t, r)
	_, err := Generate(context.Background(), NewClaude(llmKey, "claude-opus-5-5", rec.srv.URL, nil), transcript)
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeRefused || e.Transient || len(rec.requests()) != 1 {
		t.Fatalf("Generate = %#v after %d calls", err, len(rec.requests()))
	}
}

func TestClaudeFailuresAreClassifiedWithoutSDKRetries(t *testing.T) {
	for _, c := range []struct {
		status    int
		kind      string
		transient bool
	}{
		{429, "rate_limit_error", true}, {500, "api_error", true}, {529, "overloaded_error", true},
		{400, "invalid_request_error", false}, {401, "authentication_error", false}, {404, "not_found_error", false},
	} {
		rec := newRecorder(t, reply{c.status, `{"type":"error","error":{"type":"` + c.kind + `","message":"nope"}}`})
		_, err := NewClaude(llmKey, "", rec.srv.URL, nil).Complete(context.Background(), Prompt{System: "s", User: "u", Schema: Schema})
		var e *Error
		if !errors.As(err, &e) || e.Transient != c.transient || e.Status != c.status {
			t.Errorf("HTTP %d: %#v", c.status, err)
			continue
		}
		if want := map[bool]string{true: CodeUnavailable, false: CodeRejected}[c.transient]; e.Code != want {
			t.Errorf("HTTP %d: code %s, want %s", c.status, e.Code, want)
		}
		if n := len(rec.requests()); n != 1 {
			t.Errorf("HTTP %d: %d requests, want 1 (the job's budget retries, not the SDK)", c.status, n)
		}
		if strings.Contains(err.Error(), llmKey) {
			t.Errorf("HTTP %d: the error holds the key", c.status)
		}
	}
}

func TestClaudeCredentialsReadTheModel(t *testing.T) {
	rec := newRecorder(t, reply{200, `{"type":"model","id":"claude-opus-5-5","display_name":"Claude Opus 5.5","created_at":"2026-01-01T00:00:00Z"}`})
	if err := NewClaude(llmKey, "", rec.srv.URL, nil).CheckCredentials(context.Background()); err != nil {
		t.Fatal(err)
	}
	if req := rec.requests()[0]; req.method != "GET" || req.path != "/v1/models/claude-opus-5-5" || req.header.Get("Authorization") != "Bearer "+llmKey {
		t.Fatalf("request %s %s", req.method, req.path)
	}
}
