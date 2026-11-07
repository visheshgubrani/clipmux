package e2e

// Fakes and helpers for subtitles and chapters on the real binary (P5
// track B): an OpenAI-compatible speech API and chat API that serve calls
// out to, the owner connecting them, and waiting for an asset's tracks.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// Keys the fakes require; neither may appear in a row, a response or a log.
const (
	speechKey = "sk-speech-E2E-must-never-show"
	llmKey    = "sk-llm-E2E-must-never-show"
)

// The fake speech API hears the same two segments in any audio. Without
// word timings each segment is one cue (D076).
const (
	cueText  = "Hello from ClipMux."
	cueStart = 1.0
	cueEnd   = 3.0
	cue2Text = "Chapter two begins."
	cue2At   = 6.0
)

// aiFakes is a speech API and a chat API on loopback.
type aiFakes struct {
	speech, llm *httptest.Server
	mu          sync.Mutex
	// transcriptions counts requests to /audio/transcriptions; chats to
	// /chat/completions.
	transcriptions, chats int
	// hold, while set, keeps every transcription waiting until it closes
	// or the caller goes away (the drain test).
	hold chan struct{}
	// entered is signalled as each held transcription arrives.
	entered chan struct{}
}

var segmentID = regexp.MustCompile(`(?m)^(s\d+): `)

func newAIFakes(t *testing.T) *aiFakes {
	t.Helper()
	f := &aiFakes{entered: make(chan struct{}, 16)}
	f.speech = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+speechKey {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"bad key"}}`)
			return
		}
		switch r.URL.Path {
		case "/v1/models":
			_, _ = io.WriteString(w, `{"data":[{"id":"whisper-1"}]}`)
		case "/v1/audio/transcriptions":
			_ = r.ParseMultipartForm(64 << 20)
			f.mu.Lock()
			f.transcriptions++
			hold := f.hold
			f.mu.Unlock()
			if hold != nil {
				f.entered <- struct{}{}
				select {
				case <-hold:
				case <-r.Context().Done():
					return
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"language": "english", "duration": 11.0, "text": " " + cueText + " " + cue2Text,
				"segments": []map[string]any{
					{"start": cueStart, "end": cueEnd, "text": " " + cueText},
					{"start": cue2At, "end": cue2At + 2, "text": " " + cue2Text},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.speech.Close)
	f.llm = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+llmKey {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"bad key"}}`)
			return
		}
		switch r.URL.Path {
		case "/v1/models":
			_, _ = io.WriteString(w, `{"data":[{"id":"gpt-test"}]}`)
		case "/v1/chat/completions":
			var req struct {
				Messages []struct{ Content string } `json:"messages"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.mu.Lock()
			f.chats++
			f.mu.Unlock()
			// The first two segments the prompt shows start the chapters.
			ids := segmentID.FindAllStringSubmatch(req.Messages[len(req.Messages)-1].Content, -1)
			chapters := []map[string]string{}
			for i, title := range []string{"Opening", "Chapter two"} {
				if i < len(ids) {
					chapters = append(chapters, map[string]string{"start_segment_id": ids[i][1], "title": title})
				}
			}
			content, _ := json.Marshal(map[string]any{"chapters": chapters})
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
				"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": string(content)},
			}}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.llm.Close)
	return f
}

// connectSpeech connects the fake speech API and sets the speech mode.
func (in *instance) connectSpeech(c *http.Client, f *aiFakes, mode string) {
	in.t.Helper()
	var res struct {
		Connected bool `json:"connected"`
		Steps     []struct {
			Name, Detail string
			OK           bool
		} `json:"steps"`
	}
	if s := ownerCall(in.t, c, "POST", in.url+"/v1/settings/speech/connect",
		map[string]any{"base_url": f.speech.URL + "/v1", "model": "whisper-1", "api_key": speechKey}, &res); s != 200 || !res.Connected {
		in.t.Fatalf("connect speech: %d %+v", s, res)
	}
	if s := ownerCall(in.t, c, "PUT", in.url+"/v1/settings/speech", map[string]any{"mode": mode}, nil); s != 200 {
		in.t.Fatalf("speech mode %s: %d", mode, s)
	}
}

// connectLLM connects the fake chat API for chapters.
func (in *instance) connectLLM(c *http.Client, f *aiFakes) {
	in.t.Helper()
	var res struct {
		Connected bool `json:"connected"`
	}
	if s := ownerCall(in.t, c, "POST", in.url+"/v1/settings/chapters/connect",
		map[string]any{"provider": "openai_compatible", "base_url": f.llm.URL + "/v1", "model": "gpt-test", "api_key": llmKey}, &res); s != 200 || !res.Connected {
		in.t.Fatalf("connect LLM: %d %+v", s, res)
	}
}

type trackView struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Status    string `json:"status"`
	Language  string `json:"language"`
	ErrorCode string `json:"error_code"`
}

// waitTracks polls the asset until none of its tracks is preparing, and
// returns them by kind.
func (in *instance) waitTracks(key, asset string, limit time.Duration) map[string]trackView {
	in.t.Helper()
	deadline := time.Now().Add(limit)
	for {
		var a struct {
			Tracks []trackView `json:"tracks"`
		}
		in.call("GET", "/v1/assets/"+asset, key, nil, &a)
		done := len(a.Tracks) > 0
		out := map[string]trackView{}
		for _, t := range a.Tracks {
			done = done && t.Status != "preparing"
			out[t.Kind] = t
		}
		if done {
			return out
		}
		if time.Now().After(deadline) {
			in.t.Fatalf("tracks still preparing after %s: %+v", limit, a.Tracks)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// noAIKeys fails if either fake's key shows in a log or in any row of any
// table.
func (in *instance) noAIKeys() {
	in.t.Helper()
	rows, err := in.pool.Query(in.t.Context(), `SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema() AND table_type = 'BASE TABLE'`)
	if err != nil {
		in.t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		tables = append(tables, n)
	}
	rows.Close()
	for _, k := range []string{speechKey, llmKey} {
		if strings.Contains(in.log(), k) {
			in.t.Errorf("serve's log holds %s", k)
		}
		for _, tbl := range tables {
			var n int
			if err := in.pool.QueryRow(in.t.Context(), `SELECT count(*) FROM "`+tbl+`" x WHERE x::text LIKE '%' || $1 || '%'`, k).Scan(&n); err != nil {
				in.t.Fatal(err)
			}
			if n > 0 {
				in.t.Errorf("table %s holds %s", tbl, k)
			}
		}
	}
}

func (f *aiFakes) String() string { return fmt.Sprintf("speech %s, llm %s", f.speech.URL, f.llm.URL) }
