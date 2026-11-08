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
	evil.Job.WorkerName = "nas\n01\tbox"
	evil.Job.ErrorMessage = "line one\nline two\r\n" + strings.Repeat("é", 1200)
	evil.URL = `https://syncr.example/jobs/1"><img src=x onerror=alert(1)>`
	evil.Title = notify.Title(evil)

	test := notify.Message{Version: notify.MessageVersion, ID: "d3", Event: domain.EventTest, Test: true,
		OccurredAt: finished, URL: "https://syncr.example/notifications"}
	test.Title = notify.Title(test)

	return map[string]notify.Message{"failed": failed, "succeeded": succeeded, "missed": missed, "hostile": evil, "test": test}
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
