package notify_test

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/notify"
	"github.com/Zeptiny/Syncr/internal/notify/notifytest"
)

var update = flag.Bool("update", false, "rewrite the golden files of the render tests")

// hostile is a name a user could choose to break out of a format.
const hostile = `<script>alert("x")</script> & **bold** @everyone <@&123> [link](https://evil.example)`

func renderCases() map[string]notify.Message {
	failed := notifytest.SampleMessage()

	succeeded := notifytest.SampleMessage()
	succeeded.Event = domain.EventSucceeded
	succeeded.Job.Status, succeeded.Job.ErrorCode, succeeded.Job.ErrorMessage, succeeded.Job.Errors = domain.JobSucceeded, "", "", 0
	succeeded.Job.Bytes, succeeded.Job.Files, succeeded.Job.DurationSeconds = 5<<30+1<<29, 1, 3725.4
	succeeded.Schedule = nil
	succeeded.URL = ""
	succeeded.Title = notify.Title(succeeded)

	finished := time.Date(2026, 11, 4, 2, 45, 0, 0, time.UTC)
	missed := notify.Message{
		Version: notify.MessageVersion, ID: "d2", Event: domain.EventMissed, OccurredAt: finished,
		URL:      "https://syncr.example/schedules/0195f0a4-3b7c-7d1e-9f00-000000000002",
		Schedule: &notify.ScheduleInfo{ID: "0195f0a4-3b7c-7d1e-9f00-000000000002", Name: "Office docs hourly"},
		Run: &notify.RunInfo{ID: "r1", ScheduledFor: finished.Add(-45 * time.Minute), Reason: "worker_offline",
			Message: "office-pc was offline until the deadline", Coalesced: 3},
	}
	missed.Title = notify.Title(missed)

	evil := notifytest.SampleMessage()
	evil.Schedule.Name = hostile
	evil.Job.Description = "Copy " + hostile + " → B2"
	evil.Job.WorkerName = "nas\n01\tbox"
	evil.Job.ErrorMessage = "line one\nline two\r\n" + strings.Repeat("é", 1200)
	evil.URL = `https://syncr.example/jobs/1"><img src=x onerror=alert(1)>`
	evil.Title = notify.Title(evil)

	// A restic backup: the kind's label and its details (Milestone 5).
	backup := notifytest.SampleMessage()
	backup.Event = domain.EventSucceeded
	backup.Schedule = &notify.ScheduleInfo{ID: backup.Schedule.ID, Name: "Nightly photos backup"}
	backup.Job.Kind, backup.Job.KindLabel, backup.Job.Description = "restic_backup", "Backup", "Back up /srv/photos → Photos vault"
	backup.Job.Status, backup.Job.ErrorCode, backup.Job.ErrorMessage, backup.Job.Errors = domain.JobSucceeded, "", "", 0
	backup.Job.Bytes, backup.Job.Files = 51646976819, 1219
	backup.Job.Details = []notify.Fact{
		{Label: "Snapshot", Value: "1a2b3c4d"},
		{Label: "Files", Value: "12 new, 3 changed, 1,204 unmodified"},
		{Label: "Data added", Value: "1.2 GiB (840.0 MiB stored)"},
		{Label: "Processed", Value: "48.1 GiB in 1,219 files"},
	}
	backup.Title = notify.Title(backup)

	// A failed check started by hand: the label, no details.
	check := notifytest.SampleMessage()
	check.Schedule = nil
	check.Job.Kind, check.Job.KindLabel, check.Job.Description = "restic_check", "Check", "Check Photos vault (read 10% of the data)"
	check.Job.ErrorCode, check.Job.ErrorMessage = "execution_failed", "the check found 2 error(s)"
	check.Job.Bytes, check.Job.Files, check.Job.Errors = 0, 0, 0
	check.Title = notify.Title(check)

	test := notify.Message{Version: notify.MessageVersion, ID: "d3", Event: domain.EventTest, Test: true,
		OccurredAt: finished, URL: "https://syncr.example/notifications"}
	test.Title = notify.Title(test)

	return map[string]notify.Message{
		"failed": failed, "succeeded": succeeded, "missed": missed, "hostile": evil, "test": test,
		"backup": backup, "check_failed": check,
	}
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "render", name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update to create the golden files)", err)
	}
	if got != string(want) {
		t.Errorf("%s differs from the golden file:\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

func TestRenderGolden(t *testing.T) {
	t.Parallel()
	for name, m := range renderCases() {
		golden(t, name+".txt", notify.Text(m))
		golden(t, name+".html", notify.HTML(m))
	}
}

func TestHTMLEscapesHostileNames(t *testing.T) {
	t.Parallel()
	html := notify.HTML(renderCases()["hostile"])
	for _, bad := range []string{"<script>", "<img", `"><img`, "onerror=alert(1)>", "<@&"} {
		if strings.Contains(html, bad) {
			t.Errorf("the HTML contains %q unescaped", bad)
		}
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Error("the hostile name is missing or not escaped")
	}
}

func TestRenderTruncatesAndFlattens(t *testing.T) {
	t.Parallel()
	m := renderCases()["hostile"]
	for _, f := range notify.Facts(m) {
		if utf8.RuneCountInString(f.Value) > notify.MaxFactRunes || strings.ContainsAny(f.Value, "\r\n\t") {
			t.Errorf("fact %s = %q: longer than %d runes or not one line", f.Label, f.Value, notify.MaxFactRunes)
		}
	}
	long := notify.Message{Event: domain.EventFailed, Title: strings.Repeat("t", 500)}
	first := strings.SplitN(notify.Text(long), "\n", 2)[0]
	if utf8.RuneCountInString(first) != notify.MaxTitleRunes || !strings.HasSuffix(first, "…") {
		t.Errorf("a long title renders as %d runes", utf8.RuneCountInString(first))
	}
	// A message without a stored title gets one.
	if got := notify.Text(notify.Message{Event: domain.EventSucceeded}); !strings.HasPrefix(got, "Job succeeded") {
		t.Errorf("Text without a title = %q", got)
	}
}

func TestEscapes(t *testing.T) {
	t.Parallel()
	if got, want := notify.EscapeMarkdown("**a**_b_ `c` @everyone"), `\*\*a\*\*\_b\_ \`+"`"+`c\`+"`"+` \@everyone`; got != want {
		t.Errorf("EscapeMarkdown = %q, want %q", got, want)
	}
	if got := notify.EscapeSlack("<!channel> & <b>"); got != "&lt;!channel&gt; &amp; &lt;b&gt;" {
		t.Errorf("EscapeSlack = %q", got)
	}
	for n, want := range map[int64]string{0: "0 bytes", 1: "1 byte", 1023: "1023 bytes", 1024: "1.0 KiB", 1536: "1.5 KiB", 5 << 30: "5.0 GiB", 1 << 62: "4.0 EiB"} {
		if got := notify.FormatBytes(n); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestTitleNamesTheJob(t *testing.T) {
	t.Parallel()
	job := func(kind, description string) *notify.JobInfo {
		return &notify.JobInfo{Kind: kind, Description: description, Status: domain.JobFailed}
	}
	tests := []struct {
		name string
		m    notify.Message
		want string
	}{
		{"scheduled", notify.Message{Event: domain.EventFailed, Schedule: &notify.ScheduleInfo{Name: "Nightly"},
			Job: job("rclone_transfer", "Copy Photos → B2")}, "Job failed: Nightly"},
		{"manual", notify.Message{Event: domain.EventFailed, Job: job("rclone_transfer", "Copy Photos → B2")}, "Job failed: Copy Photos → B2"},
		{"manual without a description", notify.Message{Event: domain.EventFailed, Job: job("rclone_transfer", "")}, "Job failed: rclone_transfer"},
	}
	labeled := func(label, description string) *notify.JobInfo {
		j := job("restic_backup", description)
		j.KindLabel = label
		return j
	}
	tests = append(tests, []struct {
		name string
		m    notify.Message
		want string
	}{
		{"labeled, scheduled", notify.Message{Event: domain.EventFailed, Schedule: &notify.ScheduleInfo{Name: "Nightly photos backup"},
			Job: labeled("Backup", "Back up /srv/photos → Photos vault")}, "Backup failed: Nightly photos backup"},
		{"labeled, manual", notify.Message{Event: domain.EventSucceeded, Job: labeled("Check", "Check Photos vault")}, "Check succeeded: Check Photos vault"},
		{"labeled, interrupted", notify.Message{Event: domain.EventInterrupted, Job: labeled("Delete data", "Delete the data of Photos vault")},
			"Delete data interrupted: Delete the data of Photos vault"},
		{"a label of white space", notify.Message{Event: domain.EventFailed, Job: labeled(" \n", "x")}, "Job failed: x"},
		{"a run never takes the label", notify.Message{Event: domain.EventMissed, Schedule: &notify.ScheduleInfo{Name: "Nightly"}}, "Scheduled run missed: Nightly"},
	}...)
	for _, tt := range tests {
		if got := notify.Title(tt.m); got != tt.want {
			t.Errorf("%s: Title = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestFactsOfALabeledJob(t *testing.T) {
	t.Parallel()
	value := func(fs []notify.Fact, label string) (string, bool) {
		for _, f := range fs {
			if f.Label == label {
				return f.Value, true
			}
		}
		return "", false
	}
	m := renderCases()["backup"]
	facts := notify.Facts(m)
	if v, _ := value(facts, "Type"); v != "Backup" {
		t.Errorf("Type = %q, want the kind's label", v)
	}
	if _, ok := value(facts, "Kind"); ok {
		t.Error("the raw kind is shown next to its label")
	}
	if v, _ := value(facts, "Snapshot"); v != "1a2b3c4d" {
		t.Errorf("Snapshot = %q, want the detail", v)
	}
	if _, ok := value(facts, "Transferred"); ok {
		t.Error("the generic transfer counters are shown along with the details")
	}
	// Without details the counters stay; without a label, the raw kind.
	m.Job.Details, m.Job.KindLabel = nil, ""
	facts = notify.Facts(m)
	if v, _ := value(facts, "Transferred"); v != "48.1 GiB in 1219 files" {
		t.Errorf("Transferred = %q", v)
	}
	if v, _ := value(facts, "Kind"); v != "restic_backup" {
		t.Errorf("Kind = %q", v)
	}

	// Details are bounded, flattened and cannot blank out the counters.
	m.Job.Details = []notify.Fact{{Label: " ", Value: "no label"}, {Label: "Empty", Value: " "}}
	if _, ok := value(notify.Facts(m), "Transferred"); !ok {
		t.Error("empty details dropped the transfer counters")
	}
	m.Job.Details = nil
	for i := range 40 {
		m.Job.Details = append(m.Job.Details, notify.Fact{Label: strings.Repeat("l", 100) + string(rune('a'+i%26)), Value: "line one\nline two" + strings.Repeat("é", 1200)})
	}
	base := len(notify.Facts(renderCases()["backup"])) - len(renderCases()["backup"].Job.Details)
	facts = notify.Facts(m)
	if got := len(facts) - base; got != notify.MaxDetails {
		t.Errorf("%d details rendered, want at most %d", got, notify.MaxDetails)
	}
	for _, f := range facts {
		if utf8.RuneCountInString(f.Label) > notify.MaxDetailLabelRunes || utf8.RuneCountInString(f.Value) > notify.MaxFactRunes || strings.ContainsAny(f.Value, "\r\n") {
			t.Errorf("fact %q is not bounded to one line", f.Label)
		}
	}
}
