package api

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
)

// addCrypt adds an encrypted remote of owner that stores its data at path
// on wrapped.
func (e *testEnv) addCrypt(owner domain.Actor, name string, wrapped domain.RemoteID, path string) domain.RemoteID {
	e.t.Helper()
	id := e.addRemote(owner, name, "crypt")
	e.remotes.setInfo(id, remotes.Info{
		Wrapped: remotes.Wrapped{RemoteID: wrapped, Path: path}, Capabilities: backend.Capabilities{Wrapper: true, NoChecksum: true},
	})
	return id
}

func checksumTransfer(src, dst *syncrv1.JobLocation) *syncrv1.JobDefinition {
	return &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_RcloneTransfer{RcloneTransfer: &syncrv1.RcloneTransferDefinition{
		Mode: syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_COPY, Source: src, Destination: dst,
		Options: &syncrv1.RcloneTransferOptions{Checksum: true},
	}}}
}

func bisyncOf(p1, p2 *syncrv1.JobLocation, compare ...syncrv1.BisyncCompare) *syncrv1.JobDefinition {
	b := &syncrv1.RcloneBisyncDefinition{Path1: p1, Path2: p2}
	if len(compare) > 0 {
		b.Options = &syncrv1.RcloneBisyncOptions{Compare: compare}
	}
	return &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_RcloneBisync{RcloneBisync: b}}
}

func onRemote(id domain.RemoteID, path string) *syncrv1.JobLocation {
	return &syncrv1.JobLocation{RemoteId: id.String(), Path: path}
}

// TestRunJobRemoteChecks: RunJob checks what the remotes are (a remote
// that keeps no checksums, an encrypted remote's folder) and stores the
// concurrency keys computed through them; a remote that is not the
// caller's is still "remote not found".
func TestRunJobRemoteChecks(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	bucket := e.addRemote(testUser, "Bucket", "s3")
	vault := e.addCrypt(testUser, "Vault", bucket, "vault")
	local := &syncrv1.JobLocation{Path: "/srv/docs"}
	for _, tt := range []struct {
		name  string
		def   *syncrv1.JobDefinition
		field string
		msg   string
	}{
		{"checksum from crypt", checksumTransfer(onRemote(vault, "docs"), local),
			"definition.rclone_transfer.options.checksum", "is not available: a remote that keeps no checksums"},
		{"checksum to crypt", checksumTransfer(local, onRemote(vault, "docs")),
			"definition.rclone_transfer.options.checksum", "is not available: a remote that keeps no checksums"},
		{"bisync comparing checksums", bisyncOf(local, onRemote(vault, ""), syncrv1.BisyncCompare_BISYNC_COMPARE_SIZE, syncrv1.BisyncCompare_BISYNC_COMPARE_CHECKSUM),
			"definition.rclone_bisync.options.compare[1]", "is not available: a remote that keeps no checksums"},
		{"bisync sides nested through the wrapper", bisyncOf(onRemote(vault, ""), onRemote(bucket, "vault/x")),
			"definition.rclone_bisync.path2", "must differ from path1, and neither may be inside the other"},
	} {
		_, err := e.runAs(testUser, tt.def)
		wantCode(t, err, connect.CodeInvalidArgument)
		if v := fieldViolations(t, err); v[tt.field] != tt.msg {
			t.Errorf("%s: violations = %v, want %s %q", tt.name, v, tt.field, tt.msg)
		}
	}
	// Checksums between remotes that keep them are fine.
	if _, err := e.runAs(testUser, checksumTransfer(onRemote(bucket, "a"), local)); err != nil {
		t.Errorf("RunJob(checksum on s3) = %v", err)
	}

	// A crypt side holds the key of the folder its data is stored in.
	j, err := e.runAs(testUser, bisyncOf(local, onRemote(vault, "docs")))
	if err != nil {
		t.Fatal(err)
	}
	stored, err := e.jsvc.Get(t.Context(), testUser, e.jobID(j))
	if err != nil {
		t.Fatal(err)
	}
	reg, err := jobkind.NewRegistry(jobkind.Builtin()...)
	if err != nil {
		t.Fatal(err)
	}
	_, plain, err := reg.Check(bisyncOf(local, onRemote(bucket, "vault/docs")))
	if err != nil || !slices.Equal(stored.ConcurrencyKeys, plain.ConcurrencyKeys) {
		t.Errorf("stored keys %q, want those of the wrapped folder %q (%v)", stored.ConcurrencyKeys, plain.ConcurrencyKeys, err)
	}

	// Not the caller's remote: the same error as before.
	admins := e.addRemote(testAdmin, "Admin's", "s3")
	_, err = e.runAs(testUser, checksumTransfer(onRemote(admins, ""), local))
	wantCode(t, err, connect.CodeNotFound)
	if err == nil || !strings.Contains(err.Error(), "remote not found") {
		t.Errorf("RunJob(foreign remote) = %v", err)
	}
	// A failing lookup fails the call, without details.
	e.remotes.mu.Lock()
	e.remotes.infosErr = errors.New("db exploded: password=hunter2")
	e.remotes.mu.Unlock()
	_, err = e.runAs(testUser, checksumTransfer(onRemote(bucket, "a"), local))
	wantCode(t, err, connect.CodeInternal)
	if err != nil && strings.Contains(err.Error(), "hunter2") {
		t.Errorf("internal error leaks: %v", err)
	}
}

