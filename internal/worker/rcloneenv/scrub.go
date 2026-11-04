package rcloneenv

import (
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// Placeholder replaces secret values in scrubbed text.
const Placeholder = "[redacted]"

// secretForms returns the non-empty secrets plus the encoded forms in which
// tools commonly print them (Go-quoted, URL-escaped), deduplicated and
// longest first, so a longer secret is replaced before a shorter one it
// contains.
func secretForms(secrets []string) []string {
	var forms []string
	for _, s := range secrets {
		if s == "" {
			continue
		}
		q := strconv.Quote(s)
		forms = append(forms, s, q[1:len(q)-1], url.QueryEscape(s), url.PathEscape(s))
	}
	slices.SortFunc(forms, longestFirst)
	return slices.Compact(forms)
}

// Scrubber replaces secret values in text shown to users (tool error
// messages). It is safe for concurrent use.
type Scrubber struct {
	r *strings.Replacer
}

// NewScrubber returns a Scrubber for secrets, in any order. Empty strings
// are ignored.
func NewScrubber(secrets []string) *Scrubber {
	forms := secretForms(secrets)
	if len(forms) == 0 {
		return &Scrubber{}
	}
	pairs := make([]string, 0, 2*len(forms))
	for _, f := range forms {
		pairs = append(pairs, f, Placeholder)
	}
	return &Scrubber{r: strings.NewReplacer(pairs...)}
}

// Scrubber returns a Scrubber for e's secrets.
func (e *Env) Scrubber() *Scrubber { return NewScrubber(e.secrets) }

// Scrub returns s with every secret replaced by Placeholder.
func (s *Scrubber) Scrub(text string) string {
	if s == nil || s.r == nil {
		return text
	}
	return s.r.Replace(text)
}
