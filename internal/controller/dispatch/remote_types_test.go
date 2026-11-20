package dispatch_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/backend/builtin"
	"github.com/Zeptiny/Syncr/internal/controller/dispatch"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/link"
)

// remoteTypeFeatures are the link features of the remote types of 2.3, by
// type ID.
func remoteTypeFeatures() map[string]string {
	return map[string]string{
		"smb": link.FeatureRcloneSMB, "ftp": link.FeatureRcloneFTP,
		"azureblob": link.FeatureRcloneAzureBlob, "gcs": link.FeatureRcloneGCS,
	}
}

// without returns every link feature but drop.
func without(drop ...string) []string {
	return slices.DeleteFunc(link.Features(), func(f string) bool { return slices.Contains(drop, f) })
}

// addTyped adds a remote of the built-in type id to f, mapped from the
// type's first example as the remotes service maps a stored remote, with
// the type's capabilities, and returns its ID.
func (f *fakeRemotes) addTyped(t *testing.T, id string, mutate ...func(*remotes.Resolved)) domain.RemoteID {
	t.Helper()
	i := slices.IndexFunc(builtin.Types(), func(typ backend.Type) bool { return typ.ID() == id })
	if i < 0 {
		t.Fatalf("no built-in type %q", id)
	}
	typ := builtin.Types()[i]
	ex, ok := typ.(backend.Exampler)
	if !ok {
		t.Fatalf("type %q has no examples", id)
	}
	rc, err := typ.ToRclone(ex.Examples()[0])
	if err != nil {
		t.Fatal(err)
	}
	r := remotes.Resolved{ID: domain.NewRemoteID(), Name: "My " + id, Type: id, Rclone: rc, Capabilities: typ.Capabilities()}
	for _, m := range mutate {
		m(&r)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolved[r.ID] = r
	f.owners[r.ID] = testOwner
	return r.ID
}

// unionOf makes a union of members.
func unionOf(members ...domain.RemoteID) func(*remotes.Resolved) {
	return func(r *remotes.Resolved) {
		for _, m := range members {
			r.Members = append(r.Members, remotes.Member{RemoteID: m, Mode: backend.UnionReadWrite})
		}
	}
}

// copyTo copies the worker's /data into remote id, comparing checksums if
// checksum.
func copyTo(id domain.RemoteID, checksum bool) *syncrv1.JobDefinition {
	def := testDefinition("/data")
	def.GetRcloneTransfer().Destination = &syncrv1.JobLocation{RemoteId: id.String(), Path: "docs"}
	if checksum {
		def.GetRcloneTransfer().Options = &syncrv1.RcloneTransferOptions{Checksum: true}
	}
	return def
}

// TestSpecsRemoteTypesNeedTheirFeature: a spec with a remote of a type of
// 2.3, directly, as a union member or under a crypt remote, goes to a
// worker that reports the type's link feature, and is a
// *RemoteFeatureError (worker_outdated) for one that does not.
func TestSpecsRemoteTypesNeedTheirFeature(t *testing.T) {
	t.Parallel()
	rs := newFakeRemotes()
	specs, err := dispatch.NewSpecs(rs, &fakeRepos{}, kinds(t))
	if err != nil {
		t.Fatal(err)
	}
	type tcase struct {
		name    string
		target  domain.RemoteID
		backend string // the backend whose feature is missing
	}
	var cases []tcase
	for id := range remoteTypeFeatures() {
		cases = append(cases, tcase{id, rs.addTyped(t, id), id})
	}
	smb := rs.addTyped(t, "smb")
	cases = append(cases, tcase{"union with an SMB member", rs.addTyped(t, "union", unionOf(testRemote(), smb)), "smb"})
	ftp := rs.addTyped(t, "ftp")
	cases = append(cases, tcase{"crypt over FTP", rs.addTyped(t, "crypt", func(r *remotes.Resolved) {
		r.Wrapped = remotes.Wrapped{RemoteID: ftp, Path: "vault"}
	}), "ftp"})

	for _, tt := range cases {
		j := storedJob(t, copyTo(tt.target, false))
		feature := remoteTypeFeatures()[tt.backend]
		spec, err := specs.Build(context.Background(), j, workers.Info{Features: link.Features()})
		if err != nil {
			t.Errorf("%s: Build on a current worker = %v", tt.name, err)
			continue
		}
		if !slices.ContainsFunc(spec.GetRemotes(), func(d *syncrv1.RemoteDefinition) bool { return d.GetBackendType() == tt.backend }) {
			t.Errorf("%s: the spec defines no %s remote: %v", tt.name, tt.backend, spec.GetRemotes())
		}
		_, err = specs.Build(context.Background(), j, workers.Info{Features: without(feature)})
		var rf *dispatch.RemoteFeatureError
		if !errors.As(err, &rf) || rf.Backend != tt.backend || rf.Feature != feature || !errors.Is(err, dispatch.ErrWorkerOutdated) {
			t.Errorf("%s: Build on a worker without %s = %v, want a RemoteFeatureError", tt.name, feature, err)
		}
	}
	// Remotes that need none of them still go to a worker without any.
	if _, err := specs.Build(context.Background(), storedJob(t, testDefinition("/data")),
		workers.Info{Features: without(slices.Collect(maps.Values(remoteTypeFeatures()))...)}); err != nil {
		t.Errorf("Build(s3) on a worker without the remote types' features = %v", err)
	}
}

// TestSpecsChecksumThroughUnion: the spec builder refuses a checksum
// comparison on a union with an FTP member (even when the resolved union
// does not say it keeps none: the job builder follows the members), with
// crypt's message; a union of S3 and B2 builds as before.
func TestSpecsChecksumThroughUnion(t *testing.T) {
	t.Parallel()
	rs := newFakeRemotes()
	specs, err := dispatch.NewSpecs(rs, &fakeRepos{}, kinds(t))
	if err != nil {
		t.Fatal(err)
	}
	withFTP := rs.addTyped(t, "union", unionOf(testRemote(), rs.addTyped(t, "ftp")))
	plain := rs.addTyped(t, "union", unionOf(testRemote(), rs.addTyped(t, "b2")))
	info := workers.Info{Features: link.Features()}

	_, err = specs.Build(context.Background(), storedJob(t, copyTo(withFTP, true)), info)
	var verr *domain.ValidationError
	if !errors.As(err, &verr) || verr.Fields[0].Field != "definition.rclone_transfer.options.checksum" ||
		verr.Fields[0].Message != "is not available: a remote that keeps no checksums" {
		t.Errorf("Build(checksum on a union with FTP) = %v", err)
	}
	if _, err := specs.Build(context.Background(), storedJob(t, copyTo(withFTP, false)), info); err != nil {
		t.Errorf("Build(union with FTP, no checksum) = %v", err)
	}
	spec, err := specs.Build(context.Background(), storedJob(t, copyTo(plain, true)), info)
	if err != nil || !spec.GetRcloneTransfer().GetOptions().GetChecksum() || len(spec.GetRemotes()) != 3 {
		t.Errorf("Build(checksum on a union of S3 and B2) = %v, %v", spec, err)
	}
}

// TestRemoteTypesNeedTheFeature: the dispatcher fails a job on a remote
// of each type of 2.3 on a worker without its feature with
// worker_outdated, naming the type, and sends it to a worker with it.
func TestRemoteTypesNeedTheFeature(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	old := e.newFakeWorker(e.enroll("old"))
	old.features = without(slices.Collect(maps.Values(remoteTypeFeatures()))...)
	old.restart()
	nas := e.newProbeWorker(e.enroll("nas"))
	for _, w := range []*fakeWorker{old, nas} {
		w.report()
	}
	for _, id := range slices.Sorted(maps.Keys(remoteTypeFeatures())) {
		rid := e.remotes.addTyped(t, id)
		if err := e.store.CreateRemote(context.Background(), remotes.Remote{ID: rid, OwnerID: testOwner, Name: "My " + id, Type: id, Version: 1}); err != nil {
			t.Fatal(err)
		}
		outdated := e.runDef(old.id, copyTo(rid, false))
		if got := started(old.poll()); len(got) != 0 {
			t.Errorf("%s: the worker without the feature was sent %v", id, got)
		}
		want := fmt.Sprintf(`worker "old" must be upgraded to run jobs on remotes of type %q`, id)
		if got := e.job(outdated.ID); got.Status != domain.JobFailed || got.Error.Code != jobs.CodeWorkerOutdated || got.Error.Message != want {
			t.Errorf("%s: outdated = %s %q %q", id, got.Status, got.Error.Code, got.Error.Message)
		}
		fine := e.runDef(nas.id, copyTo(rid, false))
		if got := started(nas.poll()); !slices.Equal(got, []string{fine.ID.String()}) {
			t.Fatalf("%s: the worker with the feature was sent %v, want %v", id, got, fine.ID)
		}
		if d := nas.specs[fine.ID.String()].GetRemotes(); len(d) != 1 || d[0].GetBackendType() != id {
			t.Errorf("%s: sent remotes = %v", id, d)
		}
	}
}
