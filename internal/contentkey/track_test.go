package contentkey

import (
	"encoding/hex"
	"errors"
	"testing"
)

// Vectors computed independently with Python's hmac, hashlib and the
// cryptography package's AESGCM, as for the content keys (D012), so the
// Cloudflare Worker's WebCrypto can check against the same bytes.
const (
	trackPlain  = "WEBVTT\nX-TIMESTAMP-MAP=MPEGTS:0,LOCAL:00:00:00.000\n\n1\n00:00:01.000 --> 00:00:02.000\nHello\n\n"
	trackSealed = "01000000036465666768696a6b6c6d6e6ffc3396f89df76fc6009f064e575b435dcd35927edf7f661d60e3a940eb8a35bfc1e30031f763276922f78dcb9b041800bebd0fd1f8ecea579590a4deed85954e9e08200eee07be41bf490f4a2a48f21c3e66a7220f71626cc503bd51344e9f7c28cf7b5ceeb5abff9a4c25"
	trackObject = "media/ast_1/tracks/trk_1.vtt"
)

func vectorSecret(version int32) Secret {
	s := Secret{Version: version, key: make([]byte, 32)}
	for i := range s.key {
		s.key[i] = byte(i)
	}
	return s
}

func TestTrackKeyAndSealVectors(t *testing.T) {
	s := vectorSecret(3)
	key := s.DeriveTrack("ast_1", "trk_1")
	if got := hex.EncodeToString(key[:]); got != "e7911761b5f5bdd5c1f69d844436da05" {
		t.Fatalf("DeriveTrack = %s", got)
	}
	if s.DeriveTrack("ast_1", "trk_2") == key || s.Derive("ast_1", "trk_1") == key {
		t.Fatal("each track gets its own key, apart from output keys")
	}
	var nonce [12]byte
	for i := range nonce {
		nonce[i] = byte(100 + i)
	}
	sealed, err := sealTrack(s, "ast_1", "trk_1", trackObject, []byte(trackPlain), nonce)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(sealed); got != trackSealed {
		t.Fatalf("sealed = %s", got)
	}
	plain, err := NewRing(s).OpenTrack("ast_1", "trk_1", trackObject, sealed)
	if err != nil || string(plain) != trackPlain {
		t.Fatalf("open = %q, %v", plain, err)
	}
}

func TestATrackFileOpensOnlyWhereItWasSealed(t *testing.T) {
	ring := NewRing(vectorSecret(3))
	sealed, _ := hex.DecodeString(trackSealed)
	for name, open := range map[string]func() error{
		"another key": func() error {
			_, err := ring.OpenTrack("ast_1", "trk_1", "media/ast_1/tracks/trk_2.vtt", sealed)
			return err
		},
		"another track": func() error { _, err := ring.OpenTrack("ast_1", "trk_2", trackObject, sealed); return err },
		"a flipped bit": func() error {
			b := append([]byte{}, sealed...)
			b[40] ^= 1
			_, err := ring.OpenTrack("ast_1", "trk_1", trackObject, b)
			return err
		},
		"an unknown version": func() error {
			_, err := NewRing(vectorSecret(4)).OpenTrack("ast_1", "trk_1", trackObject, sealed)
			return err
		},
		"plaintext": func() error { _, err := ring.OpenTrack("ast_1", "trk_1", trackObject, []byte(trackPlain)); return err },
	} {
		if err := open(); !errors.Is(err, ErrTrackSealed) {
			t.Errorf("%s: %v, want ErrTrackSealed", name, err)
		}
	}
	// A fresh seal uses a random nonce, and still opens.
	a, _ := SealTrack(vectorSecret(3), "ast_1", "trk_1", trackObject, []byte("x"))
	b, _ := SealTrack(vectorSecret(3), "ast_1", "trk_1", trackObject, []byte("x"))
	if hex.EncodeToString(a) == hex.EncodeToString(b) {
		t.Fatal("two seals of the same file must differ")
	}
	if v, err := TrackVersion(a); err != nil || v != 3 {
		t.Fatalf("version %d, %v", v, err)
	}
}
