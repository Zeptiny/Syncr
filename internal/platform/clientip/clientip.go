// Package clientip derives the IP address of an HTTP client, trusting a
// forwarding header only when the request comes from a known reverse proxy.
//
// Allowed dependencies: the standard library only.
package clientip

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Resolver derives client addresses. The zero value (no header) always
// returns the TCP peer's address. Safe for concurrent use.
type Resolver struct {
	header  string
	trusted []netip.Prefix
}

// New returns a Resolver that reads header (e.g. "X-Forwarded-For" or
// "X-Real-IP") on requests whose TCP peer lies in one of trusted. An empty
// header or no trusted prefixes means the peer address is always used.
func New(header string, trusted []netip.Prefix) *Resolver {
	return &Resolver{header: header, trusted: trusted}
}

// ClientIP returns the client's IP address for a request from remoteAddr
// (http.Request.RemoteAddr, "host:port") carrying header h.
//
// When the peer is a trusted proxy, the header's addresses are read from
// right to left (each proxy appends the address it received the request
// from) and the first one that is not itself a trusted proxy is the client.
// An unparsable entry stops the walk: the address to its right (or the peer)
// is returned, so a client can never choose its own key by sending a forged
// header through a proxy that appends to it.
func (r *Resolver) ClientIP(remoteAddr string, h http.Header) string {
	peer, ok := parseAddr(remoteAddr)
	if !ok {
		return remoteAddr
	}
	if r == nil || r.header == "" || !r.isTrusted(peer) {
		return peer.String()
	}
	var entries []string
	for _, v := range h.Values(r.header) {
		entries = append(entries, strings.Split(v, ",")...)
	}
	client := peer
	for i := len(entries) - 1; i >= 0; i-- {
		a, ok := parseAddr(strings.TrimSpace(entries[i]))
		if !ok {
			break
		}
		client = a
		if !r.isTrusted(a) {
			break
		}
	}
	return client.String()
}

func (r *Resolver) isTrusted(a netip.Addr) bool {
	for _, p := range r.trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// parseAddr accepts "ip", "ip:port" and "[ipv6]:port" and returns the
// address with IPv4-mapped IPv6 unmapped (zone dropped).
func parseAddr(s string) (netip.Addr, bool) {
	if s == "" {
		return netip.Addr{}, false
	}
	if a, err := netip.ParseAddr(s); err == nil {
		return a.Unmap().WithZone(""), true
	}
	host, _, err := net.SplitHostPort(s)
	if err != nil {
		return netip.Addr{}, false
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap().WithZone(""), true
}
