package notify

import (
	"bytes"
	htmltemplate "html/template"
	"strconv"
	"strings"
	texttemplate "text/template"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Zeptiny/Syncr/internal/domain"
)

// Rendering limits.
const (
	// MaxTitleRunes bounds Message.Title.
	MaxTitleRunes = 200
	// MaxFactRunes bounds a Fact's Value (an error message, a name).
	MaxFactRunes = 1000
)

// TestText is the body of a test notification.
const TestText = "This is a test notification from Syncr. The contact is set up correctly."

// Title returns the one-line summary of m, e.g. "Job failed: Nightly
// photos", "Job failed: Copy Photos → B2" (a job without a schedule, by its
// description) or "Scheduled run missed: Office docs", at most
// MaxTitleRunes runes. Names come from users and are not escaped:
// renderers escape for their format.
func Title(m Message) string {
	subject := ""
	switch {
	case m.Schedule != nil && m.Schedule.Name != "":
		subject = m.Schedule.Name
	case m.Job != nil && m.Job.Description != "":
		subject = m.Job.Description
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
	return truncateRunes(oneLine(title), MaxTitleRunes)
}

// Fact is one labeled detail of a notification, e.g. {"Worker",
// "nas-01"}. Values are plain text from users and workers (not escaped),
// on one line, at most MaxFactRunes runes.
type Fact struct {
	Label string
	Value string
}

// Facts returns the details of m in display order: the schedule, the
// job's description, worker, kind, status, error, transfer counters,
// duration and end, or the run's occurrence, reason and merged
// occurrences.
func Facts(m Message) []Fact {
	var out []Fact
	add := func(label, value string) {
		if value = truncateRunes(oneLine(value), MaxFactRunes); value != "" {
			out = append(out, Fact{Label: label, Value: value})
		}
	}
	if m.Test {
		return nil
	}
	if m.Schedule != nil {
		add("Schedule", m.Schedule.Name)
	}
	if j := m.Job; j != nil {
		add("Job", j.Description)
		add("Worker", j.WorkerName)
		add("Kind", j.Kind)
		add("Status", string(j.Status))
		switch {
		case j.ErrorCode != "" && j.ErrorMessage != "":
			add("Error", j.ErrorCode+": "+j.ErrorMessage)
		default:
			add("Error", j.ErrorCode+j.ErrorMessage)
		}
		if j.Bytes > 0 || j.Files > 0 {
			add("Transferred", FormatBytes(j.Bytes)+" in "+plural(j.Files, "file"))
		}
		if j.Errors > 0 {
			add("Errors", strconv.FormatInt(j.Errors, 10))
		}
		if j.DurationSeconds > 0 {
			add("Duration", (time.Duration(j.DurationSeconds * float64(time.Second))).Round(time.Second).String())
		}
		if !j.FinishedAt.IsZero() {
			add("Finished", formatTime(j.FinishedAt))
		}
	}
	if r := m.Run; r != nil {
		add("Scheduled for", formatTime(r.ScheduledFor))
		reason := r.Reason
		if r.Message != "" {
			reason = strings.TrimPrefix(reason+": "+r.Message, ": ")
		}
		add("Reason", reason)
		if r.Coalesced > 0 {
			add("Merged occurrences", strconv.Itoa(r.Coalesced))
		}
	}
	return out
}

// view is the data of the templates: what Message renders to.
type view struct {
	Title string
	Lead  string
	Facts []Fact
	URL   string
}

func viewOf(m Message) view {
	v := view{Title: m.Title, Facts: Facts(m), URL: m.URL}
	if v.Title == "" {
		v.Title = Title(m)
	} else {
		v.Title = truncateRunes(oneLine(v.Title), MaxTitleRunes)
	}
	if m.Test {
		v.Lead = TestText
	}
	return v
}

// The templates are compiled in; users cannot change them.
const (
	textSource = `{{.Title}}
{{- if .Lead}}

{{.Lead}}
{{- end}}
{{- if .Facts}}
{{range .Facts}}
{{.Label}}: {{.Value}}
{{- end}}
{{- end}}
{{- if .URL}}

Details: {{.URL}}
{{- end}}
`
	htmlSource = `<!DOCTYPE html>
<html><head><meta charset="utf-8"><title>{{.Title}}</title></head>
<body style="font-family:system-ui,-apple-system,Segoe UI,sans-serif;color:#1f2328;line-height:1.4">
<h2 style="margin:0 0 12px;font-size:18px">{{.Title}}</h2>
{{- if .Lead}}
<p>{{.Lead}}</p>
{{- end}}
{{- if .Facts}}
<table cellpadding="4" cellspacing="0" style="border-collapse:collapse;font-size:14px">
{{- range .Facts}}
<tr><th align="left" valign="top" style="color:#59636e;font-weight:600;padding-right:16px">{{.Label}}</th><td>{{.Value}}</td></tr>
{{- end}}
</table>
{{- end}}
{{- if .URL}}
<p><a href="{{.URL}}">Open in Syncr</a></p>
{{- end}}
<p style="color:#59636e;font-size:12px">Sent by Syncr.</p>
</body></html>
`
)

// Text renders m as plain text: the title, then one "Label: value" line
// per fact and the link. Values are not escaped (plain text needs none).
func Text(m Message) string {
	t := texttemplate.Must(texttemplate.New("text").Parse(textSource))
	var b bytes.Buffer
	if err := t.Execute(&b, viewOf(m)); err != nil {
		// Unreachable: the template is static and the view well-formed.
		return viewOf(m).Title + "\n"
	}
	return b.String()
}

// HTML renders m as an HTML document (an email body). Every value is
// escaped by html/template, so names cannot inject markup or links.
func HTML(m Message) string {
	t := htmltemplate.Must(htmltemplate.New("html").Parse(htmlSource))
	var b bytes.Buffer
	if err := t.Execute(&b, viewOf(m)); err != nil {
		return htmltemplate.HTMLEscapeString(viewOf(m).Title)
	}
	return b.String()
}

// EscapeMarkdown escapes the characters of s that Discord's markdown
// interprets (emphasis, code, spoilers, quotes, headings, masked links,
// mentions and their angle brackets), so user-chosen names render
// literally.
func EscapeMarkdown(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '\\', '*', '_', '~', '`', '|', '>', '<', '#', '[', ']', '@':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// EscapeSlack escapes &, < and > for Slack-style "mrkdwn" text, which
// Mattermost and Rocket.Chat accept too: "<...>" would otherwise be a
// link or a mention such as <!channel>.
func EscapeSlack(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// FormatBytes formats n bytes with binary units, e.g. "1.5 MiB".
func FormatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return plural(n, "byte")
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 5; m /= unit {
		div *= unit
		exp++
	}
	return strconv.FormatFloat(float64(n)/float64(div), 'f', 1, 64) + " " + string("KMGTPE"[exp]) + "iB"
}

func plural(n int64, word string) string {
	s := strconv.FormatInt(n, 10) + " " + word
	if n != 1 {
		s += "s"
	}
	return s
}

// formatTime formats t in UTC, which every recipient can place.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02 15:04:05 UTC")
}

// oneLine replaces runs of white space and control characters with one
// space, drops invalid UTF-8 and trims.
func oneLine(s string) string {
	s = strings.ToValidUTF8(s, "")
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
}

// truncateRunes cuts s to at most n runes, ending in "…" when cut.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}
