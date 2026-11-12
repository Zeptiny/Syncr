package notify

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// MaxReasonRunes bounds SendError.Reason.
const MaxReasonRunes = 300

// SendError is a failed Send. Its fields are safe to show and store: Reason
// is sanitized (no control characters, URLs replaced by "<url>", at most
// MaxReasonRunes runes) and channels never put secrets into it.
type SendError struct {
	// Permanent failures are not retried (e.g. the target rejected the
	// configuration); temporary ones are.
	Permanent bool
	// RetryAfter is how long the target asked the sender to wait before
	// the next attempt (e.g. HTTP 429 Retry-After); zero if it did not.
	RetryAfter time.Duration
	// Status is the protocol status of the target's reply (HTTP status,
	// SMTP reply code); zero if there was none (e.g. a network error).
	Status int
	// Reason says what went wrong, for people.
	Reason string
	// Public says that Status and Reason tell nothing about the
	// destination's state (our own validation, a fixed public service such
	// as Discord), so every user may be shown them. Any other failure is
	// the destination's or the network's answer (open or closed port,
	// DNS, TLS details, HTTP status and body), which would turn a
	// contact into a probe of the controller's network for users who
	// are not administrators: they are shown GenericReason instead
	// (see Visible).
	Public bool
}

// GenericReason is what users who may not see network details are shown
// for a failure that is not Public.
const GenericReason = "the notification could not be delivered to the destination; an administrator can see the details in the controller's log"

// Visible returns the status and reason to show a user: the failure's own
// for administrators and for Public failures, a status-less GenericReason
// otherwise.
func (e *SendError) Visible(admin bool) (status int, reason string) {
	if admin || e.Public {
		return e.Status, e.Reason
	}
	return 0, GenericReason
}

// Public marks err, if it is a *SendError, as safe to show to every user
// (see SendError.Public) and returns it; other errors are returned as is.
func Public(err error) error {
	var serr *SendError
	if !errors.As(err, &serr) {
		return err
	}
	c := *serr
	c.Public = true
	return &c
}

// Error describes the failure.
func (e *SendError) Error() string {
	kind := "temporary"
	if e.Permanent {
		kind = "permanent"
	}
	if e.Status != 0 {
		return fmt.Sprintf("notification failed (%s, status %d): %s", kind, e.Status, e.Reason)
	}
	return fmt.Sprintf("notification failed (%s): %s", kind, e.Reason)
}

// Permanent returns a *SendError that must not be retried. status is the
// reply's protocol status (0 if none); reason is sanitized.
func Permanent(status int, reason string) error {
	return &SendError{Permanent: true, Status: status, Reason: SanitizeReason(reason)}
}

// Temporary returns a *SendError that may be retried, after retryAfter when
// it is positive. status is the reply's protocol status (0 if none); reason
// is sanitized.
func Temporary(status int, reason string, retryAfter time.Duration) error {
	return &SendError{Status: status, Reason: SanitizeReason(reason), RetryAfter: max(retryAfter, 0)}
}

// SanitizeReason makes s safe to show and store as a SendError.Reason: URLs
// (anything starting with a scheme and "://") are replaced by "<url>",
// control characters and runs of white space become one space, invalid
// UTF-8 is dropped, and the result is trimmed and truncated to
// MaxReasonRunes runes (ending in "…" when cut). It cannot know secrets:
// channels must not pass them in.
func SanitizeReason(s string) string {
	s = strings.ToValidUTF8(s, "")
	fields := strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
	for i, f := range fields {
		if j := strings.Index(f, "://"); j > 0 && isScheme(f[:j]) {
			fields[i] = f[:schemeStart(f[:j])] + "<url>"
		}
	}
	out := strings.Join(fields, " ")
	if utf8.RuneCountInString(out) > MaxReasonRunes {
		r := []rune(out)
		out = string(r[:MaxReasonRunes-1]) + "…"
	}
	return out
}

// isScheme reports whether s ends in a URL scheme (letters, digits, "+",
// "-", "." after a letter), possibly after punctuation such as a quote.
func isScheme(s string) bool {
	start := schemeStart(s)
	if start == len(s) {
		return false
	}
	c := s[start]
	return 'a' <= c|0x20 && c|0x20 <= 'z'
}

// schemeStart returns the index where the trailing scheme characters of s
// begin.
func schemeStart(s string) int {
	i := len(s)
	for i > 0 {
		c := s[i-1]
		if 'a' <= c|0x20 && c|0x20 <= 'z' || '0' <= c && c <= '9' || c == '+' || c == '-' || c == '.' {
			i--
			continue
		}
		break
	}
	return i
}
