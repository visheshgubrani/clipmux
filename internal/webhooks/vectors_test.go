package webhooks

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"strconv"
	"testing"
)

var update = flag.Bool("update", false, "regenerate testdata/vectors.json from Sign")

const vectorsFile = "testdata/vectors.json"

// vectorFile is the webhook contract a receiver in any language checks
// itself against (D112): what ClipMux sends, and whether a receiver that
// follows docs/api.md ("Webhooks") must accept it.
type vectorFile struct {
	Description      string       `json:"description"`
	ToleranceSeconds int64        `json:"tolerance_seconds"`
	Cases            []vectorCase `json:"cases"`
}

type vectorCase struct {
	Name string `json:"name"`
	// Secret is the endpoint secret as the owner sees it, prefix included.
	Secret string `json:"secret"`
	// Now is the receiver's clock, in Unix seconds.
	Now  int64  `json:"now"`
	Body string `json:"body"`
	// Headers are the request headers; a missing one is left out.
	Headers map[string]string `json:"headers"`
	Valid   bool              `json:"valid"`
	// Reason is why a receiver refuses it: missing_header, stale or
	// bad_signature.
	Reason string `json:"reason,omitempty"`
}

const (
	vectorSecret = "whsec_TESTSECRETNOTREAL0000000000000"
	vectorTime   = 1800000000
	vectorBody   = `{"id":"evt_01jd0000000000000000000000","type":"video.asset.ready","created_at":"2027-01-15T08:00:00Z","data":{"id":"ast_01jd0000000000000000000000","status":"ready","version":3,"passthrough":"lesson-42"}}`
)

// buildVectors makes every case from Sign, so -update rewrites the file and
// a normal run proves the file still matches what ClipMux sends.
func buildVectors() vectorFile {
	sig := func(secret string, ts int64, body string) string {
		return Sign([]byte(secret), ts, []byte(body))
	}
	headers := func(ts, signature string) map[string]string {
		return map[string]string{
			"X-Webhook-Event":     "video.asset.ready",
			"X-Webhook-Id":        "evt_01jd0000000000000000000000",
			"X-Webhook-Timestamp": ts,
			"X-Webhook-Signature": signature,
		}
	}
	good := sig(vectorSecret, vectorTime, vectorBody)
	without := func(name string) map[string]string {
		h := headers("1800000000", good)
		delete(h, name)
		return h
	}
	return vectorFile{
		Description:      "Webhook signature vectors (D018, D112). Regenerate with: go test ./internal/webhooks -run TestVectors -update",
		ToleranceSeconds: 300,
		Cases: []vectorCase{
			{Name: "lms", Secret: "test-secret", Now: 1700000000, Body: `{"job_id":"abc","stage":"video"}`,
				Headers: headers("1700000000", "sha256=fac6876fcc981593ebc1248ba3ce771b001862c52f3863ff0a728f73c6a44e2f"), Valid: true},
			{Name: "ready", Secret: vectorSecret, Now: vectorTime, Body: vectorBody, Headers: headers("1800000000", good), Valid: true},
			{Name: "clock_behind_within_tolerance", Secret: vectorSecret, Now: vectorTime - 300, Body: vectorBody, Headers: headers("1800000000", good), Valid: true},
			{Name: "clock_ahead_within_tolerance", Secret: vectorSecret, Now: vectorTime + 300, Body: vectorBody, Headers: headers("1800000000", good), Valid: true},
			{Name: "stale", Secret: vectorSecret, Now: vectorTime + 301, Body: vectorBody, Headers: headers("1800000000", good), Reason: "stale"},
			{Name: "future", Secret: vectorSecret, Now: vectorTime - 301, Body: vectorBody, Headers: headers("1800000000", good), Reason: "stale"},
			{Name: "tampered_body", Secret: vectorSecret, Now: vectorTime, Body: vectorBody[:len(vectorBody)-2] + ` }`, Headers: headers("1800000000", good), Reason: "bad_signature"},
			{Name: "reformatted_body", Secret: vectorSecret, Now: vectorTime, Body: "  " + vectorBody, Headers: headers("1800000000", good), Reason: "bad_signature"},
			{Name: "wrong_secret", Secret: "whsec_SOMEOTHERSECRET000000000000000", Now: vectorTime, Body: vectorBody, Headers: headers("1800000000", good), Reason: "bad_signature"},
			{Name: "secret_without_prefix", Secret: vectorSecret[len("whsec_"):], Now: vectorTime, Body: vectorBody, Headers: headers("1800000000", good), Reason: "bad_signature"},
			{Name: "timestamp_changed", Secret: vectorSecret, Now: vectorTime, Body: vectorBody, Headers: headers("1800000001", good), Reason: "bad_signature"},
			{Name: "missing_prefix", Secret: vectorSecret, Now: vectorTime, Body: vectorBody, Headers: headers("1800000000", good[len("sha256="):]), Reason: "bad_signature"},
			{Name: "uppercase_hex", Secret: vectorSecret, Now: vectorTime, Body: vectorBody, Headers: headers("1800000000", "sha256="+upper(good[len("sha256="):])), Reason: "bad_signature"},
			{Name: "missing_signature", Secret: vectorSecret, Now: vectorTime, Body: vectorBody, Headers: without("X-Webhook-Signature"), Reason: "missing_header"},
			{Name: "missing_timestamp", Secret: vectorSecret, Now: vectorTime, Body: vectorBody, Headers: without("X-Webhook-Timestamp"), Reason: "missing_header"},
			{Name: "timestamp_not_a_number", Secret: vectorSecret, Now: vectorTime, Body: vectorBody, Headers: headers("1800000000.0", good), Reason: "stale"},
		},
	}
}

func upper(s string) string { return string(bytes.ToUpper([]byte(s))) }

func TestVectors(t *testing.T) {
	want, err := json.MarshalIndent(buildVectors(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	if *update {
		if err := os.WriteFile(vectorsFile, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(vectorsFile)
	if err != nil {
		t.Fatalf("%v; run with -update", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is out of date with Sign; run with -update and review the diff", vectorsFile)
	}
}

// TestVectorsAgreeWithSign re-derives each valid case's signature from its
// own fields, so a case can't be valid by accident.
func TestVectorsAgreeWithSign(t *testing.T) {
	for _, c := range buildVectors().Cases {
		ts := c.Headers["X-Webhook-Timestamp"]
		sig, ok := c.Headers["X-Webhook-Signature"]
		n, scanErr := strconv.ParseInt(ts, 10, 64)
		match := ok && scanErr == nil && Sign([]byte(c.Secret), n, []byte(c.Body)) == sig
		fresh := scanErr == nil && max(c.Now-n, n-c.Now) <= 300
		if c.Valid != (match && fresh) {
			t.Errorf("%s: valid=%v, but signature matches=%v and fresh=%v", c.Name, c.Valid, match, fresh)
		}
	}
}
