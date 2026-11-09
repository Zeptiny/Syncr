// Package scrub removes secret values from tool output before it reaches a
// job result, an event or a log. The runners share it.
//
// Text is always scrubbed whole and only then normalized or shortened:
// collapsing whitespace or cutting first would change or split a secret so
// that it no longer matches, and the rest of it would leak. Clip and Line do
// both steps in the right order; use them rather than cutting scrubbed text
// by hand.
//
// Besides each secret as given, a Scrubber redacts the forms in which tools
// commonly print it: Go-quoted (%q) and URL-escaped (query and path), and a
// secret cut off at the end of a text.
//
// Allowed dependencies: the standard library only.
package scrub

import (
	"cmp"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Placeholder replaces secret values in scrubbed text.
const Placeholder = "[redacted]"

// Ellipsis marks text shortened by Clip.
const Ellipsis = "..."

// MinFragment is the length of the shortest partial secret that is
// redacted: the start of a secret at the end of a text. Shorter pieces reveal little and would redact ordinary
// words. Whole secrets are redacted whatever their length.
const MinFragment = 6

// Scrubber replaces secret values in text. The zero value and nil scrub
// nothing. It is safe for concurrent use.
type Scrubber struct {
	r *strings.Replacer
	// forms are the redacted strings, longest first; the prefixes of those
	// longer than MinFragment are redacted at the end of a text.
	forms []string
}

// New returns a Scrubber for secrets, in any order. Empty strings are
// ignored.
func New(secrets []string) *Scrubber {
	var forms []string
	for _, s := range secrets {
		forms = append(forms, Forms(s)...)
	}
	if len(forms) == 0 {
		return &Scrubber{}
	}
	// Longest first, so a secret that contains another is replaced whole:
	// strings.Replacer tries the old strings in argument order.
	slices.SortFunc(forms, func(a, b string) int {
		return cmp.Or(cmp.Compare(len(b), len(a)), strings.Compare(a, b))
	})
	forms = slices.Compact(forms)
	pairs := make([]string, 0, 2*len(forms))
	for _, f := range forms {
		pairs = append(pairs, f, Placeholder)
	}
	return &Scrubber{r: strings.NewReplacer(pairs...), forms: forms}
}

// Forms returns secret and the forms in which it can appear in tool output
// (see the package documentation), without duplicates. It returns nil for
// an empty secret.
func Forms(secret string) []string {
	if secret == "" {
		return nil
	}
	forms := []string{secret}
	add := func(f string) {
		if f != "" && !slices.Contains(forms, f) {
			forms = append(forms, f)
		}
	}
	add(unquote(strconv.Quote(secret)))
	add(url.QueryEscape(secret))
	add(url.PathEscape(secret))
	return forms
}

// unquote strips the quotes of a quoted literal.
func unquote(q string) string {
	if len(q) < 2 {
		return ""
	}
	return q[1 : len(q)-1]
}

// collapse joins the fields of s with single spaces: one line, no runs of
// white space.
func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

// String returns text with every secret replaced by Placeholder.
func (s *Scrubber) String(text string) string {
	if s == nil || s.r == nil {
		return text
	}
	return s.r.Replace(text)
}

// Clip scrubs text, then shortens it to at most n bytes (plus Ellipsis) on
// a UTF-8 boundary, never inside a Placeholder. A partial secret left at the
// end of the result (text that was itself cut off, or cut by Clip) is
// redacted as well.
func (s *Scrubber) Clip(text string, n int) string {
	return s.clip(s.String(text), n)
}

// Line scrubs text, collapses it to one line without runs of white space,
// scrubs it again (a secret printed with different white space matches only
// then) and clips it to n bytes like Clip.
func (s *Scrubber) Line(text string, n int) string {
	return s.clip(s.String(collapse(s.String(text))), n)
}

// clip shortens scrubbed text; see Clip.
func (s *Scrubber) clip(text string, n int) string {
	if len(text) <= n {
		return s.redactTail(text)
	}
	cut := max(n, 0)
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	// Do not split a placeholder: "[reda" would hide that something was
	// removed.
	for i := max(0, cut-len(Placeholder)+1); i < cut; i++ {
		if strings.HasPrefix(text[i:], Placeholder) {
			cut = i
			break
		}
	}
	return s.redactTail(text[:cut]) + Ellipsis
}

// redactTail replaces the longest start (at least MinFragment bytes) of a
// secret form that text ends with.
func (s *Scrubber) redactTail(text string) string {
	if s == nil || text == "" {
		return text
	}
	last := text[len(text)-1]
	best := 0
	for _, f := range s.forms {
		for k := min(len(f)-1, len(text)); k >= max(MinFragment, best+1); k-- {
			if f[k-1] == last && text[len(text)-k:] == f[:k] {
				best = k
				break
			}
		}
	}
	if best == 0 {
		return text
	}
	return text[:len(text)-best] + Placeholder
}

// Error returns err with a scrubbed message, or err itself when scrubbing
// changes nothing. The result unwraps to err, so errors.Is and errors.As
// still see the original chain. It returns nil for nil.
func (s *Scrubber) Error(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	clean := s.String(msg)
	if clean == msg {
		return err
	}
	return &scrubbedError{msg: clean, err: err}
}

// scrubbedError is an error whose text has been scrubbed of secrets.
type scrubbedError struct {
	msg string
	err error
}

func (e *scrubbedError) Error() string { return e.msg }
func (e *scrubbedError) Unwrap() error { return e.err }

// IsScrubbed reports whether err is an error rewritten by Scrubber.Error.
func IsScrubbed(err error) bool {
	_, ok := errors.AsType[*scrubbedError](err)
	return ok
}
