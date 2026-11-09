// Package sendutil is what the built-in notification channels share: one
// bounded HTTP POST, the mapping of its outcome to *notify.SendError, and
// the sanitizing that keeps URLs, hosts and tokens out of the reasons.
//
// Reasons never quote a transport error's text: a *url.Error carries the
// URL (with its token) and a *net.DNSError or *net.OpError the host and
// address, which are part of the secret URL of a webhook. Only the class
// of failure, the status code and a short sanitized excerpt of the
// target's own reply are kept.
package sendutil

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Zeptiny/Syncr/internal/notify"
	"github.com/Zeptiny/Syncr/internal/platform/netguard"
)

// MaxResponseBytes caps the response body read from a target.
const MaxResponseBytes = 64 << 10

// MaxSnippetRunes bounds the excerpt of a reply kept in a reason.
const MaxSnippetRunes = 200

// MaxRetryAfter caps a target's requested delay.
const MaxRetryAfter = time.Hour

// Response is a target's reply: the status, the headers and up to
// MaxResponseBytes of the body.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// Post sends body to url with the headers and reads the reply. A
// transport failure is returned as a *notify.SendError (see
// TransportError); any HTTP status is a Response for the caller to judge.
// what names the target in reasons ("the webhook", "Discord").
func Post(ctx context.Context, client *http.Client, url string, header http.Header, body []byte, what string) (Response, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, TransportError(err, what)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		// The configuration passed Validate, so this is a URL Go cannot
		// send to; never quote it.
		return Response{}, notify.Public(notify.Permanent(0, what+" URL cannot be used"))
	}
	for k, vs := range header {
		req.Header[k] = vs
	}
	resp, err := client.Do(req)
	if err != nil {
		return Response{}, TransportError(err, what)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes))
	if err != nil && resp.StatusCode/100 != 2 {
		// The status is known; a body cut short does not change it.
		b = nil
	}
	return Response{Status: resp.StatusCode, Header: resp.Header, Body: b}, nil
}

// TransportError maps a failure to reach the target to a *notify.SendError
// without quoting it: cancellation and timeouts, an address the network
// policy refuses (permanent), name resolution, TLS and connection
// failures (temporary).
func TransportError(err error, what string) error {
	var (
		dnsErr  *net.DNSError
		certErr *tls.CertificateVerificationError
		unknown x509.UnknownAuthorityError
		host    x509.HostnameError
	)
	switch {
	case errors.Is(err, netguard.ErrDenied):
		return notify.Permanent(0, what+" address is not allowed by the notification network policy (notify.network_policy)")
	case errors.Is(err, context.Canceled):
		return notify.Public(notify.Temporary(0, "the send was cancelled", 0))
	case errors.Is(err, context.DeadlineExceeded) || isTimeout(err):
		return notify.Temporary(0, "the connection to "+what+" timed out", 0)
	case errors.As(err, &dnsErr):
		return notify.Temporary(0, "the host name of "+what+" cannot be resolved", 0)
	case errors.As(err, &certErr), errors.As(err, &unknown), errors.As(err, &host):
		return notify.Temporary(0, "the TLS certificate of "+what+" is not trusted", 0)
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
		return notify.Temporary(0, what+" closed the connection", 0)
	}
	return notify.Temporary(0, "cannot connect to "+what, 0)
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// Snippet returns a short, sanitized excerpt of a reply body for a reason:
// one line, URLs replaced, at most MaxSnippetRunes runes; "" for none. The
// destination's secrets are redacted (r may be nil) from the whole body
// before it is cut, so a receiver that echoes the request path, a header or
// the signing secret leaks neither all of it nor a truncated part.
func Snippet(b []byte, r *notify.Redactor) string {
	if !utf8.Valid(b) {
		return ""
	}
	s := notify.SanitizeReason(string(r.RedactBytes(b)))
	if utf8.RuneCountInString(s) > MaxSnippetRunes {
		r := []rune(s)
		s = string(r[:MaxSnippetRunes-1]) + "…"
	}
	return s
}

// RetryAfter parses a Retry-After header (delay-seconds, possibly with a
// fraction as Discord sends, or an HTTP date relative to now), capped at
// MaxRetryAfter; zero if absent or invalid.
func RetryAfter(h http.Header, now time.Time) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil {
		return Seconds(secs)
	}
	if t, err := http.ParseTime(v); err == nil {
		return min(max(t.Sub(now), 0), MaxRetryAfter)
	}
	return 0
}

// Seconds converts a delay in seconds to a duration in [0, MaxRetryAfter].
func Seconds(secs float64) time.Duration {
	if secs <= 0 || secs != secs { // negative, zero or NaN
		return 0
	}
	if secs >= MaxRetryAfter.Seconds() {
		return MaxRetryAfter
	}
	return time.Duration(secs * float64(time.Second))
}

// StatusReason is the reason of an HTTP failure: "<what> answered <code>
// <text>" plus the excerpt of the reply, if any.
func StatusReason(what string, status int, detail string) string {
	r := fmt.Sprintf("%s answered %d %s", what, status, http.StatusText(status))
	if detail != "" {
		r += ": " + detail
	}
	return r
}
