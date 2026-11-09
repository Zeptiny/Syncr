package notify

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"html"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/Zeptiny/Syncr/internal/descriptor"
)

// RedactedMark replaces what a Redactor removes.
const RedactedMark = "[redacted]"

// Needle length limits. Shorter values match ordinary words by accident, so
// they are not searched for.
const (
	// minSecretLen is the shortest secret value (or form of one) that is
	// redacted.
	minSecretLen = 4
	// minPathLen is the shortest URL path (without slashes) that is
	// redacted as a whole.
	minPathLen = 3
	// minSegmentLen is the shortest path segment redacted on its own, when
	// it is not secret-like otherwise (see secretLike).
	minSegmentLen = 8
)

// commonSegment reports whether a path segment names an API rather than
// holds a secret; such segments are left readable so reasons stay useful.
func commonSegment(seg string) bool {
	switch strings.ToLower(seg) {
	case "webhook", "webhooks", "services", "notify", "trigger", "message", "messages",
		"incoming", "notification", "notifications":
		return true
	}
	return false
}

// Redactor removes a destination's secrets from text that came from the
// network (a receiver's reply, which may echo the request line, a header or
// the body) before it is stored, logged or shown. Channels build one per
// send from the contact's configuration; the services apply one to every
// reason as a safety net. A nil *Redactor redacts nothing.
//
// It matches, ignoring case (servers normalize paths), each secret value in
// the forms a receiver may echo it: as is, URL-escaped and unescaped,
// JSON-escaped (also with "\/"), HTML-escaped, base64-encoded, and without
// an authorization scheme ("Bearer "). For a value that is a URL it also
// matches the user info, the path (with and without the leading slash),
// every long enough or secret-like path segment, the query string and its
// values. The host is not secret (Summary shows it).
type Redactor struct {
	re *regexp.Regexp
}

// NewRedactor returns a Redactor for cfg: every secret value, plus extra
// values the channel knows to be sensitive (e.g. an SMTP user name, which
// is an option). Empty and very short values are ignored.
func NewRedactor(cfg descriptor.Values, extra ...string) *Redactor {
	seen := map[string]bool{}
	var needles []string
	add := func(s string) {
		if len(s) >= minSecretLen && !seen[s] {
			seen[s] = true
			needles = append(needles, s)
		}
	}
	values := make([]string, 0, len(cfg.Secrets)+len(extra))
	for _, v := range cfg.Secrets {
		values = append(values, v)
	}
	values = append(values, extra...)
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		for _, f := range forms(v) {
			add(f)
			for _, g := range transforms(f) {
				add(g)
			}
		}
	}
	if len(needles) == 0 {
		return &Redactor{}
	}
	// Leftmost-first alternation: longer needles must come first so that a
	// whole path wins over its segments.
	slices.SortFunc(needles, func(a, b string) int {
		if len(a) != len(b) {
			return len(b) - len(a)
		}
		return strings.Compare(a, b)
	})
	quoted := make([]string, len(needles))
	for i, n := range needles {
		quoted[i] = regexp.QuoteMeta(n)
	}
	return &Redactor{re: regexp.MustCompile("(?i)" + strings.Join(quoted, "|"))}
}

// Redact returns s with every secret replaced by RedactedMark.
func (r *Redactor) Redact(s string) string {
	if r == nil || r.re == nil || s == "" {
		return s
	}
	return r.re.ReplaceAllLiteralString(s, RedactedMark)
}

// RedactBytes is Redact for a reply body.
func (r *Redactor) RedactBytes(b []byte) []byte {
	if r == nil || r.re == nil || len(b) == 0 {
		return b
	}
	return r.re.ReplaceAllLiteral(b, []byte(RedactedMark))
}

// forms returns the parts of a secret value worth searching for besides the
// value itself: the token of an authorization header, the key of a
// "whsec_" secret, and the secret parts of a URL.
func forms(v string) []string {
	out := []string{v}
	if scheme, token, ok := strings.Cut(v, " "); ok && !strings.ContainsAny(scheme, "/:") && strings.TrimSpace(token) != "" {
		// "Bearer abc": servers echo the token alone as often as the header.
		out = append(out, strings.TrimSpace(token))
	}
	if rest, ok := strings.CutPrefix(v, "whsec_"); ok {
		out = append(out, rest)
	}
	u, err := url.Parse(v)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return out
	}
	if u.User != nil {
		out = append(out, u.User.String(), u.User.Username())
		if p, ok := u.User.Password(); ok {
			out = append(out, p)
		}
	}
	for _, p := range []string{u.Path, u.EscapedPath()} {
		trimmed := strings.Trim(p, "/")
		if len(trimmed) < minPathLen {
			continue
		}
		out = append(out, p, trimmed, "/"+trimmed)
		for seg := range strings.SplitSeq(trimmed, "/") {
			if secretLike(seg) {
				out = append(out, seg)
			}
		}
	}
	if u.RawQuery != "" {
		out = append(out, u.RawQuery)
		for k, vs := range u.Query() {
			for _, qv := range vs {
				out = append(out, qv)
				if len(qv) >= minSecretLen {
					out = append(out, k+"="+qv)
				}
			}
		}
	}
	return out
}

// secretLike reports whether a path segment could be (part of) a secret:
// long, or short with digits, and not a plain API word.
func secretLike(seg string) bool {
	if commonSegment(seg) {
		return false
	}
	if len(seg) >= minSegmentLen {
		return true
	}
	return len(seg) >= minSecretLen && strings.ContainsAny(seg, "0123456789")
}

// transforms returns the encodings of s a receiver may echo it in.
func transforms(s string) []string {
	out := []string{
		url.QueryEscape(s), url.PathEscape(s), html.EscapeString(s),
		strings.ReplaceAll(s, "/", `\/`),
		base64.StdEncoding.EncodeToString([]byte(s)),
		base64.RawStdEncoding.EncodeToString([]byte(s)),
		base64.URLEncoding.EncodeToString([]byte(s)),
		base64.RawURLEncoding.EncodeToString([]byte(s)),
	}
	if u, err := url.QueryUnescape(s); err == nil {
		out = append(out, u)
	}
	if u, err := url.PathUnescape(s); err == nil {
		out = append(out, u)
	}
	for _, escapeHTML := range []bool{false, true} {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(escapeHTML)
		if enc.Encode(s) != nil {
			continue
		}
		if j := strings.TrimSpace(buf.String()); len(j) >= 2 {
			j = j[1 : len(j)-1]
			out = append(out, j, strings.ReplaceAll(j, "/", `\/`))
		}
	}
	return out
}
