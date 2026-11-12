package notifications_test

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Zeptiny/Syncr/internal/app/notifications"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/notify"
)

// labeler is a JobDescriber that also labels the job's kind and reports
// details, as the controller's does for restic jobs; it records the output
// it was given.
type labeler struct {
	desc    notifications.JobDescription
	outputs []string
}

func (l *labeler) DescribeJob(_ context.Context, _ domain.UserID, _ string, _, output []byte) (notifications.JobDescription, error) {
	l.outputs = append(l.outputs, string(output))
	return l.desc, nil
}

// TestPlanCarriesKindLabelAndDetails: the kind's label titles the
// notification and the details are stored with it, bounded; the job's
// output reaches the describer.
func TestPlanCarriesKindLabelAndDetails(t *testing.T) {
	t.Parallel()
	l := &labeler{desc: notifications.JobDescription{
		Text: "Back up /srv/photos → Photos vault", KindLabel: "Backup",
		Details: []notify.Fact{{Label: "Snapshot", Value: "1a2b3c4d"}, {Label: "Data added", Value: "1.2 GiB"}},
	}}
	e := newEnv(t, withDescriber(l))
	e.contact(e.alice, "on", true, domain.EventSucceeded)

	j := e.job(e.alice, domain.JobSucceeded, domain.ScheduleID{})
	j.Output = []byte("encoded output")
	if _, err := e.store.UpdateJob(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	if n := e.plan(); n != 1 {
		t.Fatalf("Plan = %d, want 1", n)
	}
	m := payload(t, e.only())
	if m.Title != "Backup succeeded: Back up /srv/photos → Photos vault" || m.Job.KindLabel != "Backup" || len(m.Job.Details) != 2 ||
		m.Job.Details[0] != (notify.Fact{Label: "Snapshot", Value: "1a2b3c4d"}) {
		t.Errorf("message title %q, job %+v", m.Title, m.Job)
	}
	if len(l.outputs) != 1 || l.outputs[0] != "encoded output" {
		t.Errorf("the describer got the outputs %q, want the job's", l.outputs)
	}
	text := notify.Text(m)
	for _, want := range []string{"Type: Backup", "Snapshot: 1a2b3c4d", "Data added: 1.2 GiB"} {
		if !strings.Contains(text, want) {
			t.Errorf("the rendered message lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Kind:") || strings.Contains(text, "Transferred:") {
		t.Errorf("the rendered message shows the raw kind or the generic counters:\n%s", text)
	}
}

// TestPlanBoundsDetails: what a describer returns is cut to the limits of
// the stored message.
func TestPlanBoundsDetails(t *testing.T) {
	t.Parallel()
	var many []notify.Fact
	for range 3 * notify.MaxDetails {
		many = append(many, notify.Fact{Label: strings.Repeat("l", 500), Value: strings.Repeat("é", 5000)})
	}
	many[1] = notify.Fact{Label: "", Value: "no label"}
	many[2] = notify.Fact{Label: "no value"}
	l := &labeler{desc: notifications.JobDescription{Text: "x", KindLabel: strings.Repeat("k", 500), Details: many}}
	e := newEnv(t, withDescriber(l))
	e.contact(e.alice, "on", true, domain.EventFailed)
	e.job(e.alice, domain.JobFailed, domain.ScheduleID{})
	if n := e.plan(); n != 1 {
		t.Fatalf("Plan = %d, want 1", n)
	}
	m := payload(t, e.only())
	if len(m.Job.Details) != notify.MaxDetails || utf8.RuneCountInString(m.Job.KindLabel) > notify.MaxDetailLabelRunes {
		t.Fatalf("%d details, a label of %d runes", len(m.Job.Details), utf8.RuneCountInString(m.Job.KindLabel))
	}
	for _, d := range m.Job.Details {
		if d.Label == "" || d.Value == "" || utf8.RuneCountInString(d.Label) > notify.MaxDetailLabelRunes || utf8.RuneCountInString(d.Value) > notify.MaxFactRunes {
			t.Fatalf("an unbounded or empty detail was stored: %d/%d runes", utf8.RuneCountInString(d.Label), utf8.RuneCountInString(d.Value))
		}
	}
	if len(e.only().Payload) > 64<<10 {
		t.Errorf("the stored payload is %d bytes", len(e.only().Payload))
	}
}
