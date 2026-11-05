// Package publicnet is an HTTP client that reaches only public unicast
// addresses, for fetching URLs that untrusted callers supply (import from
// URL; docs/architecture.md, "Threats and controls": SSRF). The address is
// checked when the socket connects, after DNS, on every connection and
// every redirect, so neither a redirect nor DNS rebinding can reach the
// host's loopback, a private network or cloud metadata.
package publicnet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"syscall"
	"time"
)

// ErrRefused means the URL or an address it led to isn't allowed.
var ErrRefused = errors.New("publicnet: address not allowed")

// blocked are ranges that are not public unicast but that netip's predicates
// don't cover.
var blocked = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),   // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // documentation
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, and broadcast
	netip.MustParsePrefix("64:ff9b::/96"),    // NAT64: maps onto IPv4, private included
	netip.MustParsePrefix("64:ff9b:1::/48"),  // local-use NAT64
	netip.MustParsePrefix("100::/64"),        // discard
	netip.MustParsePrefix("2001::/23"),       // IETF protocol assignments (Teredo, ORCHID…)
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("2002::/16"),       // 6to4: maps onto IPv4
	netip.MustParsePrefix("fec0::/10"),       // site-local (deprecated)
}

// Public reports whether a is a public unicast address. IPv4-mapped IPv6
// addresses are judged as the IPv4 address they carry.
func Public(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsUnspecified() || a.IsMulticast() ||
		a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast() {
		return false
	}
	for _, p := range blocked {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// CheckURL accepts http and https URLs with a host and no credentials, and
// refuses a literal address that isn't public. Hostnames are checked when
// they resolve.
func CheckURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	switch {
	case err != nil || len(raw) > 2048:
		return nil, fmt.Errorf("%w: not a URL", ErrRefused)
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, fmt.Errorf("%w: only http and https", ErrRefused)
	case u.User != nil:
		return nil, fmt.Errorf("%w: credentials in the URL", ErrRefused)
	case u.Hostname() == "":
		return nil, fmt.Errorf("%w: no host", ErrRefused)
	}
	if a, err := netip.ParseAddr(u.Hostname()); err == nil && !Public(a) {
		return nil, fmt.Errorf("%w: %s is not a public address", ErrRefused, a)
	}
	return u, nil
}

// Options adjust the client; the zero value is the production policy.
type Options struct {
	// Allow decides which addresses may be dialled (default Public). Tests
	// widen it to reach a local server.
	Allow func(netip.Addr) bool
	// MaxRedirects bounds redirects (default 5).
	MaxRedirects int
}

// Client returns the client. It uses no proxy, so the address checked is
// the address connected to.
func Client(opts Options) *http.Client {
	allow := opts.Allow
	if allow == nil {
		allow = Public
	}
	maxRedirects := opts.MaxRedirects
	if maxRedirects == 0 {
		maxRedirects = 5
	}
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return ErrRefused
			}
			a, err := netip.ParseAddr(host)
			if err != nil || !allow(a) {
				return fmt.Errorf("%w: %s", ErrRefused, host)
			}
			return nil
		},
	}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("%w: more than %d redirects", ErrRefused, maxRedirects)
			}
			_, err := CheckURL(req.URL.String())
			return err
		},
	}
}

// Get fetches u with ctx, refusing what CheckURL refuses.
func Get(ctx context.Context, c *http.Client, raw string) (*http.Response, error) {
	u, err := CheckURL(raw)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "ClipMux-Import/1.0")
	return c.Do(req)
}
