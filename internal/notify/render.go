package notify

import (
	"strings"
	"unicode/utf8"

	"github.com/Zeptiny/Syncr/internal/domain"
)

// MaxTitleRunes bounds Message.Title.
const MaxTitleRunes = 200

// Title returns the one-line summary of m, e.g. "Job failed: Nightly
// photos" or "Scheduled run missed: Office docs", at most MaxTitleRunes
// runes. Names come from users and are not escaped: renderers escape for
// their format.
//
// This is the minimal version of the contracts step; Text, HTML and Facts
// follow with the channels.
func Title(m Message) string {
	subject := ""
	switch {
	case m.Schedule != nil && m.Schedule.Name != "":
		subject = m.Schedule.Name
	case m.Job != nil:
		subject = m.Job.Kind
	}
	var title string
	switch {
	case m.Test || m.Event == domain.EventTest:
		title = "Syncr test notification"
	case m.Event == domain.EventMissed:
		title = "Scheduled run missed"
	case m.Run != nil && m.Event == domain.EventFailed:
		title = "Scheduled run failed to start"
	case m.Event != "":
		title = "Job " + string(m.Event)
	default:
		title = "Syncr notification"
	}
	if subject != "" && !m.Test {
		title += ": " + subject
	}
	return truncateRunes(strings.Join(strings.Fields(title), " "), MaxTitleRunes)
}

// truncateRunes cuts s to at most n runes, ending in "…" when cut.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}
