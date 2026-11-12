package rcloneenv

import (
	"slices"
	"testing"
)

func TestSecretForms(t *testing.T) {
	t.Parallel()
	got := secretForms([]string{"", "hunter2", "hunter2-long", "p@ss word/\"x\""})
	for _, want := range []string{
		"hunter2",
		"hunter2-long",
		`p@ss word/"x"`,         // plain
		`p@ss word/\"x\"`,       // Go-quoted
		"p%40ss+word%2F%22x%22", // query-escaped
		"p@ss%20word%2F%22x%22", // path-escaped
	} {
		if !slices.Contains(got, want) {
			t.Errorf("secretForms() lacks %q: %q", want, got)
		}
	}
	if slices.Contains(got, "") {
		t.Errorf("secretForms() kept an empty secret: %q", got)
	}
	for i := 1; i < len(got); i++ {
		if len(got[i]) > len(got[i-1]) {
			t.Errorf("secretForms() not longest first: %q", got)
		}
	}
	if len(secretForms(nil)) != 0 {
		t.Error("secretForms(nil) is not empty")
	}
}
