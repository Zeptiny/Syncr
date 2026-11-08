package netguard_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/platform/netguard"
)

func TestCheck(t *testing.T) {
	t.Parallel()
	// want is the verdict of private, public and any: true = allowed.
	tests := []struct {
		addr                 string
		private, public, any bool
	}{
		// Loopback and unspecified, in every form.
		{"127.0.0.1", false, false, true},
		{"127.255.0.9", false, false, true},
		{"::1", false, false, true},
		{"::ffff:127.0.0.1", false, false, true},
		{"::127.0.0.1", false, false, true},     // IPv4-compatible
		{"64:ff9b::7f00:1", false, false, true}, // NAT64 of 127.0.0.1
		{"0.0.0.0", false, false, true},
		{"0.1.2.3", false, false, true},
		{"::", false, false, true},
		{"::ffff:0.0.0.0", false, false, true},
		// Link-local and cloud metadata.
		{"169.254.169.254", false, false, true},
		{"::ffff:169.254.169.254", false, false, true},
		{"64:ff9b::a9fe:a9fe", false, false, true},
		{"169.254.1.1", false, false, true},
		{"fe80::1", false, false, true},
		{"fe80::1%eth0", false, false, true},
		{"fd00:ec2::254", false, false, true},
		{"100.100.100.200", false, false, true},
		// Multicast and broadcast.
		{"224.0.0.1", false, false, true},
		{"239.255.255.250", false, false, true},
		{"ff02::1", false, false, true},
		{"255.255.255.255", false, false, true},
		// Private networks: allowed by default, denied by public.
		{"10.0.0.5", true, false, true},
		{"172.16.3.4", true, false, true},
		{"192.168.1.10", true, false, true},
		{"::ffff:192.168.1.10", true, false, true},
		{"fd12:3456::1", true, false, true},
		{"100.64.0.1", true, false, true},
		{"192.0.2.1", true, false, true},
		{"2001:db8::1", true, false, true},
		{"240.0.0.1", true, false, true},
		// Public addresses.
		{"8.8.8.8", true, true, true},
		{"::ffff:8.8.8.8", true, true, true},
		{"64:ff9b::808:808", true, true, true},
		{"2606:4700:4700::1111", true, true, true},
		{"162.159.135.232", true, true, true},
	}
	policies := map[string]netguard.Policy{
		"private": {}, "public": {Mode: netguard.ModePublic}, "any": {Mode: netguard.ModeAny},
	}
	for _, tt := range tests {
		ip := netip.MustParseAddr(tt.addr)
		for name, want := range map[string]bool{"private": tt.private, "public": tt.public, "any": tt.any} {
			err := policies[name].Check(ip)
			if (err == nil) != want {
				t.Errorf("%s.Check(%s) = %v, want allowed=%v", name, tt.addr, err, want)
			}
			if err != nil && !errors.Is(err, netguard.ErrDenied) {
				t.Errorf("%s.Check(%s) = %v, does not match ErrDenied", name, tt.addr, err)
			}
		}
	}
	if err := (netguard.Policy{}).Check(netip.Addr{}); !errors.Is(err, netguard.ErrDenied) {
		t.Errorf("Check(invalid) = %v", err)
	}
	var derr *netguard.DeniedError
	if err := (netguard.Policy{}).Check(netip.MustParseAddr("169.254.169.254")); !errors.As(err, &derr) || derr.Reason != "link-local" ||
		!strings.Contains(err.Error(), "169.254.169.254") {
		t.Errorf("Check(metadata) = %v", err)
	}
}

