package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Local sources (`clipmux push --local`): a file that stays on this PC. The
// agent registers it with the server by size alone and keeps the path here,
// in sources.json beside agent.json; jobs on it are pinned to this agent and
// read it where it lies.

// Errors that end a job on a local source for good: only the owner, at
// this PC, can fix them.
var (
	ErrSourceMissing = errors.New("the local source file is gone")
	ErrSourceChanged = errors.New("the local source file changed since it was pushed")
)

// LocalFile is what the agent remembers about a registered file.
type LocalFile struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mod_time_ns"`
}

// Sources is this agent's registry of local files, by source ID.
type Sources struct {
	path string
	mu   sync.Mutex
	m    map[string]LocalFile
}

// OpenSources loads the registry kept in dir.
func OpenSources(dir string) (*Sources, error) {
	s := &Sources{path: filepath.Join(dir, "sources.json"), m: map[string]LocalFile{}}
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open local sources: %w", err)
	}
	if err := json.Unmarshal(raw, &s.m); err != nil {
		return nil, fmt.Errorf("open local sources: %w", err)
	}
	return s, nil
}

func (s *Sources) save() error {
	raw, _ := json.MarshalIndent(s.m, "", "  ") // a map of plain structs always marshals
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("save local sources: %w", err)
	}
	return os.Rename(tmp, s.path)
}

func (s *Sources) add(id string, f LocalFile) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[id] = f
	return s.save()
}

// IDs lists the registered sources.
func (s *Sources) IDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.m))
	for id := range s.m {
		out = append(out, id)
	}
	return out
}

// Forget drops sources the server no longer needs.
func (s *Sources) Forget(ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		delete(s.m, id)
	}
	return s.save()
}

// Resolve returns a registered file's path once it is checked to be the
// same file that was pushed: same size, same modification time.
func (s *Sources) Resolve(id string, size int64) (string, error) {
	s.mu.Lock()
	f, ok := s.m[id]
	s.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("%w: this PC has no source %s", ErrSourceMissing, id)
	}
	return f.Path, f.check(size)
}

func (f LocalFile) check(size int64) error {
	fi, err := os.Stat(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %s", ErrSourceMissing, f.Path)
	}
	if err != nil {
		return err
	}
	if fi.Size() != size || fi.Size() != f.Size || fi.ModTime().UnixNano() != f.ModTime {
		return fmt.Errorf("%w: %s", ErrSourceChanged, f.Path)
	}
	return nil
}

// localFile checks a path offered for push --local: an existing, non-empty
// regular file after resolving symlinks, inside one of roots when any are
// set.
func localFile(path string, roots []string) (LocalFile, error) {
	if !filepath.IsAbs(path) {
		return LocalFile{}, errors.New("the path must be absolute")
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return LocalFile{}, err
	}
	if len(roots) > 0 {
		inside := false
		for _, r := range roots {
			rr, err := filepath.EvalSymlinks(r)
			if err != nil {
				continue
			}
			if rel, err := filepath.Rel(rr, real); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
				inside = true
				break
			}
		}
		if !inside {
			return LocalFile{}, fmt.Errorf("%s is outside the folders this agent may read (roots in agent.json)", real)
		}
	}
	fi, err := os.Stat(real)
	if err != nil {
		return LocalFile{}, err
	}
	if !fi.Mode().IsRegular() || fi.Size() == 0 {
		return LocalFile{}, fmt.Errorf("%s is not a non-empty regular file", real)
	}
	return LocalFile{Path: real, Size: fi.Size(), ModTime: fi.ModTime().UnixNano()}, nil
}

// SocketPath is the agent's control socket in dir.
func SocketPath(dir string) string { return filepath.Join(dir, "agent.sock") }

// LocalRequest asks the agent to register a file.
type LocalRequest struct {
	Path string `json:"path"`
	// Attach, when set, also creates the asset as the owner's upload, for
	// a push without an API key.
	Attach         bool   `json:"attach"`
	PlaybackPolicy string `json:"playback_policy,omitempty"`
	Passthrough    string `json:"passthrough,omitempty"`
}

