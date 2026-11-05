package tokens

import (
	"encoding/asn1"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func keys(t *testing.T) (privPEM, pubPEM string) {
	t.Helper()
	privPEM, pubPEM, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return privPEM, pubPEM
}

func TestSignParseVerifyRoundTrip(t *testing.T) {
	privPEM, pubPEM := keys(t)
	priv, _ := ParsePrivateKey(privPEM)
	pub, _ := ParsePublicKey(pubPEM)
	tok, err := Sign(priv, "kid_1", Claims{Sub: "pid_1", Aud: "v", Iat: 100, Exp: 200, Vid: "u_7"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := Parse(tok)
	if err != nil || p.Kid != "kid_1" || p.Claims != (Claims{Sub: "pid_1", Aud: "v", Iat: 100, Exp: 200, Vid: "u_7"}) {
		t.Fatalf("Parse = %+v, %v", p, err)
	}
	if err := p.Verify(pub); err != nil {
		t.Fatal(err)
	}
	_, otherPub := keys(t)
	other, _ := ParsePublicKey(otherPub)
	if p.Verify(other) == nil {
		t.Fatal("verified with the wrong key")
	}
	// Changing any claim breaks the signature.
	parts := strings.Split(tok, ".")
	parts[1] = b64.EncodeToString([]byte(`{"sub":"pid_2","aud":"v","iat":100,"exp":200}`))
	if p2, err := Parse(strings.Join(parts, ".")); err != nil || p2.Verify(pub) == nil {
		t.Fatal("a token with a swapped sub verified")
	}
}

func TestParseAcceptsOnlyES256WithAKid(t *testing.T) {
	body := b64.EncodeToString([]byte(`{"sub":"p","aud":"v","iat":1,"exp":2}`))
	sig := b64.EncodeToString(make([]byte, 64))
	for name, h := range map[string]string{
		"none":   `{"alg":"none","kid":"k"}`,
		"HS256":  `{"alg":"HS256","kid":"k"}`,
		"no kid": `{"alg":"ES256"}`,
	} {
		if _, err := Parse(b64.EncodeToString([]byte(h)) + "." + body + "." + sig); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for _, bad := range []string{"", "a.b", "a.b.c.d", "!!.!!.!!"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
}

// Interop with an independent implementation: a token signed by openssl
// verifies here, and a token signed here verifies with openssl.
func TestInteropWithOpenSSL(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl not found")
	}
	dir := t.TempDir()
	privPEM, pubPEM := keys(t)
	privFile, pubFile := filepath.Join(dir, "k.pem"), filepath.Join(dir, "p.pem")
	_ = os.WriteFile(privFile, []byte(privPEM), 0o600)
	_ = os.WriteFile(pubFile, []byte(pubPEM), 0o600)
	pub, _ := ParsePublicKey(pubPEM)
	priv, _ := ParsePrivateKey(privPEM)

	type ecdsaSig struct{ R, S *big.Int }

	// openssl signs, Go verifies.
	input := b64.EncodeToString([]byte(`{"alg":"ES256","kid":"kid_x","typ":"JWT"}`)) + "." + b64.EncodeToString([]byte(`{"sub":"pid_1","aud":"v","iat":1,"exp":2}`))
	inFile := filepath.Join(dir, "in")
	_ = os.WriteFile(inFile, []byte(input), 0o600)
	der, err := exec.Command("openssl", "dgst", "-sha256", "-sign", privFile, inFile).Output()
	if err != nil {
		t.Fatal(err)
	}
	var s ecdsaSig
	if _, err := asn1.Unmarshal(der, &s); err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, 64)
	s.R.FillBytes(raw[:32])
	s.S.FillBytes(raw[32:])
	tok, err := Parse(input + "." + b64.EncodeToString(raw))
	if err != nil || tok.Verify(pub) != nil {
		t.Fatalf("an openssl-signed token did not verify: %v", err)
	}

	// Go signs, openssl verifies.
	signed, _ := Sign(priv, "kid_y", Claims{Sub: "pid_2", Aud: "t", Iat: 1, Exp: 2})
	parts := strings.Split(signed, ".")
	_ = os.WriteFile(inFile, []byte(parts[0]+"."+parts[1]), 0o600)
	sig, _ := b64.DecodeString(parts[2])
	der, _ = asn1.Marshal(ecdsaSig{new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])})
	sigFile := filepath.Join(dir, "sig")
	_ = os.WriteFile(sigFile, der, 0o600)
	if out, err := exec.Command("openssl", "dgst", "-sha256", "-verify", pubFile, "-signature", sigFile, inFile).CombinedOutput(); err != nil {
		t.Fatalf("openssl rejected a Go-signed token: %v %s", err, out)
	}
}

// A 64-byte signature is 86 base64url characters; the last carries 4 unused
// bits. A lenient decoder would accept several spellings of one token.
func TestNonCanonicalEncodingsAreRejected(t *testing.T) {
	privPEM, pubPEM := keys(t)
	priv, _ := ParsePrivateKey(privPEM)
	pub, _ := ParsePublicKey(pubPEM)
	tok, _ := Sign(priv, "kid_1", Claims{Sub: "p", Aud: "v", Iat: 1, Exp: 2})
	last := tok[len(tok)-1]
	accepted := 0
	for _, c := range "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_" {
		if byte(c) == last {
			continue
		}
		if p, err := Parse(tok[:len(tok)-1] + string(c)); err == nil && p.Verify(pub) == nil {
			accepted++
		}
	}
	if accepted != 0 {
		t.Fatalf("%d other spellings of the same token verified", accepted)
	}
}
