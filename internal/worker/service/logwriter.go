package service

import (
	"bytes"
	"io"
	"strings"
	"sync"

	kservice "github.com/kardianos/service"
)

// NewLogWriter returns an io.Writer that forwards each line of structured
// log output (slog text or JSON) to the service manager's log, mapping the
// record's level to error, warning or info. Lines are never split.
func NewLogWriter(l kservice.Logger) io.Writer { return &lineWriter{logger: l} }

type lineWriter struct {
	logger kservice.Logger
	mu     sync.Mutex
	buf    bytes.Buffer
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	for {
		line, err := w.buf.ReadString('\n')
		if err != nil { // incomplete line: keep it for the next write
			w.buf.Reset()
			w.buf.WriteString(line)
			return len(p), nil
		}
		w.emit(strings.TrimRight(line, "\r\n"))
	}
}

func (w *lineWriter) emit(line string) {
	if line == "" {
		return
	}
	switch levelOf(line) {
	case "ERROR":
		_ = w.logger.Error(line)
	case "WARN":
		_ = w.logger.Warning(line)
	default:
		_ = w.logger.Info(line)
	}
}

// levelOf extracts the level from a slog text ("level=WARN") or JSON
// ("\"level\":\"WARN\"") line; slog puts it near the start.
func levelOf(line string) string {
	head := line
	if len(head) > 96 {
		head = head[:96]
	}
	for _, level := range []string{"ERROR", "WARN"} {
		if strings.Contains(head, "level="+level) || strings.Contains(head, `"level":"`+level+`"`) {
			return level
		}
	}
	return "INFO"
}
