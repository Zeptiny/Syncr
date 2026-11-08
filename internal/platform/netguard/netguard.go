// Package netguard keeps outbound connections that users configure (the
// targets of notification webhooks, Discord, SMTP relays) away from
// addresses they must not reach: the controller itself, link-local
// networks with cloud metadata services, and, by policy, private networks.
//
// The check runs when a connection is made, on the address the socket is
// about to connect to (net.Dialer.Control), not on the host name: a name
// that resolves to an allowed address when it is saved and to 127.0.0.1
// when it is used (DNS rebinding) is still refused. HTTPClient never
// follows redirects (a public target cannot bounce a request inward) and
// never uses a proxy (the guard would check the proxy instead of the
// target), so proxies are unsupported for these connections.
//
// Policies:
//
//   - always denied (except in mode "any" or inside Policy.Allow): loopback,
//     link-local (169.254.0.0/16, fe80::/10, so 169.254.169.254), the cloud
//     metadata addresses outside them (fd00:ec2::254, 100.100.100.200),
//     unspecified, multicast and broadcast addresses, and the IPv4-mapped,
//     IPv4-compatible and NAT64 (64:ff9b::/96) forms of all of them;
//   - "private" (the default) allows everything else, LAN addresses
//     included (a Home Assistant instance, an internal SMTP relay);
//   - "public" also denies private and shared addresses (RFC 1918, fc00::/7,
//     100.64.0.0/10) and the other special-purpose ranges;
//   - "any" allows every address.
//
// Allowed dependencies: the standard library only.
package netguard

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"syscall"
	"time"
)

// Mode is how much of the network a Policy allows.
type Mode string

// Modes.
const (
	// ModePrivate denies loopback, link-local, metadata and other
	// non-unicast addresses and allows private networks (the default).
	ModePrivate Mode = "private"
	// ModePublic also denies private and other special-purpose networks.
	ModePublic Mode = "public"
	// ModeAny allows every address.
	ModeAny Mode = "any"
)

// ParseMode parses a configured mode; empty means ModePrivate.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(strings.ToLower(strings.TrimSpace(s))); m {
	case "":
		return ModePrivate, nil
	case ModePrivate, ModePublic, ModeAny:
		return m, nil
	}
	return "", fmt.Errorf("netguard: unknown network policy %q (want private, public or any)", s)
}

// ParsePrefixes parses CIDRs ("192.168.10.0/24", "fd12::/48") or single
// addresses (as /32 or /128) for Policy.Allow.
func ParsePrefixes(cidrs []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if !strings.Contains(c, "/") {
			a, err := netip.ParseAddr(c)
			if err != nil {
				return nil, fmt.Errorf("netguard: allowed network %q: %w", c, err)
			}
			a = a.Unmap()
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
			continue
		}
		p, err := netip.ParsePrefix(c)
		if err != nil {
			return nil, fmt.Errorf("netguard: allowed network %q: %w", c, err)
		}
		if p.Addr().Is4In6() {
			if p.Bits() < 96 {
				return nil, fmt.Errorf("netguard: allowed network %q: an IPv4-mapped prefix needs at least 96 bits", c)
			}
			p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// ErrDenied matches the errors of connections the policy refused (with
// errors.Is), however deeply a client wraps them.
var ErrDenied = errors.New("the address is not allowed by the network policy")

// DeniedError is a refused address.
type DeniedError struct {
	// Addr is the address that was refused.
	Addr netip.Addr
	// Reason names its class, e.g. "loopback".
	Reason string
}

// Error describes the refusal.
func (e *DeniedError) Error() string {
	return fmt.Sprintf("connection to %s refused: %s addresses are not allowed by the network policy", e.Addr, e.Reason)
}

// Is makes errors.Is(err, ErrDenied) match.
func (e *DeniedError) Is(target error) bool { return target == ErrDenied }

// Policy decides which addresses outbound connections may reach. The zero
// value is ModePrivate without extra allowed networks.
type Policy struct {
	Mode Mode
	// Allow are networks allowed whatever the mode (e.g. one LAN host
	// in mode public, or a local relay on loopback).
	Allow []netip.Prefix
}

// Special-purpose addresses and networks (IANA registries).
const (
	nat64Prefix  = "64:ff9b::/96"
	compatPrefix = "::/96" // IPv4-compatible (deprecated); also holds :: and ::1
	thisNetwork  = "0.0.0.0/8"
)

// metadataAddrs are cloud metadata services outside the link-local ranges.
func metadataAddrs() []netip.Addr {
	return []netip.Addr{
		netip.MustParseAddr("fd00:ec2::254"),   // AWS IMDS over IPv6
		netip.MustParseAddr("100.100.100.200"), // Alibaba Cloud
	}
}

// nonPublicRange is a range ModePublic denies besides the private ones.
type nonPublicRange struct {
	prefix netip.Prefix
	reason string
}

func nonPublicRanges() []nonPublicRange {
	r := func(p, reason string) nonPublicRange { return nonPublicRange{netip.MustParsePrefix(p), reason} }
	return []nonPublicRange{
		r("100.64.0.0/10", "shared (carrier-grade NAT)"),
		r("192.0.0.0/24", "IETF protocol assignment"),
		r("192.0.2.0/24", "documentation"),
		r("198.18.0.0/15", "benchmarking"),
		r("198.51.100.0/24", "documentation"),
		r("203.0.113.0/24", "documentation"),
		r("240.0.0.0/4", "reserved"),
		r("100::/64", "discard-only"),
		r("2001:db8::/32", "documentation"),
		r("2001::/23", "IETF protocol assignment"),
		r("2002::/16", "6to4"),
	}
}

// Check returns nil if p allows connecting to ip, or a *DeniedError
// (matching ErrDenied).
func (p Policy) Check(ip netip.Addr) error {
	if !ip.IsValid() {
		return &DeniedError{Addr: ip, Reason: "invalid"}
	}
	ip = ip.Unmap().WithZone("")
	for _, a := range p.Allow {
		if a.Contains(ip) {
			return nil
		}
	}
	if p.Mode == ModeAny {
		return nil
	}
	if reason := denied(ip, p.Mode == ModePublic); reason != "" {
		return &DeniedError{Addr: ip, Reason: reason}
	}
	// Addresses that embed an IPv4 address reach it: check that too.
	if ip.Is6() && (netip.MustParsePrefix(nat64Prefix).Contains(ip) || netip.MustParsePrefix(compatPrefix).Contains(ip)) {
		b := ip.As16()
		v4 := netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})
		if reason := denied(v4, p.Mode == ModePublic); reason != "" {
			return &DeniedError{Addr: ip, Reason: reason}
		}
	}
	return nil
}

