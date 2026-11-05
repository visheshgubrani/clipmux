// Package httpjson holds the JSON request and response conventions shared by
// every ClipMux HTTP API: bodies are JSON objects of at most 64 KiB, errors
// are {"error": {"code", "message"}}, and credentials arrive as bearer tokens.
package httpjson

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// MaxBody bounds every JSON request body.
const MaxBody = 64 << 10

// Bearer returns the bearer token of a request, or "".
func Bearer(r *http.Request) string {
	if t, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return strings.TrimSpace(t)
	}
	return ""
}

// Decode reads a JSON body of at most MaxBody bytes into v. On failure it
// writes a 400 and returns false. An empty body decodes as {}.
func Decode(w http.ResponseWriter, r *http.Request, v any) bool {
	return DecodeLimit(w, r, v, MaxBody)
}

// DecodeLimit is Decode with a different size limit, for the few bodies that
// are legitimately larger (an output manifest lists every segment).
func DecodeLimit(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		Error(w, http.StatusBadRequest, "invalid_request", "body must be a JSON object")
		return false
	}
	return true
}

// Write sends v as JSON. API responses are never cached.
func Write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Error sends a JSON error with a stable snake_case code.
func Error(w http.ResponseWriter, status int, code, message string) {
	Write(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
