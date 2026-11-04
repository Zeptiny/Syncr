package rcloneenv

import (
	"strings"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
)

func TestScrubber(t *testing.T) {
	t.Parallel()
	s := NewScrubber([]string{"", "hunter2", "hunter2-long", "p@ss word/\"x\""})
	tests := map[string]string{
		"no secrets here":                        "no secrets here",
		"auth failed for hunter2":                "auth failed for [redacted]",
		"hunter2-long then hunter2":              "[redacted] then [redacted]",
		`quoted: "p@ss word/\"x\""`:              `quoted: "[redacted]"`,
		"url: https://u:p%40ss+word%2F%22x%22@h": "url: https://u:[redacted]@h",
		"path: p@ss%20word%2F%22x%22":            "path: [redacted]",
	}
	for in, want := range tests {
		if got := s.Scrub(in); got != want {
			t.Errorf("Scrub(%q) = %q, want %q", in, got, want)
		}
	}

	var nilScrubber *Scrubber
	if got := nilScrubber.Scrub("x"); got != "x" {
		t.Errorf("nil Scrubber changed the text: %q", got)
	}
	if got := NewScrubber(nil).Scrub("x"); got != "x" {
		t.Errorf("empty Scrubber changed the text: %q", got)
	}
}

func TestEnvScrubberCoversObscuredSecrets(t *testing.T) {
	t.Parallel()
	env, err := NewBuilder(nil).Remotes([]*syncrv1.RemoteDefinition{
		remote("box", "sftp", withSecrets("pass", "s3cr3t"), withObscure("pass")),
	})
	if err != nil {
		t.Fatal(err)
	}
	obscured := env.Vars()["RCLONE_CONFIG_BOX_PASS"]
	msg := "error: bad password s3cr3t (obscured " + obscured + ")"
	got := env.Scrubber().Scrub(msg)
	if strings.Contains(got, "s3cr3t") || strings.Contains(got, obscured) {
		t.Errorf("Scrub() = %q", got)
	}
}
