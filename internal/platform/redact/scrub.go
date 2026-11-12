package redact

import (
	"cmp"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// Scrub returns text with every occurrence of each secret replaced by
// Placeholder. It is the defense for text that may echo a secret verbatim,
// such as a tool's error message: the controller scrubs what workers
// report even though workers scrub it too.
//
// Besides the plain values it replaces the forms in which tools commonly
// print them: Go-quoted (escaped quotes, backslashes and control
// characters) and URL-escaped (query and path escaping). Longer forms are
// replaced first, so a secret that contains another is removed as a whole.
// Empty secrets are ignored; secrets may be passed in any order and more
// than once.
//
// A very short secret also hits unrelated text (a one-letter password
// replaces every such letter): over-redaction is the cheaper failure.
func Scrub(text string, secrets ...string) string {
	forms := scrubForms(secrets)
	if len(forms) == 0 || text == "" {
		return text
	}
	pairs := make([]string, 0, 2*len(forms))
	for _, f := range forms {
		pairs = append(pairs, f, Placeholder)
	}
	// strings.Replacer tries the old strings in argument order at each
	// position, so the longest-first order wins at a shared position.
	return strings.NewReplacer(pairs...).Replace(text)
}

// scrubForms returns the non-empty secrets and their encoded forms,
// deduplicated, longest first (ties in byte order, for a deterministic
// replacer).
func scrubForms(secrets []string) []string {
	var forms []string
	for _, s := range secrets {
		if s == "" {
			continue
		}
		q := strconv.Quote(s)
		forms = append(forms, s, q[1:len(q)-1], url.QueryEscape(s), url.PathEscape(s))
	}
	slices.SortFunc(forms, func(a, b string) int {
		return cmp.Or(cmp.Compare(len(b), len(a)), strings.Compare(a, b))
	})
	return slices.Compact(forms)
}
