package registry_test

import (
	"context"
	"errors"
	"testing"

	"github.com/visheshgubrani/clipmux/internal/registry"
	"github.com/visheshgubrani/clipmux/internal/registry/registrytest"
)

func TestParseReadsReferencesTheWayDockerDoes(t *testing.T) {
	digest := "sha256:" + "ab0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd"[:64]
	cases := map[string]registry.Ref{
		"ghcr.io/visheshgubrani/clipmux:sha-abc":   {Host: "ghcr.io", Repo: "visheshgubrani/clipmux", Tag: "sha-abc"},
		"ghcr.io/visheshgubrani/clipmux@" + digest: {Host: "ghcr.io", Repo: "visheshgubrani/clipmux", Digest: digest},
		"ubuntu":             {Host: "registry-1.docker.io", Repo: "library/ubuntu", Tag: "latest"},
		"someone/app:1.2":    {Host: "registry-1.docker.io", Repo: "someone/app", Tag: "1.2"},
		"localhost:5000/app": {Host: "localhost:5000", Repo: "app", Tag: "latest"},
	}
	for ref, want := range cases {
		got, err := registry.Parse(ref)
		if err != nil || got != want {
			t.Errorf("Parse(%s) = %+v, %v; want %+v", ref, got, err, want)
		}
	}
	if _, err := registry.Parse("ghcr.io/x/y@sha256:short"); err == nil {
		t.Error("Parse accepted a malformed digest")
	}
	if got := (registry.Ref{Host: "registry-1.docker.io", Repo: "library/ubuntu", Digest: digest}).Pinned(); got != "docker.io/library/ubuntu@"+digest {
		t.Errorf("Pinned = %s", got)
	}
}

// A private image resolves to its digest through the Bearer token flow,
// only with the right credentials.
func TestResolvePinsAPrivateImageWithItsCredentials(t *testing.T) {
	reg := registrytest.New(t)
	r := &registry.Resolver{HTTP: reg.Client}
	ctx := context.Background()

	got, err := r.Resolve(ctx, reg.Image(), registrytest.Username, registrytest.Password)
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest != reg.Digest || got.Pinned() != reg.Pinned() {
		t.Fatalf("Resolve = %+v, want %s", got, reg.Pinned())
	}
	// A pinned reference is checked again, not trusted.
	if again, err := r.Resolve(ctx, reg.Pinned(), registrytest.Username, registrytest.Password); err != nil || again.Pinned() != reg.Pinned() {
		t.Fatalf("Resolve(pinned) = %+v, %v", again, err)
	}
	// Each refusal says which it is, so Connect can tell the owner what to fix.
	if _, err := r.Resolve(ctx, reg.Image(), "", ""); !errors.Is(err, registry.ErrDenied) {
		t.Errorf("anonymously = %v, want ErrDenied", err)
	}
	if _, err := r.Resolve(ctx, reg.Image(), registrytest.Username, "ghp_WRONG"); !errors.Is(err, registry.ErrBadCredentials) {
		t.Errorf("a wrong token = %v, want ErrBadCredentials", err)
	}
	if _, err := r.Resolve(ctx, reg.Host+"/"+registrytest.Repo+":no-such-tag", registrytest.Username, registrytest.Password); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("an unknown tag = %v, want ErrNotFound", err)
	}
	if _, err := r.Resolve(ctx, reg.Host+"/"+registrytest.Repo+":no-such-tag", "", ""); !errors.Is(err, registry.ErrDenied) {
		t.Errorf("an unknown tag, anonymously = %v, want ErrDenied (it may be private)", err)
	}
}

// A public image (P6-T07) pins with no credentials at all.
func TestResolvePinsAPublicImageAnonymously(t *testing.T) {
	reg := registrytest.NewPublic(t)
	r := &registry.Resolver{HTTP: reg.Client}
	got, err := r.Resolve(context.Background(), reg.Image(), "", "")
	if err != nil || got.Pinned() != reg.Pinned() {
		t.Fatalf("Resolve = %+v, %v; want %s", got, err, reg.Pinned())
	}
}
