package hooks

import (
	"bytes"
	"strings"
	"unicode/utf8"

	"github.com/Zeptiny/Syncr/internal/worker/proc"
)

// tail keeps the end of a command's output: the last lines that fit in max
// bytes. It is filled from the supervisor's line callbacks, which run one
// at a time on the goroutine that started the command, so it needs no lock.
type tail struct {
	max int
	buf []byte
	// cut reports that output was dropped: lines before the kept ones, or
	// the start (or, for a line the supervisor truncated, the end) of a
	// line.
	cut bool
}

func newTail(maxBytes int) *tail { return &tail{max: maxBytes} }

// add appends one line of output.
func (t *tail) add(l proc.Line) {
	if l.Truncated {
		t.cut = true
	}
	line := l.Data
	if len(line) > t.max {
		// The line alone overfills the tail: keep its end.
		line = line[len(line)-t.max:]
		t.cut = true
	}
	t.buf = append(t.buf, line...)
	t.buf = append(t.buf, '\n')
	// The final line break is not reported, so it does not count.
	if over := len(t.buf) - 1 - t.max; over > 0 {
		// Drop whole lines from the front until the rest fits. The line
		// just added fits by itself, so the break that ends the last line
		// to drop lies in what was there before.
		drop := over + bytes.IndexByte(t.buf[over-1:], '\n')
		t.buf = t.buf[:copy(t.buf, t.buf[drop:])]
		t.cut = true
	}
}

// text returns the kept output as valid UTF-8 of at most max bytes, without
// the final line break, and whether anything was dropped.
func (t *tail) text() (string, bool) {
	s := strings.ToValidUTF8(string(bytes.TrimSuffix(t.buf, []byte("\n"))), "�")
	cut := t.cut
	// Replacing invalid bytes can lengthen the text.
	if len(s) > t.max {
		start := len(s) - t.max
		for start < len(s) && !utf8.RuneStart(s[start]) {
			start++
		}
		s, cut = s[start:], true
	}
	return s, cut
}
