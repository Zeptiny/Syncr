package clientip

import (
	"net/http"
	"net/netip"
	"testing"
)

func TestClientIP(t *testing.T) {
	t.Parallel()
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("fd00::/8")}
	xff := New("X-Forwarded-For", trusted)
	tests := []struct {
		name   string
		r      *Resolver
		remote string
		header []string
		want   string
	}{
		{"no header configured", New("", nil), "192.0.2.1:1234", []string{"198.51.100.1"}, "192.0.2.1"},
		{"nil resolver", nil, "192.0.2.1:1234", nil, "192.0.2.1"},
		{"untrusted peer ignores header", xff, "192.0.2.1:1234", []string{"198.51.100.1"}, "192.0.2.1"},
		{"trusted peer, single entry", xff, "10.0.0.2:1234", []string{"198.51.100.1"}, "198.51.100.1"},
		{"forged left entry is skipped", xff, "10.0.0.2:1234", []string{"6.6.6.6, 198.51.100.1"}, "198.51.100.1"},
		{"chain of proxies", xff, "10.0.0.2:1234", []string{"198.51.100.1, 10.0.0.9"}, "198.51.100.1"},
		{"several header lines", xff, "10.0.0.2:1234", []string{"6.6.6.6", "198.51.100.1"}, "198.51.100.1"},
		{"all trusted gives leftmost", xff, "10.0.0.2:1234", []string{"10.1.1.1, 10.0.0.9"}, "10.1.1.1"},
		{"garbage stops the walk", xff, "10.0.0.2:1234", []string{"198.51.100.1, garbage"}, "10.0.0.2"},
		{"missing header", xff, "10.0.0.2:1234", nil, "10.0.0.2"},
		{"entry with port", xff, "10.0.0.2:1234", []string{"198.51.100.1:5555"}, "198.51.100.1"},
		{"ipv6 peer and client", xff, "[fd00::1]:443", []string{"2001:db8::7"}, "2001:db8::7"},
		{"mapped ipv4 peer", xff, "[::ffff:10.0.0.2]:1234", []string{"198.51.100.1"}, "198.51.100.1"},
		{"unparsable remote", xff, "pipe", nil, "pipe"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := http.Header{}
			for _, v := range tt.header {
				h.Add("X-Forwarded-For", v)
			}
			if got := tt.r.ClientIP(tt.remote, h); got != tt.want {
				t.Errorf("ClientIP(%q, %v) = %q, want %q", tt.remote, tt.header, got, tt.want)
			}
		})
	}
}
