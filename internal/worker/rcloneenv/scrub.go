package rcloneenv

import (
	"net/url"
	"slices"
	"strconv"
)

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
