package publicnet

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"testing"
)

func TestOnlyPublicUnicastAddressesAreAllowed(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8": true, "1.1.1.1": true, "2606:4700:4700::1111": true,
		"127.0.0.1": false, "10.1.2.3": false, "172.16.0.1": false, "192.168.1.1": false,
		"169.254.169.254": false, // cloud metadata
		"100.64.0.1":      false, "0.0.0.0": false, "0.1.2.3": false, "255.255.255.255": false, "240.0.0.1": false,
		"224.0.0.1": false, "192.0.2.1": false, "198.18.0.1": false,
		"::1": false, "::": false, "fe80::1": false, "fc00::1": false, "fd12::1": false, "ff02::1": false,
		"::ffff:127.0.0.1": false, "::ffff:10.0.0.1": false, "::ffff:8.8.8.8": true, // IPv4-mapped
		"64:ff9b::a00:1": false, "2002:a00:1::": false, "2001:db8::1": false, "fec0::1": false,
	} {
		if got := Public(netip.MustParseAddr(addr)); got != want {
			t.Errorf("Public(%s) = %v, want %v", addr, got, want)
		}
	}
}

func TestURLsAreCheckedBeforeAnyRequest(t *testing.T) {
	for _, u := range []string{
		"file:///etc/passwd", "ftp://example.com/x", "gopher://x", "http://user:pass@example.com/x",
		"http://127.0.0.1/x", "http://[::1]/x", "http://169.254.169.254/latest/meta-data", "http:///x", "not a url\x7f",
	} {
		if _, err := CheckURL(u); !errors.Is(err, ErrRefused) {
			t.Errorf("CheckURL(%q) = %v, want ErrRefused", u, err)
		}
	}
	if _, err := CheckURL("https://cdn.example.com/video.mp4?sig=1"); err != nil {
		t.Errorf("a public https URL was refused: %v", err)
	}
}

// A hostname that resolves to loopback ("localhost", or a rebinding name)
// is refused when the socket connects, whatever DNS said earlier.
func TestTheDialerRefusesPrivateAddressesAfterDNS(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("secret")) }))
	defer srv.Close()
	u := "http://localhost:" + strconv.Itoa(int(netip.MustParseAddrPort(srv.Listener.Addr().String()).Port())) + "/"
	if _, err := Get(context.Background(), Client(Options{}), u); !errors.Is(err, ErrRefused) {
		t.Fatalf("GET %s = %v, want ErrRefused", u, err)
	}
}

// A public-looking URL that redirects inward is refused at the redirect.
func TestARedirectToAPrivateAddressIsRefused(t *testing.T) {
	inner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("secret")) }))
	defer inner.Close()
	outer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, inner.URL+"/", http.StatusFound)
	}))
	defer outer.Close()
	// The dialer lets the outer server through, as if it were public; the
	// redirect's target is still checked.
	c := Client(Options{Allow: func(netip.Addr) bool { return true }})
	res, err := Get(context.Background(), c, outer.URL+"/")
	if err == nil {
		res.Body.Close()
	}
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("redirect to %s = %v, want ErrRefused", inner.URL, err)
	}
}
