package e2e

// Checks 18 and 20 of the Phase 5 exit test on real accounts: 11 s of
// English is transcribed by OpenAI's speech API, then chapters are made by
// Claude and by OpenAI's chat API, each through Connect. exit-llm.yml runs
// it on main, weekly and by hand; without a provider's key that part is
// skipped.
//
//	CLIPMUX_E2E_LLM=1
//	OPENAI_API_KEY      the speech API (whisper-1) and an OpenAI-compatible chat API
//	ANTHROPIC_API_KEY   Claude

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestExitRealLLMAndSpeech(t *testing.T) {
	if os.Getenv("CLIPMUX_E2E_LLM") != "1" {
		t.Skip("set CLIPMUX_E2E_LLM=1 (exit-llm.yml) to run against real speech and LLM accounts")
	}
	openai, anthropic := os.Getenv("OPENAI_API_KEY"), os.Getenv("ANTHROPIC_API_KEY")
	if openai == "" {
		t.Skip("OPENAI_API_KEY is not set: the transcript comes from OpenAI's speech API")
	}
	in := start(t)
	owner := in.owner()
	key, _, _ := in.credentials("LLM")
	connect := func(path string, body map[string]any) {
		t.Helper()
		var res struct {
			Connected bool `json:"connected"`
			Steps     []struct {
				Name, Detail string
				OK           bool
			} `json:"steps"`
		}
		if s := ownerCall(t, owner, "POST", in.url+path, body, &res); s != 200 || !res.Connected {
			t.Fatalf("%s: %d %+v", path, s, res)
		}
		for _, st := range res.Steps {
			t.Logf("%s %s: %s", path, st.Name, st.Detail)
		}
	}
	connect("/v1/settings/speech/connect", map[string]any{"base_url": "https://api.openai.com/v1", "model": "whisper-1", "api_key": openai})
	if s := ownerCall(t, owner, "PUT", in.url+"/v1/settings/speech", map[string]any{"mode": "api"}, nil); s != 200 {
		t.Fatalf("speech mode: %d", s)
	}

	llms := []struct {
		name string
		key  string
		body map[string]any
	}{
		{"claude", anthropic, map[string]any{"provider": "anthropic", "api_key": anthropic}},
		{"openai", openai, map[string]any{"provider": "openai_compatible", "base_url": "https://api.openai.com/v1", "model": "gpt-4.1-mini", "api_key": openai}},
	}
	var asset string
	for _, l := range llms {
		t.Run(l.name, func(t *testing.T) {
			if l.key == "" {
				t.Skipf("no key for %s", l.name)
			}
			connect("/v1/settings/chapters/connect", l.body)
			if asset == "" {
				asset, _ = in.uploadWith(key, speechFixture(t), map[string]any{"generate": map[string]bool{"chapters": true}})
				in.waitReady(key, asset, 5*time.Minute)
				subs := in.waitTracks(key, asset, 10*time.Minute)["subtitles"]
				var tr struct{ Language, Text string }
				in.call("GET", "/v1/assets/"+asset+"/tracks/"+subs.ID+"/transcript", key, nil, &tr)
				if subs.Status != "ready" || tr.Language != "en" || !strings.Contains(strings.ToLower(tr.Text), "ask not what your country") {
					t.Fatalf("subtitles through OpenAI = %+v %+v", subs, tr)
				}
			} else if s := in.call("POST", "/v1/assets/"+asset+"/tracks", key, map[string]any{"kind": "chapters"}, nil); s != 202 {
				t.Fatalf("regenerate chapters: %d", s)
			}
			deadline := time.Now().Add(10 * time.Minute)
			for {
				var a struct{ Tracks []trackView }
				in.call("GET", "/v1/assets/"+asset, key, nil, &a)
				var ch *trackView
				for i, tr := range a.Tracks {
					if tr.Kind == "chapters" {
						ch = &a.Tracks[i]
					}
				}
				if ch != nil && ch.Status == "errored" {
					t.Fatalf("chapters through %s errored %s", l.name, ch.ErrorCode)
				}
				if ch != nil && ch.Status == "ready" && in.scalar(`SELECT count(*)::text FROM jobs WHERE asset_id = $1 AND kind = 'chapters' AND status IN ('queued', 'running')`, asset) == "0" {
					var d struct {
						Chapters []struct {
							StartMS int64  `json:"start_ms"`
							Title   string `json:"title"`
						} `json:"chapters"`
					}
					in.call("GET", "/v1/assets/"+asset+"/tracks/"+ch.ID, key, nil, &d)
					if len(d.Chapters) == 0 || d.Chapters[0].StartMS != 0 {
						t.Fatalf("chapters through %s = %+v", l.name, d.Chapters)
					}
					t.Logf("chapters through %s: %+v", l.name, d.Chapters)
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("chapters through %s never became ready", l.name)
				}
				time.Sleep(2 * time.Second)
			}
		})
	}
	for _, k := range []string{openai, anthropic} {
		if k != "" && strings.Contains(in.log(), k) {
			t.Fatal("serve's log holds a provider key")
		}
	}
}
