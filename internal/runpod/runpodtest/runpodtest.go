// Package runpodtest is a fake RunPod REST API v2 for tests (D056, D139).
// It keeps pods and registry credentials in memory, checks the bearer key
// on every call, and records every request. Like RunPod's, it lists pods a
// page at a time, ignores filters it doesn't know, answers errors in
// RFC 9457's shape and refuses a pod request with a field RunPod's schema
// doesn't have. With Start set it also runs each pod, so the Phase 4 exit
// test can drive serve's real RunPod adapter with local
// `clipmux worker --once` processes.
package runpodtest

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
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

// createPodFields are the top-level fields of RunPod's CreatePodRequest
// (with ContainerConfig's), from its v2 API reference; it rejects any
// other with a 422.
var createPodFields = []string{
	"name", "image", "cloud", "gpu", "cpu", "env", "cmd", "args", "entrypoint", "disk", "ports",
	"registry", "mounts", "dataCenterIds", "templateId", "globalNetworking", "startJupyter", "startSsh", "queue",
}

// gpuFields are CreateGpuConfig's.
var gpuFields = []string{"id", "count", "allowedCudaVersions", "minCudaVersion", "minRamPerGpu", "minVcpuCountPerGpu"}

// Pod is a pod as the fake holds it: what was asked for, and its state.
type Pod struct {
	runpod.PodInput
	ID     string
	Status string
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
	// NoCapacity are GPU types the fake has none of: creating a pod on one
	// answers 400, as RunPod does.
	NoCapacity map[string]bool
	// PageSize is how many pods a list page holds; zero is RunPod's 1000.
	PageSize int

	// GraphQL is where the browser approval runs (D109): unauthenticated,
	// like RunPod's. Approve or Deny settles a request.
	GraphQL string
	// Catalog is the v2 API base whose /catalog/gpus lists Catalogue.
	Catalog string

	mu        sync.Mutex
	pods      map[string]*Pod
	order     []string // pod IDs, oldest first
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
	f.URL = srv.URL + "/v2"
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
		problem(w, http.StatusUnauthorized, "The API key is missing or invalid.")
		return
	}
	var body map[string]any
	raw, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	f.calls = append(f.calls, Call{Method: r.Method, Path: r.URL.Path, Body: body})
	f.mu.Unlock()

	path := strings.TrimPrefix(r.URL.Path, "/v2")
	switch {
	case r.Method == http.MethodGet && path == "/catalog/gpus":
		writeJSON(w, http.StatusOK, map[string]any{"gpus": Catalogue})
	case r.Method == http.MethodPost && path == "/pods":
		f.create(w, raw, body)
	case r.Method == http.MethodGet && path == "/pods":
		f.list(w, r.URL.Query().Get("cursor"))
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/pods/"):
		f.delete(w, strings.TrimPrefix(path, "/pods/"))
	case r.Method == http.MethodPost && path == "/registries":
		name, _ := body["name"].(string)
		user, _ := body["username"].(string)
		pass, _ := body["password"].(string)
		if name == "" || user == "" || pass == "" || len(body) != 3 {
			problem(w, http.StatusUnprocessableEntity, "The request body is invalid.")
			return
		}
		f.mu.Lock()
		f.nextID++
		id := "reg" + strconv.Itoa(f.nextID)
		f.auths[id] = name
		f.mu.Unlock()
		writeJSON(w, http.StatusCreated, map[string]string{"id": id, "name": name})
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/registries/"):
		f.mu.Lock()
		id := strings.TrimPrefix(path, "/registries/")
		_, ok := f.auths[id]
		delete(f.auths, id)
		f.mu.Unlock()
		if !ok {
			problem(w, http.StatusNotFound, "registry not found")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		problem(w, http.StatusNotFound, "no such route")
	}
}

