package scrub

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestString(t *testing.T) {
	t.Parallel()
	sc := New([]string{"abc", "abcdef", "", "abc"})
	tests := map[string]string{
		"no secret here": "no secret here",
		"the abc is out": "the [redacted] is out",
		"abcdef whole":   "[redacted] whole", // the longer secret wins
		"abcabc":         "[redacted][redacted]",
		"":               "",
		"newline\nabc\n": "newline\n[redacted]\n",
	}
	for in, want := range tests {
		if got := sc.String(in); got != want {
			t.Errorf("String(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNilAndEmptyScrubNothing(t *testing.T) {
	t.Parallel()
	var nilScrubber *Scrubber
	for _, sc := range []*Scrubber{nilScrubber, New(nil), New([]string{""}), {}} {
		if got := sc.String("x y"); got != "x y" {
			t.Errorf("String = %q", got)
		}
		if got := sc.Clip("abcdefgh", 4); got != "abcd"+Ellipsis {
			t.Errorf("Clip = %q", got)
		}
		if got := sc.Line(" a \n b ", 10); got != "a b" {
			t.Errorf("Line = %q", got)
		}
	}
}

// TestWhitespace checks that collapsing white space cannot reveal a secret
// with runs of spaces, or one printed with other spacing.
func TestWhitespace(t *testing.T) {
	t.Parallel()
	sc := New([]string{"pass  word", "one two"})
	for _, text := range []string{"key: pass  word end", "key: one   two end", "key: one\ttwo end"} {
		if got := sc.Line(text, 300); got != "key: "+Placeholder+" end" {
			t.Errorf("Line(%q) = %q", text, got)
		}
	}
}

func TestClipScrubsBeforeCutting(t *testing.T) {
	t.Parallel()
	secret := "s3cr3t-access-key-value"
	sc := New([]string{secret})
	// The secret straddles the cut: cutting first would leave a prefix that
	// matches no secret.
	text := strings.Repeat("x", 290) + secret + " tail"
	got := sc.Clip(text, 300)
	if strings.Contains(got, "s3cr3t") {
		t.Errorf("Clip leaks a prefix of the secret: %q", got)
	}
	if !strings.HasSuffix(got, Ellipsis) || len(got) > 300+len(Ellipsis) {
		t.Errorf("Clip = %q (%d bytes)", got, len(got))
	}
	// Line does the same after collapsing white space.
	got = sc.Line(strings.Repeat("y ", 145)+secret+"  tail", 300)
	if strings.Contains(got, "s3cr3t") {
		t.Errorf("Line leaks a prefix of the secret: %q", got)
	}
}

func TestClipNeverSplitsAPlaceholder(t *testing.T) {
	t.Parallel()
	sc := New([]string{"hunter2-long"})
	text := "abcd hunter2-long efgh"
	for n := range len(text) + 2 {
		got := sc.Clip(text, n)
		body := strings.TrimSuffix(got, Ellipsis)
		if i := strings.LastIndexByte(body, '['); i >= 0 && !strings.HasPrefix(body[i:], Placeholder) {
			t.Errorf("Clip(%d) = %q splits the placeholder", n, got)
		}
		if strings.Contains(got, "hunter") {
			t.Errorf("Clip(%d) = %q leaks", n, got)
		}
	}
}

// TestTrailingFragment checks that a secret cut off at the end of a text
// (for example by the supervisor's line limit) is redacted.
func TestTrailingFragment(t *testing.T) {
	t.Parallel()
	sc := New([]string{"hunter2-repo-password"})
	tests := map[string]string{
		"password=hunter2-re":      "password=" + Placeholder,
		"password=hunte":           "password=hunte", // below MinFragment
		"a hunter2-repo-password":  "a " + Placeholder,
		"nothing to see":           "nothing to see",
		"hunter2-repo-passwor":     Placeholder,
		"password=hunter2-re more": "password=hunter2-re more", // only at the end
	}
	for in, want := range tests {
		if got := sc.Clip(in, 100); got != want {
			t.Errorf("Clip(%q) = %q, want %q", in, got, want)
		}
	}
	// A cut that leaves the start of a secret that was not there whole.
	if got := sc.Clip("password=hunter2-repo-passwo and more", 20); got != "password="+Placeholder+Ellipsis {
		t.Errorf("Clip at a fragment = %q", got)
	}
}

func TestClipKeepsRunes(t *testing.T) {
	t.Parallel()
	sc := New(nil)
	if got := sc.Clip("héllo wörld", 2); got != "h"+Ellipsis {
		t.Errorf("Clip split a rune: %q", got)
	}
	if got := sc.Clip("short", 10); got != "short" {
		t.Errorf("Clip = %q", got)
	}
	got := sc.Line(strings.Repeat("é", 400), 301)
	if !utf8.ValidString(got) {
		t.Errorf("Line produced invalid UTF-8: %q", got)
	}
}

func TestError(t *testing.T) {
	t.Parallel()
	sc := New([]string{"abc"})
	if sc.Error(nil) != nil {
		t.Error("Error(nil) != nil")
	}
	orig := errors.New("x abc y")
	clean := sc.Error(orig)
	if clean.Error() != "x [redacted] y" || !errors.Is(clean, orig) || !IsScrubbed(clean) {
		t.Errorf("Error() = %v (unwrap ok: %v)", clean, errors.Is(clean, orig))
	}
	same := errors.New("nothing")
	if got := sc.Error(same); IsScrubbed(got) || !errors.Is(got, same) {
		t.Error("an error without secrets must be returned unchanged")
	}
}

func TestForms(t *testing.T) {
	t.Parallel()
	if Forms("") != nil {
		t.Error("Forms(\"\") != nil")
	}
	if got := Forms("plain"); len(got) != 1 || got[0] != "plain" {
		t.Errorf("Forms(plain) = %q, want no duplicate forms", got)
	}
}
