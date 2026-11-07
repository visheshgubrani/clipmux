package media

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite models.go from toolchain/versions.env")

// versionsEnv reads the shell assignments in toolchain/versions.env.
func versionsEnv(t *testing.T) map[string]string {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "toolchain", "versions.env"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	vars := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if k, v, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(line, "#") {
			vars[k] = os.Expand(v, func(name string) string { return vars[name] })
		}
	}
	return vars
}

func modelFromEnv(t *testing.T, vars map[string]string, prefix string) Model {
	t.Helper()
	size, err := strconv.ParseInt(vars[prefix+"_SIZE"], 10, 64)
	if err != nil {
		t.Fatalf("%s_SIZE: %v", prefix, err)
	}
	return Model{Name: vars[prefix+"_NAME"], URL: vars[prefix+"_URL"], SHA256: vars[prefix+"_SHA256"], Size: size}
}

// The model pins in Go are a checked copy of versions.env, the one place a
// digest is written (AGENTS.md).
func TestSpeechModelsMatchVersionsEnv(t *testing.T) {
	vars := versionsEnv(t)
	speech, vad := modelFromEnv(t, vars, "WHISPER_MODEL"), modelFromEnv(t, vars, "WHISPER_VAD_MODEL")
	if *update {
		var b bytes.Buffer
		head, _, _ := strings.Cut(mustRead(t, "models.go"), "// SpeechModel is")
		b.WriteString(head)
		for _, m := range []struct {
			name, doc string
			m         Model
		}{{"SpeechModel", "the pinned whisper model", speech}, {"VADModel", "the pinned Silero VAD model", vad}} {
			fmt.Fprintf(&b, "// %s is %s.\nvar %s = Model{\n\tName:   %q,\n\tURL:    %q,\n\tSHA256: %q,\n\tSize:   %d,\n}\n\n",
				m.name, m.doc, m.name, m.m.Name, m.m.URL, m.m.SHA256, m.m.Size)
		}
		if err := os.WriteFile("models.go", bytes.TrimSuffix(b.Bytes(), []byte("\n")), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if SpeechModel != speech || VADModel != vad {
		t.Fatalf("models.go differs from toolchain/versions.env; run go test ./internal/media -run TestSpeechModelsMatchVersionsEnv -update\ngo:  %+v %+v\nenv: %+v %+v",
			SpeechModel, VADModel, speech, vad)
	}
	if SpeechModel.FileName() != "ggml-large-v3-turbo-q8_0.bin" || !strings.HasPrefix(SpeechModel.ID(), "large-v3-turbo-q8_0@sha256:317eb69c") {
		t.Fatalf("file %s, id %s", SpeechModel.FileName(), SpeechModel.ID())
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseMediaPlaylist(t *testing.T) {
	p, err := ParseMediaPlaylist(`#EXTM3U
#EXT-X-VERSION:6
#EXT-X-TARGETDURATION:6
#EXT-X-PLAYLIST-TYPE:VOD
#EXT-X-MAP:URI="init.mp4"
#EXT-X-KEY:METHOD=AES-128,URI="../key/1",IV=0x00112233445566778899aabbccddeeff,KEYFORMAT="identity"
#EXTINF:6.006,
seg_00001.m4s
#EXTINF:1.5,
seg_00002.m4s
#EXT-X-ENDLIST
`)
	if err != nil {
		t.Fatal(err)
	}
	if p.Init != "init.mp4" || p.KeyURI != "../key/1" || len(p.Segments) != 2 {
		t.Fatalf("playlist %+v", p)
	}
	if s := p.Segments[1]; s.URI != "seg_00002.m4s" || s.Duration != 1.5 || s.IV[0] != 0x00 || s.IV[15] != 0xff {
		t.Fatalf("segment %+v", s)
	}
	// Without an IV attribute, the IV is the media sequence number.
	p, err = ParseMediaPlaylist("#EXT-X-MEDIA-SEQUENCE:7\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXT-X-KEY:METHOD=AES-128,URI=\"k\"\n#EXTINF:6,\na.m4s\n#EXTINF:6,\nb.m4s\n")
	if err != nil || p.Segments[0].IV[15] != 7 || p.Segments[1].IV[15] != 8 || p.Segments[1].IV[0] != 0 {
		t.Fatalf("sequence IVs %+v, %v", p.Segments, err)
	}
	for _, bad := range []string{"#EXT-X-MAP:URI=\"i\"\n#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"k\"\n#EXTINF:6,\na.m4s\n", "#EXTINF:6,\na.m4s\n", "#EXT-X-MAP:URI=\"i\"\n#EXT-X-KEY:METHOD=AES-128,URI=\"k\"\na.m4s\n"} {
		if _, err := ParseMediaPlaylist(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestDecryptSegmentStripsPadding(t *testing.T) {
	key, iv := [16]byte{1, 2, 3}, [16]byte{9}
	plain := []byte("an fMP4 fragment of 29 bytes!")
	padded := append(append([]byte{}, plain...), bytes.Repeat([]byte{3}, 3)...)
	block, _ := aes.NewCipher(key[:])
	seg := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv[:]).CryptBlocks(seg, padded)
	got, err := DecryptSegment(key, iv, seg)
	if err != nil || string(got) != string(plain) {
		t.Fatalf("decrypt = %q, %v", got, err)
	}
	if _, err := DecryptSegment([16]byte{7}, iv, seg); err == nil {
		t.Fatal("a wrong key must fail on the padding")
	}
	if _, err := DecryptSegment(key, iv, seg[:20]); err == nil {
		t.Fatal("a partial block must fail")
	}
}

func TestProbeSpeechOnTheCPU(t *testing.T) {
	tools, sp := speechTools(t)
	if err := ProbeSpeech(t.Context(), tools, t.TempDir(), sp); err != nil {
		t.Fatal(err)
	}
	broken := sp
	broken.Model = filepath.Join(t.TempDir(), "missing.bin")
	if err := ProbeSpeech(t.Context(), tools, t.TempDir(), broken); err == nil {
		t.Fatal("a probe with no model must fail")
	}
	_ = time.Second
}
