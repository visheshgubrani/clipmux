package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestPushRefusesAURLWithLocal(t *testing.T) {
	var out, errOut bytes.Buffer
	env := map[string]string{}
	code := runPush([]string{"--local", "https://files.example.com/leds.mp4"}, func(k string) string { return env[k] }, &out, &errOut)
	if code != 2 || !strings.Contains(errOut.String(), "--local takes files on this PC") {
		t.Fatalf("exit %d, stderr %q; want 2 and the --local refusal", code, errOut.String())
	}
}

func TestPushRefusesAnUnknownOrRepeatedPolicy(t *testing.T) {
	env := map[string]string{"CLIPMUX_SERVER": "https://clipmux.example.com", "CLIPMUX_API_KEY": "cmx_test"}
	for _, p := range []string{"private", "public,public"} {
		var out, errOut bytes.Buffer
		code := runPush([]string{"--policy", p, "https://files.example.com/leds.mp4"}, func(k string) string { return env[k] }, &out, &errOut)
		if code != 2 || !strings.Contains(errOut.String(), "--policy is signed, public, or signed,public") {
			t.Errorf("--policy %s: exit %d, stderr %q", p, code, errOut.String())
		}
	}
}