// TestScheduleRemoteChecks: schedules refuse what RunJob refuses about the
// remotes, on the same fields.
func TestScheduleRemoteChecks(t *testing.T) {
	t.Parallel()
	e := newScheduleEnv(t)
	bucket := e.addRemote(testUser, "Bucket", "s3")
	vault := e.addCrypt(testUser, "Vault", bucket, "vault")
	in := e.scheduleInput("Encrypted copy")
	in.Definition = checksumTransfer(&syncrv1.JobLocation{Path: "/srv/docs"}, onRemote(vault, "docs"))
	c := e.schedulesFor(testUser)
	_, err := c.CreateSchedule(t.Context(), connect.NewRequest(&syncrv1.CreateScheduleRequest{Input: in}))
	wantCode(t, err, connect.CodeInvalidArgument)
	if v := fieldViolations(t, err); v["definition.rclone_transfer.options.checksum"] != "is not available: a remote that keeps no checksums" {
		t.Errorf("CreateSchedule(checksum on crypt) violations = %v", v)
	}
	in.Definition.GetRcloneTransfer().Options = nil
	created, err := c.CreateSchedule(t.Context(), connect.NewRequest(&syncrv1.CreateScheduleRequest{Input: in}))
	if err != nil {
		t.Fatal(err)
	}
	in.Definition = bisyncOf(&syncrv1.JobLocation{Path: "/srv/docs"}, onRemote(vault, ""), syncrv1.BisyncCompare_BISYNC_COMPARE_CHECKSUM)
	_, err = c.UpdateSchedule(t.Context(), connect.NewRequest(&syncrv1.UpdateScheduleRequest{
		ScheduleId: created.Msg.GetSchedule().GetId(), Version: created.Msg.GetSchedule().GetVersion(), Input: in,
	}))
	wantCode(t, err, connect.CodeInvalidArgument)
	if v := fieldViolations(t, err); v["definition.rclone_bisync.options.compare[0]"] != "is not available: a remote that keeps no checksums" {
		t.Errorf("UpdateSchedule(bisync checksum on crypt) violations = %v", v)
	}
	// Another user's remote: "remote not found", as before.
	admins := e.addRemote(testAdmin, "Admin's", "s3")
	in.Definition = checksumTransfer(onRemote(admins, ""), &syncrv1.JobLocation{Path: "/srv/docs"})
	_, err = c.CreateSchedule(t.Context(), connect.NewRequest(&syncrv1.CreateScheduleRequest{Input: in}))
	wantCode(t, err, connect.CodeNotFound)
}