// denied returns why ip (unmapped) is refused, or "".
func denied(ip netip.Addr, public bool) string {
	switch {
	case ip.IsLoopback():
		return "loopback"
	case ip.IsUnspecified() || netip.MustParsePrefix(thisNetwork).Contains(ip):
		return "unspecified"
	case ip.IsLinkLocalUnicast():
		return "link-local"
	case ip.IsMulticast():
		return "multicast"
	case ip == netip.AddrFrom4([4]byte{255, 255, 255, 255}):
		return "broadcast"
	}
	for _, m := range metadataAddrs() {
		if ip == m {
			return "cloud metadata"
		}
	}
	if !public {
		return ""
	}
	if ip.IsPrivate() {
		return "private"
	}
	for _, r := range nonPublicRanges() {
		if r.prefix.Contains(ip) {
			return r.reason
		}
	}
	return ""
}

// DialFunc dials like net.Dialer.DialContext.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// Dialer returns a TCP dial function whose connections p checks: the
// address each attempt connects to (after name resolution) must pass
// Check, or the attempt fails with an error matching ErrDenied before any
// packet is sent. Only the tcp networks are allowed. timeout bounds each
// connection attempt (zero means no bound beyond ctx).
func (p Policy) Dialer(timeout time.Duration) DialFunc {
	d := &net.Dialer{
		Timeout:   timeout,
		KeepAlive: 30 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			return p.checkConnect(network, address)
		},
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		switch network {
		case "tcp", "tcp4", "tcp6":
		default:
			return nil, fmt.Errorf("netguard: network %q is not allowed", network)
		}
		return d.DialContext(ctx, network, address)
	}
}

// checkConnect is the dialer's Control hook: address is the resolved
// "ip:port" the socket connects to.
func (p Policy) checkConnect(network, address string) error {
	switch network {
	case "tcp4", "tcp6":
	default:
		return fmt.Errorf("netguard: network %q is not allowed", network)
	}
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("netguard: unexpected address %q: %w", address, err)
	}
	return p.Check(ap.Addr())
}

// HTTPClient returns an HTTP client whose connections p checks (Dialer),
// which never follows redirects (a 3xx response is returned as is), never
// uses a proxy and speaks TLS 1.2 or later. timeout bounds each request,
// reading the body included.
func (p Policy) HTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           p.Dialer(timeout),
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          16,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
		CheckRedirect: NoRedirects,
	}
}

// NoRedirects is an http.Client.CheckRedirect that returns every redirect
// response to the caller instead of following it.
func NoRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