// LocalResponse is a registered file, and its asset when attached.
type LocalResponse struct {
	SourceID string          `json:"source_id"`
	Size     int64           `json:"size"`
	Asset    json.RawMessage `json:"asset,omitempty"`
}

// Control is the agent's side of push --local: a Unix socket only this
// user can open, which registers files with the server.
type Control struct {
	Worker  *Worker
	Sources *Sources
	Roots   []string
	Logger  *slog.Logger
}

// Serve listens on the socket in dir until ctx ends.
func (c *Control) Serve(ctx context.Context, dir string) error {
	path := SocketPath(dir)
	_ = os.Remove(path) // a socket left by a crash
	ln, err := (&net.ListenConfig{}).Listen(ctx, "unix", path)
	if err != nil {
		return fmt.Errorf("control socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return fmt.Errorf("control socket: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /local/sources", c.handleRegister)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (c *Control) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req LocalRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeLocal(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	f, err := localFile(req.Path, c.Roots)
	if err != nil {
		writeLocal(w, http.StatusBadRequest, "path_refused", err.Error())
		return
	}
	var out struct {
		ID string `json:"id"`
	}
	status, err := c.Worker.postJSON(r.Context(), "/worker/v1/sources", c.Worker.Key, map[string]any{"size": f.Size}, &out)
	if err != nil || status != http.StatusCreated {
		writeLocal(w, http.StatusBadGateway, "server_refused", fmt.Sprintf("registering with the server: HTTP %d %v", status, err))
		return
	}
	// Record the file before any job on it can exist.
	if err := c.Sources.add(out.ID, f); err != nil {
		writeLocal(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	var asset json.RawMessage
	if req.Attach {
		status, err := c.Worker.postJSON(r.Context(), "/worker/v1/sources/"+out.ID+"/attach", c.Worker.Key,
			map[string]string{"playback_policy": req.PlaybackPolicy, "passthrough": req.Passthrough}, &asset)
		if err != nil || status != http.StatusCreated {
			writeLocal(w, http.StatusBadGateway, "server_refused", fmt.Sprintf("creating the asset: HTTP %d %v", status, err))
			return
		}
	}
	c.Logger.Info("registered a local source", "source", out.ID, "path", f.Path, "size", f.Size)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(LocalResponse{SourceID: out.ID, Size: f.Size, Asset: asset})
}

func writeLocal(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

// SyncSources asks the server which local sources it no longer needs (their
// assets were deleted) and forgets them. Agents run it at start and hourly.
func (w *Worker) SyncSources(ctx context.Context, s *Sources) error {
	ids := s.IDs()
	for len(ids) > 0 {
		batch := ids[:min(len(ids), 1000)]
		ids = ids[len(batch):]
		var out struct {
			Gone []string `json:"gone"`
		}
		status, err := w.postJSON(ctx, "/worker/v1/sources/sync", w.Key, map[string]any{"ids": batch}, &out)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("sources/sync: HTTP %d", status)
		}
		if len(out.Gone) > 0 {
			if err := s.Forget(out.Gone); err != nil {
				return err
			}
			w.Logger.Info("forgot local sources the server no longer needs", "count", len(out.Gone))
		}
	}
	return nil
}

// RegisterLocal is the CLI's side: it asks the agent listening in dir to
// register path.
func RegisterLocal(ctx context.Context, dir string, req LocalRequest) (LocalResponse, error) {
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", SocketPath(dir))
		},
	}}
	raw, _ := json.Marshal(req)
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://agent/local/sources", strings.NewReader(string(raw)))
	if err != nil {
		return LocalResponse{}, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	res, err := client.Do(hreq)
	if err != nil {
		return LocalResponse{}, fmt.Errorf("no agent is running on this PC (start `clipmux worker` first): %w", err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusCreated {
		var e struct {
			Error struct{ Message string } `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		return LocalResponse{}, fmt.Errorf("the agent refused: %s", e.Error.Message)
	}
	var out LocalResponse
	return out, json.Unmarshal(data, &out)
}
