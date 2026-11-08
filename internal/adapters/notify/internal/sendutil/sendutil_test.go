package sendutil_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Zeptiny/Syncr/internal/adapters/notify/internal/sendutil"
	"github.com/Zeptiny/Syncr/internal/notify"
	"github.com/Zeptiny/Syncr/internal/platform/netguard"
)

func TestTransportErrorNeverQuotes(t *testing.T) {
	t.Parallel()
	const secretURL = "https://hooks.example/secret-token-123"
	wrap := func(err error) error { return &url.Error{Op: "Post", URL: secretURL, Err: err} }
	tests := []struct {
		err       error
		permanent bool
		reason    string
	}{
		{wrap(&net.OpError{Op: "dial", Err: &netguard.DeniedError{Addr: netip.MustParseAddr("127.0.0.1"), Reason: "loopback"}}), true, "network policy"},
		{wrap(context.Canceled), false, "cancelled"},
		{wrap(context.DeadlineExceeded), false, "timed out"},
		{wrap(&net.DNSError{Err: "no such host", Name: "hooks.example"}), false, "resolved"},
		{wrap(errors.New("dial tcp 10.0.0.1:443: connection refused")), false, "cannot connect"},
	}
	for _, tt := range tests {
		err := sendutil.TransportError(tt.err, "the webhook")
		var serr *notify.SendError
		if !errors.As(err, &serr) || serr.Permanent != tt.permanent || !strings.Contains(serr.Reason, tt.reason) {
			t.Errorf("%v → %v", tt.err, err)
		}
		for _, leak := range []string{"secret-token-123", "hooks.example", "10.0.0.1", "127.0.0.1"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("%v → %q quotes %q", tt.err, err, leak)
			}
		}
	}
}

func TestRetryAfter(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	for v, want := range map[string]time.Duration{
		"": 0, "0": 0, "-5": 0, "NaN": 0, "junk": 0, "1.5": 1500 * time.Millisecond, "60": time.Minute, "86400": time.Hour,
		now.Add(90 * time.Second).Format(http.TimeFormat): 90 * time.Second,
		now.Add(-time.Minute).Format(http.TimeFormat):     0,
	} {
		h := http.Header{}
		if v != "" {
			h.Set("Retry-After", v)
		}
		if got := sendutil.RetryAfter(h, now); got != want {
			t.Errorf("RetryAfter(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestSnippet(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 500)
	if got := sendutil.Snippet([]byte(long)); len([]rune(got)) != sendutil.MaxSnippetRunes {
		t.Errorf("Snippet(500 x) has %d runes", len([]rune(got)))
	}
	if got := sendutil.Snippet([]byte("see https://evil.example/token\nnow")); got != "see <url> now" {
		t.Errorf("Snippet = %q", got)
	}
	if got := sendutil.Snippet([]byte{0xff, 0xfe}); got != "" {
		t.Errorf("Snippet(binary) = %q", got)
	}
	if got := sendutil.StatusReason("the webhook", http.StatusServiceUnavailable, ""); got != fmt.Sprintf("the webhook answered 503 %s", http.StatusText(http.StatusServiceUnavailable)) {
		t.Errorf("StatusReason = %q", got)
	}
}
