package hooks

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Zeptiny/Syncr/internal/worker/proc"
)

func lines(t *tail, ls ...string) {
	for _, l := range ls {
		t.add(proc.Line{Data: []byte(l)})
	}
}

func TestTail(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		max     int
		lines   []string
		want    string
		wantCut bool
	}{
		{name: "nothing", max: 10},
		{name: "fits", max: 10, lines: []string{"ab", "cd"}, want: "ab\ncd"},
		{name: "fits exactly", max: 5, lines: []string{"ab", "cd"}, want: "ab\ncd"},
		{name: "empty lines are kept", max: 10, lines: []string{"a", "", "b"}, want: "a\n\nb"},
		{name: "drops whole lines from the front", max: 5, lines: []string{"ab", "cd", "ef"}, want: "cd\nef", wantCut: true},
		{name: "drops as many lines as needed", max: 4, lines: []string{"a", "b", "c", "wxyz"}, want: "wxyz", wantCut: true},
		{name: "a line as long as the tail replaces the rest", max: 4, lines: []string{"ab", "wxyz"}, want: "wxyz", wantCut: true},
		{name: "a longer line keeps its end", max: 4, lines: []string{"ab", "0123456789"}, want: "6789", wantCut: true},
		{name: "lines after a long one", max: 4, lines: []string{"0123456789", "a"}, want: "a", wantCut: true},
		{name: "a short line after a full one", max: 4, lines: []string{"wxyz", "ab"}, want: "ab", wantCut: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tl := newTail(tt.max)
			lines(tl, tt.lines...)
			got, cut := tl.text()
			if got != tt.want || cut != tt.wantCut {
				t.Errorf("text() = %q, %v; want %q, %v", got, cut, tt.want, tt.wantCut)
			}
		})
	}
}

func TestTailReportsLinesTheSupervisorTruncated(t *testing.T) {
	t.Parallel()
	tl := newTail(100)
	tl.add(proc.Line{Data: []byte("start of a very long line"), Truncated: true})
	if got, cut := tl.text(); got != "start of a very long line" || !cut {
		t.Errorf("text() = %q, %v; want the line and cut", got, cut)
	}
}

// Output is reported in a protobuf string: it must be valid UTF-8 and stay
// within the bound, whatever bytes a command prints and wherever a long
// line was cut.
func TestTailTextIsValidAndBounded(t *testing.T) {
	t.Parallel()
	const limit = 64
	inputs := [][]string{
		{"caf\xe9 latin-1", "\xff\xfe\xfd"},
		{strings.Repeat("a\xff", 100)}, // every \xff becomes a 3-byte U+FFFD: the text grows
		{strings.Repeat("\xff", 200)},
		{strings.Repeat("é", 100)}, // cut in the middle of a character
		{strings.Repeat("日本語", 30), "x"},
		{"a", strings.Repeat("\x80", limit)},
	}
	for _, in := range inputs {
		tl := newTail(limit)
		lines(tl, in...)
		got, cut := tl.text()
		if !utf8.ValidString(got) {
			t.Errorf("text() of %q is not valid UTF-8: %q", in, got)
		}
		if len(got) > limit {
			t.Errorf("text() of %q is %d bytes, want at most %d", in, len(got), limit)
		}
		total := 0
		for _, l := range in {
			total += len(l) + 1
		}
		if total-1 > limit && !cut {
			t.Errorf("text() of %q (%d bytes) does not report the cut", in, total-1)
		}
	}
	// Invalid bytes that still fit are replaced without being reported as
	// dropped output.
	tl := newTail(limit)
	lines(tl, "caf\xe9")
	if got, cut := tl.text(); got != "caf�" || cut {
		t.Errorf("text() = %q, %v; want the replacement character and no cut", got, cut)
	}
}

// Whatever is added, the buffer stays bounded: a command that prints for
// hours does not grow the worker.
func TestTailStaysBounded(t *testing.T) {
	t.Parallel()
	tl := newTail(256)
	for i := range 10000 {
		tl.add(proc.Line{Data: []byte(strings.Repeat("x", i%300))})
		if len(tl.buf) > 257 {
			t.Fatalf("after line %d the buffer holds %d bytes", i, len(tl.buf))
		}
	}
	got, cut := tl.text()
	if !cut || len(got) > 256 || !strings.HasSuffix(got, strings.Repeat("x", 9999%300)) {
		t.Errorf("text() = %d bytes, cut %v; want the last line kept", len(got), cut)
	}
}