func (f *Fake) create(w http.ResponseWriter, raw []byte, body map[string]any) {
	var unknown []string
	for k := range body {
		if !slices.Contains(createPodFields, k) {
			unknown = append(unknown, "$: additional properties '"+k+"' not allowed")
		}
	}
	gpu, _ := body["gpu"].(map[string]any)
	for k := range gpu {
		if !slices.Contains(gpuFields, k) {
			unknown = append(unknown, "$.gpu: additional properties '"+k+"' not allowed")
		}
	}
	var in runpod.PodInput
	switch {
	case len(unknown) > 0:
		slices.Sort(unknown)
		problem(w, http.StatusUnprocessableEntity, "The request body is invalid.", unknown...)
		return
	case json.Unmarshal(raw, &in) != nil || in.Name == "" || in.Image == "":
		problem(w, http.StatusUnprocessableEntity, "The request body is invalid.", "$: name and image are required")
		return
	case in.GPU == nil || in.GPU.ID == "":
		problem(w, http.StatusBadRequest, "Exactly one of gpu or cpu must be set.")
		return
	}
	f.mu.Lock()
	if f.FailCreates > 0 {
		f.FailCreates--
		f.mu.Unlock()
		problem(w, http.StatusInternalServerError, "Something went wrong.")
		return
	}
	if f.NoCapacity[in.GPU.ID] {
		f.mu.Unlock()
		problem(w, http.StatusBadRequest, "There are no instances available with the requested specifications.")
		return
	}
	p := f.add(in)
	start := f.Start
	f.mu.Unlock()
	if start != nil {
		stop := start(*p)
		f.mu.Lock()
		f.stops[p.ID] = stop
		f.mu.Unlock()
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": p.ID, "name": p.Name, "status": p.Status})
}

// add holds a new running pod; f.mu is held.
func (f *Fake) add(in runpod.PodInput) *Pod {
	f.nextID++
	p := &Pod{PodInput: in, ID: "pod" + strconv.Itoa(f.nextID), Status: "RUNNING"}
	f.pods[p.ID] = p
	f.order = append(f.order, p.ID)
	return p
}

// list answers one page, newest first, as RunPod does. The cursor is the
// offset of the page's first pod.
func (f *Fake) list(w http.ResponseWriter, cursor string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	from := 0
	if cursor != "" {
		n, err := strconv.Atoi(strings.TrimPrefix(cursor, "c"))
		if err != nil || !strings.HasPrefix(cursor, "c") || n < 0 {
			problem(w, http.StatusUnprocessableEntity, "The cursor is invalid.")
			return
		}
		from = n
	}
	size := f.PageSize
	if size == 0 {
		size = 1000
	}
	out := []map[string]any{}
	for i := len(f.order) - 1 - from; i >= 0 && len(out) < size; i-- {
		p := f.pods[f.order[i]]
		out = append(out, map[string]any{"id": p.ID, "name": p.Name, "status": p.Status})
	}
	page := map[string]any{"nextCursor": nil, "hasNextPage": false}
	if next := from + len(out); next < len(f.order) {
		page = map[string]any{"nextCursor": "c" + strconv.Itoa(next), "hasNextPage": true}
	}
	writeJSON(w, http.StatusOK, map[string]any{"pods": out, "pagination": page})
}

func (f *Fake) delete(w http.ResponseWriter, id string) {
	f.mu.Lock()
	_, ok := f.pods[id]
	if ok {
		delete(f.pods, id)
		f.order = slices.DeleteFunc(f.order, func(o string) bool { return o == id })
	}
	stop := f.stops[id]
	delete(f.stops, id)
	f.mu.Unlock()
	if stop != nil {
		stop()
	}
	if !ok {
		problem(w, http.StatusNotFound, "pod not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Pods returns the pods that aren't terminated.
func (f *Fake) Pods() []Pod {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Pod, 0, len(f.pods))
	for _, id := range f.order {
		out = append(out, *f.pods[id])
	}
	return out
}

// AddPod adds a pod the fake runs without being asked, such as one a
// crash left behind or one the owner started by hand.
func (f *Fake) AddPod(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.add(runpod.PodInput{Name: name}).ID
}

// SetStatus moves a pod to status, such as EXITED when its container
// stopped or ERROR when it failed.
func (f *Fake) SetStatus(id, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.pods[id]; ok {
		p.Status = status
	}
}

// Calls returns every request so far.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

// Created counts pod creations, including refused ones.
func (f *Fake) Created() int {
	n := 0
	for _, c := range f.Calls() {
		if c.Method == http.MethodPost && c.Path == "/v2/pods" {
			n++
		}
	}
	return n
}

// Auths returns the stored registry credentials, by ID.
func (f *Fake) Auths() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for k, v := range f.auths {
		out[k] = v
	}
	return out
}

// problem writes an RFC 9457 error, as RunPod's v2 API does.
func problem(w http.ResponseWriter, status int, detail string, errs ...string) {
	body := map[string]any{"title": http.StatusText(status), "status": status, "detail": detail}
	if len(errs) > 0 {
		body["errors"] = errs
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
