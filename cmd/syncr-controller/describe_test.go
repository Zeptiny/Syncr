package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
	"github.com/Zeptiny/Syncr/internal/notify"
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

	// Every transfer carries its kind's label, and no details.
	if got, err := d.DescribeJob(t.Context(), owner, "rclone_transfer", def("COPY", on(photos), on(b2)), nil); err != nil || got.KindLabel != "Transfer" || len(got.Details) != 0 {
		t.Errorf("a transfer = %+v (%v), want the label Transfer and no details", got, err)
	}

	// The names cannot be read: an error, so the outcome is planned again.
	d.remotes = fakeRemoteNames{err: errors.New("store unavailable")}
	if got, err := d.DescribeJob(t.Context(), owner, "rclone_transfer", def("COPY", on(photos), on(b2)), nil); err == nil {
		t.Errorf("DescribeJob with failing names = %q, want an error", got.Text)
	}
}

// fakeRepositoryNames answers Names from refs, or fails with err.
type fakeRepositoryNames struct {
	refs map[domain.ResticRepoID]restic.Ref
	err  error
}

func (f fakeRepositoryNames) Names(_ context.Context, ids []domain.ResticRepoID) (map[domain.ResticRepoID]restic.Ref, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := map[domain.ResticRepoID]restic.Ref{}
	for _, id := range ids {
		if r, ok := f.refs[id]; ok {
			out[id] = r
		}
	}
	return out, nil
}

// TestJobDescriberRestic: a restic job names the owner's repository (a
// deleted one included, never another user's), carries its kind's label
// and, once it has a result, the details the job's page shows.
func TestJobDescriberRestic(t *testing.T) {
	t.Parallel()
	kinds, err := jobkind.NewRegistry(jobkind.Builtin()...)
	if err != nil {
		t.Fatal(err)
	}
	owner, other := domain.NewUserID(), domain.NewUserID()
	vault, gone, theirs := domain.NewResticRepoID(), domain.NewResticRepoID(), domain.NewResticRepoID()
	d := jobDescriber{kinds: kinds, remotes: fakeRemoteNames{}, repos: fakeRepositoryNames{refs: map[domain.ResticRepoID]restic.Ref{
		vault:  {ID: vault, OwnerID: owner, Name: "Photos vault"},
		gone:   {ID: gone, OwnerID: owner, Name: "Retired NAS", Deleted: true},
		theirs: {ID: theirs, OwnerID: other, Name: "Bob's secret vault"},
	}}}
	backup := func(id domain.ResticRepoID) []byte {
		return []byte(`{"restic_backup":{"repository_id":"` + id.String() + `","paths":["/srv/photos"]}}`)
	}
	output, err := proto.Marshal(&syncrv1.JobOutput{Output: &syncrv1.JobOutput_ResticBackup{ResticBackup: &syncrv1.ResticBackupOutput{
		SnapshotId: "1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f809", FilesNew: 12, FilesChanged: 3, FilesUnmodified: 1204,
		DataAddedBytes: 1 << 30, TotalFilesProcessed: 1219, TotalBytesProcessed: 48 << 30,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, kind string
		definition []byte
		output     []byte
		want       string
		label      string
		details    bool
	}{
		{"a backup", "restic_backup", backup(vault), output, "Back up /srv/photos → Photos vault", "Backup", true},
		{"without a result", "restic_backup", backup(vault), nil, "Back up /srv/photos → Photos vault", "Backup", false},
		{"a deleted repository", "restic_backup", backup(gone), nil, "Back up /srv/photos → Retired NAS", "Backup", false},
		{"a check", "restic_check", []byte(`{"restic_check":{"repository_id":"` + vault.String() + `"}}`), nil, "Check Photos vault", "Check", false},
		{"a data deletion", "restic_delete_data", []byte(`{"restic_delete_data":{"repository_id":"` + gone.String() + `"}}`), nil, "Retired NAS", "Delete data", false},
	}
	for _, tt := range tests {
		got, err := d.DescribeJob(t.Context(), owner, tt.kind, tt.definition, tt.output)
		if err != nil || !strings.Contains(got.Text, tt.want) || got.KindLabel != tt.label || (len(got.Details) > 0) != tt.details {
			t.Errorf("%s: DescribeJob = %+v (%v), want %q, label %q, details %v", tt.name, got, err, tt.want, tt.label, tt.details)
		}
		if tt.details {
			if i := slices.IndexFunc(got.Details, func(f notify.Fact) bool { return f.Label == "Snapshot" }); i < 0 || !strings.HasPrefix(got.Details[i].Value, "1a2b3c4d") {
				t.Errorf("%s: details = %v, want the snapshot", tt.name, got.Details)
			}
		}
	}

	// Another user's repository is never named.
	got, err := d.DescribeJob(t.Context(), owner, "restic_backup", backup(theirs), nil)
	if err != nil || strings.Contains(got.Text, "Bob") || got.Text == "" {
		t.Errorf("another user's repository: %+v (%v)", got, err)
	}
	// Without the repositories (nil), the job is still described.
	if got, err := (jobDescriber{kinds: kinds, remotes: fakeRemoteNames{}}).DescribeJob(t.Context(), owner, "restic_backup", backup(vault), nil); err != nil || got.KindLabel != "Backup" || got.Text == "" {
		t.Errorf("without repositories: %+v (%v)", got, err)
	}
	// The names cannot be read: an error, so the outcome is planned again.
	d.repos = fakeRepositoryNames{err: errors.New("store unavailable")}
	if got, err := d.DescribeJob(t.Context(), owner, "restic_backup", backup(vault), nil); err == nil {
		t.Errorf("DescribeJob with failing names = %q, want an error", got.Text)
	}
}
