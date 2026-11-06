package worker

// push --local --upload-source (D059), end to end: a PC agent's worker
// uploads a local source through the worker API into filesystem and S3
// storage, and the asset's source moves there.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/visheshgubrani/clipmux/internal/db"
	"github.com/visheshgubrani/clipmux/internal/queue"
	"github.com/visheshgubrani/clipmux/internal/storage"
)

// pcKey authenticates as the PC agent agt_pc.
type pcKey struct{}

func (pcKey) AuthenticateWorker(_ context.Context, key string) (queue.Worker, bool) {
	return queue.Worker{Executor: "agent", AgentID: "agt_pc"}, key == "k"
}

// localSources answers the claim's question the agents service answers in
// serve: which agent source an asset's file stays on.
type localSources struct{ pool *pgxpool.Pool }

func (localSources) ReportClaim(*http.Request, string, json.RawMessage, bool) {}
func (localSources) ReportBeat(*http.Request, string)                         {}
func (localSources) RegisterWorker(*http.ServeMux)                            {}
func (l localSources) LocalSource(ctx context.Context, assetID string) (string, int64, error) {
	a, err := db.New(l.pool).GetAssetByID(ctx, assetID)
	if err != nil || !a.AgentSourceID.Valid {
		return "", 0, err
	}
	size, err := db.New(l.pool).GetAssetAgentSourceSize(ctx, assetID)
	return a.AgentSourceID.String, size, err
}

// localSource writes a file larger than two 32 MiB parts, registers it as
// the agent's source src_1 for an encoded asset that asked for an upload
// (the upload is queued at commit, while the asset may still be
// processing), and
// queues that upload.
func localSource(t *testing.T, r *rig) (path string, sum [32]byte) {
	t.Helper()
	ctx := context.Background()
	data := make([]byte, 70<<20)
	rng := rand.NewChaCha8([32]byte{7})
	_, _ = rng.Read(data)
	path = filepath.Join(t.TempDir(), "lecture.mp4")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	src, err := OpenSources(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f, err := localFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := src.add("src_1", f); err != nil {
		t.Fatal(err)
	}
	r.worker.Local = src
	r.api.Agents = localSources{r.pool}
	for _, sql := range []string{
		`INSERT INTO agents (id, project_id, name, key_hash, status, sandboxed) VALUES ('agt_pc', 'prj_default', 'PC', 'h', 'approved', true)`,
		`INSERT INTO agent_sources (id, project_id, agent_id, size, upload_source) VALUES ('src_1', 'prj_default', 'agt_pc', 73400320, true)`,
		`INSERT INTO assets (id, project_id, app_id, status, storage_id, source_uri, agent_source_id)
			VALUES ('ast_1', 'prj_default', 'app_dashboard', 'processing', 'sto_default', 'agent://agt_pc/src_1', 'src_1')`,
	} {
		if _, err := r.pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	if _, err := queue.Enqueue(ctx, r.pool, queue.Job{ProjectID: "prj_default", AssetID: "ast_1", Kind: "upload_source", PinnedAgentID: "agt_pc"}); err != nil {
		t.Fatal(err)
	}
	return path, sha256.Sum256(data)
}

func TestAnUploadedLocalSourceMovesToStorage(t *testing.T) {
	stores := map[string]func(*testing.T) func(string, *http.ServeMux) storage.Storage{"filesystem": fsStore, "s3": s3Store}
	for name, open := range stores {
		t.Run(name, func(t *testing.T) {
			r := newRigOn(t, pcKey{}, open(t))
			_, want := localSource(t, r)
			ctx := context.Background()
			got, err := r.worker.RunOnce(ctx)
			if err != nil || !got {
				t.Fatalf("RunOnce = %v, %v", got, err)
			}
			body, err := r.store.Get(ctx, "uploads/ast_1/source")
			if err != nil {
				t.Fatal(err)
			}
			defer body.Close()
			h := sha256.New()
			if _, err := io.Copy(h, body); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(h.Sum(nil), want[:]) {
				t.Fatal("the stored source isn't the file")
			}
			a, err := db.New(r.pool).GetAssetByID(ctx, "ast_1")
			if err != nil {
				t.Fatal(err)
			}
			// No longer tied to the PC: any worker may re-encode it now.
			if a.SourceUri.String != "uploads/ast_1/source" || a.AgentSourceID.Valid || a.Status != "processing" || !a.SourceKept {
				t.Fatalf("asset = source %q agent source %v status %s", a.SourceUri.String, a.AgentSourceID, a.Status)
			}
			var status string
			_ = r.pool.QueryRow(ctx, `SELECT status FROM jobs WHERE kind = 'upload_source'`).Scan(&status)
			if status != "succeeded" {
				t.Fatalf("upload job = %s", status)
			}
		})
	}
}

// A file edited after it was registered is never uploaded: the job fails
// for good with source_changed, and the asset stays as it was.
func TestAnEditedLocalSourceIsNotUploaded(t *testing.T) {
	r := newRigOn(t, pcKey{}, fsStore(t))
	path, _ := localSource(t, r)
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := r.worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var status, code string
	_ = r.pool.QueryRow(ctx, `SELECT status, coalesce(error_code, '') FROM jobs WHERE kind = 'upload_source'`).Scan(&status, &code)
	if status != "failed" || code != "source_changed" {
		t.Fatalf("upload job = %s %s, want failed source_changed", status, code)
	}
	a, _ := db.New(r.pool).GetAssetByID(ctx, "ast_1")
	if a.SourceUri.String != "agent://agt_pc/src_1" || !a.AgentSourceID.Valid || a.Status != "processing" {
		t.Fatalf("asset changed: %+v", a)
	}
	if _, err := r.store.Head(ctx, "uploads/ast_1/source"); err == nil {
		t.Fatal("an edited file was stored")
	}
}
