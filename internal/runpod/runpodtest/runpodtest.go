// Package runpodtest is a fake RunPod REST API for tests (D056). It keeps
// pods and registry auths in memory, checks the bearer key on every call,
// and records every request. With Start set it also runs each pod, so the
// Phase 4 exit test can drive serve's real RunPod adapter with local
// `clipmux worker --once` processes.
package runpodtest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/visheshgubrani/clipmux/internal/runpod"
)

// Key is the API key the fake accepts.
const Key = "rpa_TESTKEY0000000000000000000000000000"

// Catalogue is what the fake's GPU catalog lists, in RunPod's shape: two
// NVENC types on Secure Cloud, one without NVENC, one RunPod's NVENC
// refuses, and one only on Community Cloud.
var Catalogue = []map[string]any{
	{"id": "NVIDIA RTX A4000", "name": "RTX A4000", "memory": 16, "secure": true, "price": map[string]float64{"secure": 0.25, "community": 0.17}},
	{"id": "NVIDIA A100 80GB PCIe", "name": "A100 PCIe", "memory": 80, "secure": true, "price": map[string]float64{"secure": 1.64}},
	{"id": "NVIDIA L4", "name": "L4", "memory": 24, "secure": true, "price": map[string]float64{"secure": 0.43}},
	{"id": "NVIDIA GeForce RTX 4090", "name": "RTX 4090", "memory": 24, "secure": true, "price": map[string]float64{"secure": 0.69}},
	{"id": "NVIDIA RTX A5000", "name": "RTX A5000", "memory": 24, "secure": false, "price": map[string]float64{"community": 0.16}},
}

// Pod is a pod as the fake holds it: what was asked for, and its state.
type Pod struct {
	runpod.PodInput
	ID            string
	DesiredStatus string
}

// Call is one recorded request.
type Call struct {
	Method, Path string
	Body         map[string]any
}

// Fake is the API. Its URL is a base for runpod.NewClient.
type Fake struct {
	URL string

	// Start, if set, runs a created pod and returns how to stop it.
	Start func(Pod) (stop func())
	// FailCreates makes the next n pod creations answer 500.
	FailCreates int

	// GraphQL is where the browser approval runs (D109): unauthenticated,
	// like RunPod's. Approve or Deny settles a request.
	GraphQL string
	// Catalog is the v2 API base whose /catalog/gpus lists Catalogue.
	Catalog string

	mu        sync.Mutex
	pods      map[string]*Pod
	stops     map[string]func()
	auths     map[string]string // id → name
	approvals map[string]string // request id → PENDING, APPROVED, CONSUMED or DENIED
	calls     []Call
	nextID    int
}

// Approve has the owner approve request id on RunPod's page.
func (f *Fake) Approve(id string) { f.settle(id, "APPROVED") }

// Show has RunPod's page show request id's key to the owner instead of
// handing it to the poller, as the real console did.
func (f *Fake) Show(id string) { f.settle(id, "CONSUMED") }

// Deny has the owner refuse request id.
func (f *Fake) Deny(id string) { f.settle(id, "DENIED") }

func (f *Fake) settle(id, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.approvals[id] = status
}

// graphQL answers the two approval operations Flash's login uses.
func (f *Fake) graphQL(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.Contains(body.Query, "createFlashAuthRequest"):
		f.nextID++
		id := "req" + strconv.Itoa(f.nextID)
		f.approvals[id] = "PENDING"
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"createFlashAuthRequest": map[string]any{"id": id, "status": "PENDING", "expiresAt": "2099-01-01T00:00:00Z"}}})
	case strings.Contains(body.Query, "flashAuthRequestStatus"):
		id, _ := body.Variables["flashAuthRequestId"].(string)
		status, ok := f.approvals[id]
		if !ok {
			writeJSON(w, http.StatusOK, map[string]any{"errors": []map[string]string{{"message": "not found"}}})
			return
		}
		var key any
		if status == "APPROVED" {
			key = Key
			f.approvals[id] = "CONSUMED"
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"flashAuthRequestStatus": map[string]any{"id": id, "status": status, "apiKey": key}}})
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

