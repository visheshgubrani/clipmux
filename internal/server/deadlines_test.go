package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// short are deadlines scaled down so the test runs in a second; the shipped
// ones are defaultDeadlines.
var short = deadlines{
	apiRead: 200 * time.Millisecond, apiWrite: 200 * time.Millisecond,
	longPoll: 800 * time.Millisecond, long: 800 * time.Millisecond, streamIdle: 1500 * time.Millisecond,
}

func deadlineServer(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	sleepThenOK := func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		_, _ = w.Write([]byte("ok"))
	}
	mux.HandleFunc("POST /worker/v1/claim", sleepThenOK)
	mux.HandleFunc("GET /v1/assets/{id}", sleepThenOK)
	// A stream that writes a chunk every 400 ms for about 3 s.
	mux.HandleFunc("GET /v/{token}/{pid}/flowing", func(w http.ResponseWriter, r *http.Request) {
		flush := http.NewResponseController(w)
		for i := 0; i < 8; i++ {
			if _, err := w.Write([]byte(strings.Repeat("x", 1024))); err != nil {
				return
			}
			if err := flush.Flush(); err != nil {
				return
			}
			time.Sleep(400 * time.Millisecond)
		}
	})
	// A large body for a client that stops reading: writes block once the
	// socket buffers fill, and the idle deadline must end them.
	mux.HandleFunc("GET /v/{token}/{pid}/flood", func(w http.ResponseWriter, r *http.Request) {
		chunk := []byte(strings.Repeat("x", 64<<10))
		for i := 0; i < 4096; i++ {
			if _, err := w.Write(chunk); err != nil {
				floodEnded <- time.Now()
				return
			}
		}
		floodEnded <- time.Time{}
	})
	srv := httptest.NewServer(short.wrap(mux))
	t.Cleanup(srv.Close)
	return srv.URL
}

func fetch(method, url string) (string, error) {
	req, _ := http.NewRequest(method, url, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	return string(b), err
}

func TestALongPollIsntCutAtTheAPIDeadline(t *testing.T) {
	base := deadlineServer(t)
	if body, err := fetch("POST", base+"/worker/v1/claim"); err != nil || body != "ok" {
		t.Fatalf("claim: %q %v", body, err)
	}
	// The same 400 ms on an ordinary route passes its 200 ms write deadline.
	if body, err := fetch("GET", base+"/v1/assets/ast_1"); err == nil && body == "ok" {
		t.Fatal("an API request outlived its write deadline")
	}
}

func TestAStreamOutlivesItsIdleBoundWhileBytesFlow(t *testing.T) {
	base := deadlineServer(t)
	start := time.Now()
	body, err := fetch("GET", base+"/v/tok/pid_1/flowing")
	if err != nil || len(body) != 8*1024 {
		t.Fatalf("a flowing stream was cut after %s: %d bytes, %v", time.Since(start), len(body), err)
	}
	if time.Since(start) < 2*short.streamIdle {
		t.Fatalf("the stream ran %s, not past its idle bound", time.Since(start))
	}
}

var floodEnded = make(chan time.Time, 1)

func TestAClientThatStopsReadingIsCutAfterTheIdleBound(t *testing.T) {
	base := deadlineServer(t)
	res, err := http.Get(base + "/v/tok/pid_1/flood")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if _, err := io.ReadFull(res.Body, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	stopped := time.Now() // and read nothing more
	select {
	case at := <-floodEnded:
		if at.IsZero() {
			t.Fatal("the whole body was written to a client that stopped reading")
		}
		if took := at.Sub(stopped); took < short.streamIdle/2 || took > 4*short.streamIdle {
			t.Fatalf("the stalled write ended after %s, want about %s", took, short.streamIdle)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a client that stopped reading held the stream past its idle bound")
	}
}

func TestRoutesAreClassified(t *testing.T) {
	for _, c := range []struct {
		method, path string
		want         routeClass
	}{
		{"GET", "/_storage/objects/media/a/o/720p/seg_00001.m4s", classStream},
		{"PUT", "/_storage/parts/up_1/3", classStream},
		{"GET", "/v/tok/pid/master.m3u8", classStream},
		{"GET", "/p/pid/master.m3u8", classStream},
		{"POST", "/worker/v1/claim", classLongPoll},
		{"GET", "/v1/events", classLongPoll},
		{"POST", "/worker/v1/attempts/att_1/complete", classLong},
		{"POST", "/worker/v1/attempts/att_1/heartbeat", classAPI},
		{"GET", "/worker/v1/attempts/att_1/audio", classStream},
		{"POST", "/v1/executors/runpod/connect", classLong},
		{"POST", "/v1/cloudflare/connect", classLong},
		{"POST", "/v1/setup-test", classLong},
		{"DELETE", "/v1/assets/ast_1", classLong},
		{"DELETE", "/v1/assets/ast_1/playback-ids/pid_1", classAPI},
		{"POST", "/v1/storage", classLong},
		{"PUT", "/v1/storage/sto_1/credentials", classLong},
		{"POST", "/v1/storage/sto_1/default", classAPI},
		{"DELETE", "/v1/cloudflare", classLong},
		{"GET", "/v1/assets/ast_1", classAPI},
		{"GET", "/library", classAPI},
	} {
		if got := classify(httptest.NewRequest(c.method, c.path, nil)); got != c.want {
			t.Errorf("%s %s = %d, want %d", c.method, c.path, got, c.want)
		}
	}
}
