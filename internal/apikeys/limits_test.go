package apikeys

import (
	"net/http/httptest"
	"testing"
)

func TestWritesHaveTheirOwnBudgetPerKey(t *testing.T) {
	l := NewLimits()
	a, b := Key{ID: "key_a", AppID: "app_1"}, Key{ID: "key_b", AppID: "app_1"}
	for i := 1; i <= WriteBurst; i++ {
		if !l.take(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/uploads", nil), a) {
			t.Fatalf("write %d of the burst refused", i)
		}
	}
	rec := httptest.NewRecorder()
	if l.take(rec, httptest.NewRequest("POST", "/v1/uploads", nil), a) || rec.Code != 429 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("write %d: %d, want 429 with Retry-After", WriteBurst+1, rec.Code)
	}
	if !l.take(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/uploads", nil), b) {
		t.Fatal("another key's writes were refused")
	}
	if !l.take(httptest.NewRecorder(), httptest.NewRequest("GET", "/v1/assets/ast_1", nil), a) {
		t.Fatal("a key out of writes must still read")
	}
	// The owner's session acting as an app has a budget of its own.
	if !l.take(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/uploads", nil), Key{AppID: "app_1"}) {
		t.Fatal("the session shares a key's budget")
	}
}

func TestEventPollsAreCappedPerKey(t *testing.T) {
	l := NewLimits()
	k := Key{ID: "key_a"}
	var releases []func()
	for i := 1; i <= EventPolls; i++ {
		release, ok := l.EventPoll(httptest.NewRecorder(), k)
		if !ok {
			t.Fatalf("poll %d refused", i)
		}
		releases = append(releases, release)
	}
	rec := httptest.NewRecorder()
	if _, ok := l.EventPoll(rec, k); ok || rec.Code != 429 {
		t.Fatalf("poll %d: %d, want 429", EventPolls+1, rec.Code)
	}
	releases[0]()
	if _, ok := l.EventPoll(httptest.NewRecorder(), k); !ok {
		t.Fatal("a finished poll didn't free its slot")
	}
	var none *Limits
	if !none.take(httptest.NewRecorder(), httptest.NewRequest("POST", "/", nil), k) {
		t.Fatal("no limits must limit nothing")
	}
}
