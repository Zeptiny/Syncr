package passive

import (
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/controller/dispatch"
	"github.com/Zeptiny/Syncr/internal/link"
)

// Defaults for ClientConfig.
const (
	DefaultTimeout             = 30 * time.Second
	DefaultDialTimeout         = 10 * time.Second
	DefaultTLSHandshakeTimeout = 10 * time.Second
	DefaultIdleConnTimeout     = 90 * time.Second
	// DefaultReadMaxBytes caps a worker's response: Pulls carry about 4
	// MiB of events by default (8 MiB at most) and single events far less.
	DefaultReadMaxBytes = 16 << 20
)

// ErrFingerprintMismatch reports a worker certificate that does not match
// the pinned fingerprint.
var ErrFingerprintMismatch = errors.New("passive worker certificate does not match the pinned fingerprint")

// ErrInvalidTarget reports an unusable worker URL, fingerprint or secret.
var ErrInvalidTarget = errors.New("invalid passive worker target")

// ClientConfig tunes Clients. Zero fields take the defaults.
type ClientConfig struct {
	// Timeout bounds one call, from dialing to the end of the response.
	Timeout time.Duration
	// DialTimeout bounds establishing a TCP connection.
	DialTimeout time.Duration
	// TLSHandshakeTimeout bounds a TLS handshake.
	TLSHandshakeTimeout time.Duration
	// IdleConnTimeout closes keep-alive connections idle this long. Keep it
	// above the passive loop's idle interval (15 s) to reuse connections.
	IdleConnTimeout time.Duration
	// ReadMaxBytes caps a decoded response message.
	ReadMaxBytes int
}

func (c ClientConfig) withDefaults() ClientConfig {
	c.Timeout = cmp.Or(c.Timeout, DefaultTimeout)
	c.DialTimeout = cmp.Or(c.DialTimeout, DefaultDialTimeout)
	c.TLSHandshakeTimeout = cmp.Or(c.TLSHandshakeTimeout, DefaultTLSHandshakeTimeout)
	c.IdleConnTimeout = cmp.Or(c.IdleConnTimeout, DefaultIdleConnTimeout)
	c.ReadMaxBytes = cmp.Or(c.ReadMaxBytes, DefaultReadMaxBytes)
	return c
}

// Client calls one passive worker. It implements dispatch.PassiveClient.
// Create it with NewClient and Close it when done.
type Client struct {
	target    string
	auth      string
	transport *http.Transport
	rpc       syncrv1connect.WorkerLinkServiceClient
}

var _ dispatch.PassiveClient = (*Client)(nil)

// NewClient returns a Client for t: t.URL must be an https:// base URL,
// t.CertFingerprint the pinned SHA-256 fingerprint (64 hex digits; colons,
// spaces and upper case accepted) and t.Secret non-empty.
func NewClient(t workers.PassiveTarget, cfg ClientConfig) (*Client, error) {
	cfg = cfg.withDefaults()
	u, err := url.Parse(t.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("%w: worker %s: url must be an https:// URL", ErrInvalidTarget, t.WorkerID)
	}
	pin, err := ParseFingerprint(t.CertFingerprint)
	if err != nil {
		return nil, fmt.Errorf("worker %s: %w", t.WorkerID, err)
	}
	if t.Secret == "" {
		return nil, fmt.Errorf("%w: worker %s: empty secret", ErrInvalidTarget, t.WorkerID)
	}
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	tr := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: cfg.DialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       PinnedTLSConfig(pin),
		TLSHandshakeTimeout:   cfg.TLSHandshakeTimeout,
		ResponseHeaderTimeout: cfg.Timeout,
		IdleConnTimeout:       cfg.IdleConnTimeout,
		MaxIdleConnsPerHost:   2,
		Protocols:             protocols,
	}
	hc := &http.Client{
		Transport: tr,
		Timeout:   cfg.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return &Client{
		target:    t.WorkerID.String(),
		auth:      link.PassiveAuthorization(t.Secret),
		transport: tr,
		rpc:       syncrv1connect.NewWorkerLinkServiceClient(hc, strings.TrimRight(u.String(), "/"), connect.WithReadMaxBytes(cfg.ReadMaxBytes)),
	}, nil
}

// Command delivers commands to the worker.
func (c *Client) Command(ctx context.Context, req *syncrv1.CommandRequest) (*syncrv1.CommandResponse, error) {
	r := connect.NewRequest(req)
	r.Header().Set("Authorization", c.auth)
	resp, err := c.rpc.Command(ctx, r)
	if err != nil {
		return nil, fmt.Errorf("command worker %s: %w", c.target, err)
	}
	return resp.Msg, nil
}

// Pull fetches the worker's events and state.
func (c *Client) Pull(ctx context.Context, req *syncrv1.PullRequest) (*syncrv1.PullResponse, error) {
	r := connect.NewRequest(req)
	r.Header().Set("Authorization", c.auth)
	resp, err := c.rpc.Pull(ctx, r)
	if err != nil {
		return nil, fmt.Errorf("pull worker %s: %w", c.target, err)
	}
	return resp.Msg, nil
}

// Close closes the client's idle connections.
func (c *Client) Close() { c.transport.CloseIdleConnections() }

// Fingerprint is a SHA-256 certificate fingerprint.
type Fingerprint [sha256.Size]byte

// String returns the fingerprint as 64 lower-case hex digits.
func (f Fingerprint) String() string { return hex.EncodeToString(f[:]) }

// ParseFingerprint parses 64 hex digits; colons, spaces and upper case are
// accepted. Errors wrap ErrInvalidTarget.
func ParseFingerprint(s string) (Fingerprint, error) {
	var f Fingerprint
	clean := strings.ToLower(strings.NewReplacer(":", "", " ", "").Replace(strings.TrimSpace(s)))
	b, err := hex.DecodeString(clean)
	if err != nil || len(b) != len(f) {
		return f, fmt.Errorf("%w: certificate fingerprint must be a SHA-256 fingerprint (64 hex digits)", ErrInvalidTarget)
	}
	copy(f[:], b)
	return f, nil
}

// PinnedTLSConfig returns a client TLS configuration that accepts exactly
// the leaf certificate whose DER encoding has SHA-256 pin, without CA or
// host name verification (see the package documentation).
func PinnedTLSConfig(pin Fingerprint) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		// The standard verification (CA chain, host name) is replaced by
		// the pin check in VerifyConnection, which runs on every handshake.
		InsecureSkipVerify: true, //nolint:gosec // verification is the leaf-certificate pin in VerifyConnection
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return fmt.Errorf("%w: the worker presented no certificate", ErrFingerprintMismatch)
			}
			got := Fingerprint(sha256.Sum256(cs.PeerCertificates[0].Raw))
			if subtle.ConstantTimeCompare(got[:], pin[:]) != 1 {
				return fmt.Errorf("%w: the worker presented %s, pinned %s", ErrFingerprintMismatch, got, pin)
			}
			return nil
		},
	}
}
