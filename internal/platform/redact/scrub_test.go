package redact

import (
	"strings"
	"testing"
)

func TestScrub(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		text    string
		secrets []string
		want    string
	}{
		{name: "no secrets", text: "open repository: wrong password", want: "open repository: wrong password"},
		{name: "empty secrets ignored", text: "abc", secrets: []string{"", ""}, want: "abc"},
		{name: "empty text", text: "", secrets: []string{"hunter2"}, want: ""},
		{name: "plain", text: "Fatal: password hunter2 rejected", secrets: []string{"hunter2"}, want: "Fatal: password [redacted] rejected"},
		{name: "every occurrence", text: "x hunter2 y hunter2", secrets: []string{"hunter2"}, want: "x [redacted] y [redacted]"},
		{
			name: "longest first, in any order", text: "key=abcdef and abc",
			secrets: []string{"abc", "abcdef"}, want: "key=[redacted] and [redacted]",
		},
		{name: "duplicates", text: "s3cr3t", secrets: []string{"s3cr3t", "s3cr3t"}, want: "[redacted]"},
		{
			name: "several secrets", text: "s3 secret AKIA/xyz with pw P@ss",
			secrets: []string{"AKIA/xyz", "P@ss"}, want: "s3 secret [redacted] with pw [redacted]",
		},
		{name: "go-quoted form", text: `error: "pa\"ss\\w"`, secrets: []string{`pa"ss\w`}, want: `error: "[redacted]"`},
		{name: "query-escaped form", text: "GET /x?k=a%2Bb%2Fc", secrets: []string{"a+b/c"}, want: "GET /x?k=[redacted]"},
		{name: "path-escaped form", text: "GET /bucket/a%20b", secrets: []string{"a b"}, want: "GET /bucket/[redacted]"},
		{name: "unicode", text: "pässwörd: grüße", secrets: []string{"grüße"}, want: "pässwörd: [redacted]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Scrub(tt.text, tt.secrets...); got != tt.want {
				t.Errorf("Scrub(%q, %q) = %q, want %q", tt.text, tt.secrets, got, tt.want)
			}
		})
	}
}

// TestScrubLeavesNoFragment: a secret that contains another is removed as a
// whole, never leaving a recognizable part behind.
func TestScrubLeavesNoFragment(t *testing.T) {
	t.Parallel()
	const long, short = "correct-horse-battery-staple", "horse"
	got := Scrub("pw="+long+"; user="+short, short, long)
	if strings.Contains(got, "battery") || strings.Contains(got, "staple") || strings.Contains(got, short) {
		t.Errorf("Scrub left a fragment: %q", got)
	}
}