// New starts a fake API for the test.
func New(t *testing.T) *Fake {
	t.Helper()
	f := &Fake{pods: map[string]*Pod{}, stops: map[string]func(){}, auths: map[string]string{}, approvals: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(func() {
		srv.Close()
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, stop := range f.stops {
			stop()
		}
	})
	f.URL = srv.URL + "/v1"
	f.GraphQL = srv.URL + "/graphql"
	f.Catalog = srv.URL + "/v2"
	return f
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/graphql" && r.Method == http.MethodPost {
		f.graphQL(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+Key {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var body map[string]any
	raw, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	f.calls = append(f.calls, Call{Method: r.Method, Path: r.URL.Path, Body: body})
	f.mu.Unlock()

	if r.URL.Path == "/v2/catalog/gpus" {
		writeJSON(w, http.StatusOK, map[string]any{"gpus": Catalogue})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1")
	switch {
	case r.Method == http.MethodPost && path == "/pods":
		f.create(w, raw)
	case r.Method == http.MethodGet && path == "/pods":
		f.list(w, r.URL.Query().Get("name"))
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/pods/"):
		f.delete(w, strings.TrimPrefix(path, "/pods/"))
	case r.Method == http.MethodPost && path == "/containerregistryauth":
		f.mu.Lock()
		f.nextID++
		id := "cra" + strconv.Itoa(f.nextID)
		f.auths[id], _ = body["name"].(string)
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]string{"id": id, "name": f.auths[id]})
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/containerregistryauth/"):
		f.mu.Lock()
		id := strings.TrimPrefix(path, "/containerregistryauth/")
		_, ok := f.auths[id]
		delete(f.auths, id)
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *Fake) create(w http.ResponseWriter, raw []byte) {
	var in runpod.PodInput
	if err := json.Unmarshal(raw, &in); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	if f.FailCreates > 0 {
		f.FailCreates--
		f.mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	f.nextID++
	p := &Pod{PodInput: in, ID: "pod" + strconv.Itoa(f.nextID), DesiredStatus: "RUNNING"}
	f.pods[p.ID] = p
	start := f.Start
	f.mu.Unlock()
	if start != nil {
		stop := start(*p)
		f.mu.Lock()
		f.stops[p.ID] = stop
		f.mu.Unlock()
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": p.ID, "name": p.Name, "desiredStatus": p.DesiredStatus})
}

func (f *Fake) list(w http.ResponseWriter, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []map[string]any{}
	for _, p := range f.pods {
		if name == "" || p.Name == name {
			out = append(out, map[string]any{"id": p.ID, "name": p.Name, "desiredStatus": p.DesiredStatus})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (f *Fake) delete(w http.ResponseWriter, id string) {
	f.mu.Lock()
	p, ok := f.pods[id]
	if ok {
		delete(f.pods, id)
	}
	stop := f.stops[id]
	delete(f.stops, id)
	f.mu.Unlock()
	if stop != nil {
		stop()
	}
	if !ok || p == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Pods returns the live pods.
func (f *Fake) Pods() []Pod {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Pod, 0, len(f.pods))
	for _, p := range f.pods {
		out = append(out, *p)
	}
	return out
}

// AddPod adds a pod the fake runs without being asked, such as one a
// crash left behind or one the owner started by hand.
func (f *Fake) AddPod(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := "pod" + strconv.Itoa(f.nextID)
	f.pods[id] = &Pod{PodInput: runpod.PodInput{Name: name}, ID: id, DesiredStatus: "RUNNING"}
	return id
}

// Calls returns every request so far.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

// Created counts pod creations, including failed ones.
func (f *Fake) Created() int {
	n := 0
	for _, c := range f.Calls() {
		if c.Method == http.MethodPost && c.Path == "/v1/pods" {
			n++
		}
	}
	return n
}

// Auths returns the stored registry auths, by ID.
func (f *Fake) Auths() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for k, v := range f.auths {
		out[k] = v
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
