package dispatch_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/controller/dispatch"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/link"
)

// cryptPassword is the crypt remote's test password.
const cryptPassword = "crypt-password-value"

func testCryptRemote() domain.RemoteID {
	id, err := domain.ParseRemoteID("00000000-0000-7000-8000-00000000c0c0")
	if err != nil {
		panic(err)
	}
	return id
}

// addCrypt adds the test crypt remote, which stores its data in the folder
// "vault" of the test remote, to f.
func (f *fakeRemotes) addCrypt() domain.RemoteID {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := testCryptRemote()
	f.resolved[id] = remotes.Resolved{
		ID: id, Name: "Vault", Type: "crypt",
		Rclone: backend.Rclone{Backend: "crypt", Options: map[string]string{"filename_encryption": "standard"},
			Secrets: map[string]string{"password": cryptPassword}, ObscureKeys: []string{"password"}},
		Wrapped:      remotes.Wrapped{RemoteID: testRemote(), Path: "vault"},
		Capabilities: backend.Capabilities{Wrapper: true, NoChecksum: true},
	}
	f.owners[id] = testOwner
	return id
}

// cryptDefinition copies the worker's path into the crypt remote, with
// checksums compared if checksum.
func cryptDefinition(checksum bool) *syncrv1.JobDefinition {
	def := testDefinition("/data")
	def.GetRcloneTransfer().Destination = &syncrv1.JobLocation{RemoteId: testCryptRemote().String(), Path: "docs"}
	if checksum {
		def.GetRcloneTransfer().Options = &syncrv1.RcloneTransferOptions{Checksum: true}
	}
	return def
}

func withoutCrypt() []string {
	return slices.DeleteFunc(link.Features(), func(f string) bool { return f == link.FeatureRcloneCrypt })
}

// TestSpecsCryptRemote: a spec with a crypt remote defines it with its
// wrapped location, after it the remote it stores its data in; a worker
// without link.FeatureRcloneCrypt gets a *RemoteFeatureError, and a
// checksum comparison with the crypt remote is refused.
func TestSpecsCryptRemote(t *testing.T) {
	t.Parallel()
	rs := newFakeRemotes()
	rs.addCrypt()
	specs, err := dispatch.NewSpecs(rs, &fakeRepos{}, kinds(t))
	if err != nil {
		t.Fatal(err)
	}
	j := storedJob(t, cryptDefinition(false))
	spec, err := specs.Build(context.Background(), j, workers.Info{Features: link.Features()})
	if err != nil {
		t.Fatal(err)
	}
	want := []*syncrv1.RemoteDefinition{
		{Alias: "r1", BackendType: "crypt", Options: map[string]string{"filename_encryption": "standard"},
			Secrets: map[string]string{"password": cryptPassword}, ObscureKeys: []string{"password"},
			Wrapped: &syncrv1.Location{Remote: "r2", Path: "bucket/vault"}},
		{Alias: "r2", BackendType: "s3", Options: map[string]string{"provider": "Minio"}, Secrets: map[string]string{"secret_access_key": testSecret}},
	}
	if len(spec.GetRemotes()) != 2 || !proto.Equal(spec.GetRemotes()[0], want[0]) || !proto.Equal(spec.GetRemotes()[1], want[1]) ||
		!proto.Equal(spec.GetRcloneTransfer().GetDestination(), &syncrv1.Location{Remote: "r1", Path: "docs"}) {
		t.Errorf("Build = %v", spec)
	}

	_, err = specs.Build(context.Background(), j, workers.Info{Features: withoutCrypt()})
	var rf *dispatch.RemoteFeatureError
	if !errors.As(err, &rf) || rf.Backend != "crypt" || rf.Feature != link.FeatureRcloneCrypt || !errors.Is(err, dispatch.ErrWorkerOutdated) {
		t.Errorf("Build on a worker without %s = %v, want a RemoteFeatureError", link.FeatureRcloneCrypt, err)
	}
	if err != nil && strings.Contains(err.Error(), cryptPassword) {
		t.Errorf("the error contains the password: %v", err)
	}
	// Remotes that need nothing still go to that worker.
	if _, err := specs.Build(context.Background(), storedJob(t, testDefinition("/data")), workers.Info{Features: withoutCrypt()}); err != nil {
		t.Errorf("Build(plain remote) on a worker without %s = %v", link.FeatureRcloneCrypt, err)
	}

	_, err = specs.Build(context.Background(), storedJob(t, cryptDefinition(true)), workers.Info{Features: link.Features()})
	var verr *domain.ValidationError
	if !errors.As(err, &verr) || verr.Fields[0].Field != "definition.rclone_transfer.options.checksum" ||
		verr.Fields[0].Message != "is not available: a remote that keeps no checksums" {
		t.Errorf("Build(checksum on crypt) = %v", err)
	}
}

// TestCryptRemoteNeedsTheFeature: the dispatcher fails a job with a crypt
// remote on a worker without link.FeatureRcloneCrypt with worker_outdated,
// naming the remote type, and sends it to a worker with the feature.
func TestCryptRemoteNeedsTheFeature(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id := e.remotes.addCrypt()
	vault := remotes.Remote{ID: id, OwnerID: testOwner, Name: "Vault", Type: "crypt", Version: 1,
		Wrapped: remotes.Wrapped{RemoteID: testRemote(), Path: "vault"}}
	if err := e.store.CreateRemote(context.Background(), vault); err != nil {
		t.Fatal(err)
	}
	old := e.newFakeWorker(e.enroll("old"))
	old.features = withoutCrypt()
	old.restart()
	nas := e.newProbeWorker(e.enroll("nas"))
	for _, w := range []*fakeWorker{old, nas} {
		w.report()
	}

	outdated := e.runDef(old.id, cryptDefinition(false))
	if got := started(old.poll()); len(got) != 0 {
		t.Errorf("the worker without the feature was sent %v", got)
	}
	if got := e.job(outdated.ID); got.Status != domain.JobFailed || got.Error.Code != jobs.CodeWorkerOutdated ||
		got.Error.Message != `worker "old" must be upgraded to run jobs on remotes of type "crypt"` {
		t.Errorf("outdated = %s %q %q", got.Status, got.Error.Code, got.Error.Message)
	}
	fine := e.runDef(nas.id, cryptDefinition(false))
	if got := started(nas.poll()); !slices.Equal(got, []string{fine.ID.String()}) {
		t.Fatalf("the worker with the feature was sent %v, want %v", got, fine.ID)
	}
	if d := nas.specs[fine.ID.String()].GetRemotes(); len(d) != 2 || d[0].GetWrapped().GetRemote() != "r2" {
		t.Errorf("sent remotes = %v", d)
	}
}