func TestAllow(t *testing.T) {
	t.Parallel()
	allow, err := netguard.ParsePrefixes([]string{"127.0.0.1", " 192.168.10.0/24 ", "", "::ffff:10.1.0.0/112", "fd00::/8"})
	if err != nil {
		t.Fatal(err)
	}
	p := netguard.Policy{Mode: netguard.ModePublic, Allow: allow}
	for addr, want := range map[string]bool{
		"127.0.0.1": true, "::ffff:127.0.0.1": true, "127.0.0.2": false,
		"192.168.10.77": true, "192.168.11.1": false,
		"10.1.2.3": true, "10.2.0.1": false,
		"fd00:ec2::254": true, // explicitly allowed
		"8.8.8.8":       true,
	} {
		if err := p.Check(netip.MustParseAddr(addr)); (err == nil) != want {
			t.Errorf("Check(%s) = %v, want allowed=%v", addr, err, want)
		}
	}
	for _, bad := range [][]string{{"10.0.0.0/33"}, {"not-an-ip"}, {"::ffff:10.0.0.0/64"}} {
		if _, err := netguard.ParsePrefixes(bad); err == nil {
			t.Errorf("ParsePrefixes(%q) succeeded", bad)
		}
	}
}

func TestParseMode(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]netguard.Mode{"": netguard.ModePrivate, "private": netguard.ModePrivate, "PUBLIC": netguard.ModePublic, "any": netguard.ModeAny} {
		if got, err := netguard.ParseMode(in); err != nil || got != want {
			t.Errorf("ParseMode(%q) = (%q, %v)", in, got, err)
		}
	}
	if _, err := netguard.ParseMode("lan"); err == nil {
		t.Error("ParseMode(lan) succeeded")
	}
}

// listen starts a TCP listener on loopback that accepts and closes.
func listen(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return ln.Addr().String()
}

// TestDialChecksTheResolvedAddress: a name is checked by what it resolves
// to when the connection is made ("localhost" stands in for a name
// rebound to loopback), and IP literals the same way.
func TestDialChecksTheResolvedAddress(t *testing.T) {
	t.Parallel()
	addr := listen(t)
	_, port, _ := net.SplitHostPort(addr)
	ctx := context.Background()
	dial := netguard.Policy{}.Dialer(5 * time.Second)
	for _, target := range []string{"localhost:" + port, addr, "[::ffff:127.0.0.1]:" + port} {
		conn, err := dial(ctx, "tcp", target)
		if err == nil {
			_ = conn.Close()
			t.Errorf("dial %s succeeded", target)
			continue
		}
		if !errors.Is(err, netguard.ErrDenied) {
			t.Errorf("dial %s = %v, want ErrDenied", target, err)
		}
	}
	if _, err := dial(ctx, "udp", addr); err == nil || errors.Is(err, netguard.ErrDenied) {
		t.Errorf("dial udp = %v, want a network error", err)
	}

	// Allowed explicitly, or in mode any, the same dial works.
	allow, _ := netguard.ParsePrefixes([]string{"127.0.0.0/8"})
	for _, p := range []netguard.Policy{{Allow: allow}, {Mode: netguard.ModeAny}} {
		conn, err := p.Dialer(5*time.Second)(ctx, "tcp", addr)
		if err != nil {
			t.Errorf("%+v: dial = %v", p, err)
			continue
		}
		_ = conn.Close()
	}
}

func TestHTTPClient(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	t.Cleanup(target.Close)
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/inner", http.StatusFound)
	}))
	t.Cleanup(redirect.Close)

	// The default policy refuses the loopback test servers.
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, target.URL, nil)
	resp, err := netguard.Policy{}.HTTPClient(5 * time.Second).Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("GET loopback succeeded")
	}
	if !errors.Is(err, netguard.ErrDenied) {
		t.Errorf("GET loopback = %v, want ErrDenied", err)
	}

	// Redirects are returned, never followed.
	c := netguard.Policy{Mode: netguard.ModeAny}.HTTPClient(5 * time.Second)
	req, _ = http.NewRequestWithContext(context.Background(), http.MethodPost, redirect.URL, strings.NewReader("{}"))
	resp, err = c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound || hits.Load() != 0 {
		t.Errorf("status %d, %d hits on the redirect target", resp.StatusCode, hits.Load())
	}
	if tr, ok := c.Transport.(*http.Transport); !ok || tr.Proxy != nil {
		t.Error("the client uses a proxy")
	}
}
