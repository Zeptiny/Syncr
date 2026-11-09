package main

import (
	"context"
	"errors"
	"testing"

	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
)

// fakeRemoteNames answers Names from refs, or fails with err.
type fakeRemoteNames struct {
	refs map[domain.RemoteID]remotes.Ref
	err  error
}

func (f fakeRemoteNames) Names(_ context.Context, ids []domain.RemoteID) (map[domain.RemoteID]remotes.Ref, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := map[domain.RemoteID]remotes.Ref{}
	for _, id := range ids {
		if r, ok := f.refs[id]; ok {
			out[id] = r
		}
	}
	return out, nil
}

// TestJobDescriber: jobs are described as the web UI's jobs list shows
// them, with the names of the owner's remotes (deleted ones included) and
// local paths; another user's remote is never named.
func TestJobDescriber(t *testing.T) {
	t.Parallel()
	kinds, err := jobkind.NewRegistry(jobkind.Builtin()...)
	if err != nil {
		t.Fatal(err)
	}
	owner, other := domain.NewUserID(), domain.NewUserID()
	photos, b2, theirs := domain.NewRemoteID(), domain.NewRemoteID(), domain.NewRemoteID()
	names := fakeRemoteNames{refs: map[domain.RemoteID]remotes.Ref{
		photos: {ID: photos, OwnerID: owner, Name: "Photos"},
		b2:     {ID: b2, OwnerID: owner, Name: "B2 offsite", Deleted: true},
		theirs: {ID: theirs, OwnerID: other, Name: "Bob's secret bucket"},
	}}
	d := jobDescriber{kinds: kinds, remotes: names}
	def := func(mode, src, dst string) []byte {
		return []byte(`{"rclone_transfer":{"mode":"RCLONE_TRANSFER_MODE_` + mode + `","source":` + src + `,"destination":` + dst + `}}`)
	}
	on := func(id domain.RemoteID) string { return `{"remote_id":"` + id.String() + `","path":"x"}` }
	tests := []struct {
		name, kind string
		definition []byte
		want       string
	}{
		{"remotes", "rclone_transfer", def("COPY", on(photos), on(b2)), "Copy Photos → B2 offsite"},
		{"local source", "rclone_transfer", def("SYNC", `{"path":"/srv/data"}`, on(b2)), "Sync /srv/data → B2 offsite"},
		{"another user's remote", "rclone_transfer", def("MOVE", on(theirs), `{"path":"/backup"}`), "Move Unknown remote → /backup"},
		{"not JSON", "rclone_transfer", []byte("{"), ""},
		{"another kind", "restic_backup", def("COPY", on(photos), on(b2)), ""},
	}
	for _, tt := range tests {
		got, err := d.DescribeJob(t.Context(), owner, tt.kind, tt.definition, nil)
		if err != nil || got.Text != tt.want {
			t.Errorf("%s: DescribeJob = (%q, %v), want %q", tt.name, got.Text, err, tt.want)
		}
	}

	// The names cannot be read: an error, so the outcome is planned again.
	d.remotes = fakeRemoteNames{err: errors.New("store unavailable")}
	if got, err := d.DescribeJob(t.Context(), owner, "rclone_transfer", def("COPY", on(photos), on(b2)), nil); err == nil {
		t.Errorf("DescribeJob with failing names = %q, want an error", got.Text)
	}
}
