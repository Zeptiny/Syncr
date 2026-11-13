package proc

import "bytes"

// streamLine is a line queued for delivery to its stream's callback.
type streamLine struct {
	fn   LineFunc
	line Line
}

func (l streamLine) deliver() { l.fn(l.line) }

// lineWriter is the io.Writer exec copies a child stream into. It splits the
// stream into lines and queues them for the Run loop, which owns the
// callbacks. Write is only called from exec's copy goroutine; flush is only
// called by Run after Wait returned, so no locking is needed.
type lineWriter struct {
	fn        LineFunc
	max       int
	out       chan<- streamLine
	buf       []byte
	truncated bool
	// chunks queues what is written as it comes, in pieces of at most max
	// bytes, instead of splitting it into lines (Spec.StdoutChunks).
	chunks bool
}

func newLineWriter(fn LineFunc, maxBytes int, out chan<- streamLine) *lineWriter {
	return &lineWriter{fn: fn, max: maxBytes, out: out}
}

// Write implements io.Writer. It never fails; it blocks while the line queue
// is full, which back-pressures the child.
func (w *lineWriter) Write(p []byte) (int, error) {
	n := len(p)
	if w.chunks {
		for len(p) > 0 {
			c := min(len(p), w.max)
			w.out <- streamLine{fn: w.fn, line: Line{Data: bytes.Clone(p[:c])}}
			p = p[c:]
		}
		return n, nil
	}
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			w.append(p)
			break
		}
		w.append(p[:i])
		w.out <- streamLine{fn: w.fn, line: w.take()}
		p = p[i+1:]
	}
	return n, nil
}

// append adds b to the current line, keeping at most max bytes.
func (w *lineWriter) append(b []byte) {
	if room := w.max - len(w.buf); len(b) > room {
		b = b[:room]
		w.truncated = true
	}
	w.buf = append(w.buf, b...)
}

// take returns the current line and resets the buffer.
func (w *lineWriter) take() Line {
	data := w.buf
	if !w.truncated {
		data = bytes.TrimSuffix(data, []byte{'\r'})
	}
	l := Line{Data: bytes.Clone(data), Truncated: w.truncated}
	if l.Data == nil {
		l.Data = []byte{}
	}
	w.buf = w.buf[:0]
	w.truncated = false
	return l
}

// flush delivers a final line that was not terminated by a newline. It is a
// no-op on a nil writer (stream not captured).
func (w *lineWriter) flush() {
	if w == nil || (len(w.buf) == 0 && !w.truncated) {
		return
	}
	w.fn(w.take())
}
