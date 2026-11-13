package proc

import (
	"slices"
	"testing"
)

func TestLineWriter(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		max       int
		writes    []string
		want      []string
		truncated []bool
	}{
		{name: "simple", max: 8, writes: []string{"a\nb\n"}, want: []string{"a", "b"}, truncated: []bool{false, false}},
		{name: "split across writes", max: 8, writes: []string{"he", "llo\nwo", "rld\n"}, want: []string{"hello", "world"}, truncated: []bool{false, false}},
		{name: "crlf split across writes", max: 8, writes: []string{"a\r", "\nb\r\n"}, want: []string{"a", "b"}, truncated: []bool{false, false}},
		{name: "only trailing cr is stripped", max: 8, writes: []string{"a\rb\n"}, want: []string{"a\rb"}, truncated: []bool{false}},
		{name: "empty lines", max: 8, writes: []string{"\n\n"}, want: []string{"", ""}, truncated: []bool{false, false}},
		{name: "exactly max", max: 4, writes: []string{"abcd\n"}, want: []string{"abcd"}, truncated: []bool{false}},
		{name: "one over max", max: 4, writes: []string{"abcde\nok\n"}, want: []string{"abcd", "ok"}, truncated: []bool{true, false}},
		{name: "long line over many writes", max: 4, writes: []string{"ab", "cd", "ef", "gh\nij\n"}, want: []string{"abcd", "ij"}, truncated: []bool{true, false}},
		{name: "unterminated final line", max: 8, writes: []string{"a\nrest"}, want: []string{"a", "rest"}, truncated: []bool{false, false}},
		{name: "unterminated truncated final line", max: 2, writes: []string{"abc"}, want: []string{"ab"}, truncated: []bool{true}},
		{name: "nothing", max: 8, writes: nil, want: nil, truncated: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got []string
			var truncated []bool
			fn := func(l Line) {
				got = append(got, string(l.Data))
				truncated = append(truncated, l.Truncated)
			}
			out := make(chan streamLine, 16)
			w := newLineWriter(fn, tt.max, out)
			for _, s := range tt.writes {
				n, err := w.Write([]byte(s))
				if n != len(s) || err != nil {
					t.Fatalf("Write(%q) = (%d, %v)", s, n, err)
				}
			}
			close(out)
			for l := range out {
				l.deliver()
			}
			w.flush()
			if !slices.Equal(got, tt.want) || !slices.Equal(truncated, tt.truncated) {
				t.Errorf("lines = %q truncated = %v; want %q truncated = %v", got, truncated, tt.want, tt.truncated)
			}
		})
	}
}

func TestLineDataIsOwnedByCallback(t *testing.T) {
	t.Parallel()
	var kept [][]byte
	out := make(chan streamLine, 4)
	w := newLineWriter(func(l Line) { kept = append(kept, l.Data) }, 8, out)
	_, _ = w.Write([]byte("one\ntwo\n"))
	close(out)
	for l := range out {
		l.deliver()
	}
	if string(kept[0]) != "one" || string(kept[1]) != "two" {
		t.Errorf("retained lines were overwritten: %q", kept)
	}
}

func TestNilLineWriterFlush(t *testing.T) {
	t.Parallel()
	var w *lineWriter
	w.flush() // must not panic
}

func TestLineWriterChunks(t *testing.T) {
	t.Parallel()
	var got []string
	out := make(chan streamLine, 16)
	w := newLineWriter(func(l Line) {
		if l.Truncated {
			t.Error("a chunk is marked truncated")
		}
		got = append(got, string(l.Data))
	}, 4, out)
	w.chunks = true
	for _, s := range []string{"ab\ncd", "", "efghij\r\n"} {
		if n, err := w.Write([]byte(s)); n != len(s) || err != nil {
			t.Fatalf("Write(%q) = (%d, %v)", s, n, err)
		}
	}
	close(out)
	for l := range out {
		l.deliver()
	}
	w.flush()
	// Chunks keep newlines and carriage returns, follow the writes and
	// are at most max bytes.
	if want := []string{"ab\nc", "d", "efgh", "ij\r\n"}; !slices.Equal(got, want) {
		t.Errorf("chunks = %q, want %q", got, want)
	}
}
