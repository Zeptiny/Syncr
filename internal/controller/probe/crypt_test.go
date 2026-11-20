package probe_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/controller/probe"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/link"
)

// crypt adds an encrypted remote that stores its data at path on the
// remote wrapped.
func (f *fakeRemotes) crypt(wrapped domain.RemoteID, path string) domain.RemoteID {
	return f.put(remotes.Resolved{Name: "Vault", Type: "crypt", Rclone: backend.Rclone{
		Backend: "crypt", Options: map[string]string{"filename_encryption": "standard"},
		Secrets: map[string]string{"password": remoteSecret}, ObscureKeys: []string{"password"},
	}, Wrapped: remotes.Wrapped{RemoteID: wrapped, Path: path}, Capabilities: backend.Capabilities{Wrapper: true, NoChecksum: true}})
}

// outdatedInfo is a current worker's info without the crypt feature.
func outdatedInfo() workers.Info {
	info := current()
	info.Features = slices.DeleteFunc(slices.Clone(info.Features), func(f string) bool { return f == link.FeatureRcloneCrypt })
	return info
}

// TestCryptProbesPickByFeature: a probe on an encrypted remote goes only
// to a worker that reports link.FeatureRcloneCrypt; with none, the error
// says the workers must be upgraded, and a chosen worker without it is
// refused as outdated. Other remotes still use any worker.
func TestCryptProbesPickByFeature(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.transport.answer = listing("a")
	bucket := e.remotes.s3("bucket")
	vault := e.remotes.crypt(bucket, "vault")
	old := e.workers.add("aaa-old", domain.WorkerOnline, outdatedInfo())
	ctx := context.Background()

	// Only an outdated worker: the probe is not sent.
	_, err := e.svc.ListRemoteFiles(ctx, alice, vault, "", domain.WorkerID{}, false)
	if !errors.Is(err, probe.ErrNoWorker) || !strings.Contains(err.Error(), `no worker can use these remotes yet (they need the feature "rclone.crypt"); the workers must be upgraded`) {
		t.Errorf("ListRemoteFiles(crypt, outdated workers) = %v", err)
	}
	if e.transport.count() != 0 {
		t.Fatal("a probe was sent to an outdated worker")
	}
	// The remote test reports it in its response.
	resp, err := e.svc.TestRemote(ctx, alice, vault, nil, "", domain.WorkerID{})
	if err != nil || resp.GetOk() || resp.GetErrorCode() != "no_worker" || !strings.Contains(resp.GetError(), "must be upgraded") {
		t.Errorf("TestRemote(crypt, outdated workers) = (%v, %v)", resp, err)
	}
	// A plain remote still uses the outdated worker.
	if _, err := e.svc.ListRemoteFiles(ctx, alice, bucket, "", domain.WorkerID{}, false); err != nil || e.transport.last(t).worker != old {
		t.Errorf("ListRemoteFiles(s3) = %v", err)
	}

	// A current worker is picked, even busier and later by name.
	nas := e.workers.add("zzz-nas", domain.WorkerOnline, current())
	e.jobs.mu.Lock()
	e.jobs.unfinished[nas] = 5
	e.jobs.mu.Unlock()
	if _, err := e.svc.ListRemoteFiles(ctx, alice, vault, "docs", domain.WorkerID{}, false); err != nil {
		t.Fatal(err)
	}
	sent := e.transport.last(t)
	defs := sent.probe.GetSpec().GetRemotes()
	if sent.worker != nas || len(defs) != 2 || defs[0].GetBackendType() != "crypt" ||
		defs[0].GetWrapped().GetRemote() != "r2" || defs[0].GetWrapped().GetPath() != "bucket/vault" ||
		sent.probe.GetSpec().GetListRemote().GetLocation().GetPath() != "docs" {
		t.Errorf("sent to %s: %v", sent.worker, sent.probe.GetSpec())
	}
	if _, err := e.svc.MeasureRemoteFolder(ctx, alice, vault, "docs", domain.WorkerID{}); err == nil && e.transport.last(t).worker != nas {
		t.Errorf("MeasureRemoteFolder(crypt) went to %s", e.transport.last(t).worker)
	}

	// A chosen worker without the feature is refused as outdated.
	before := e.transport.count()
	_, err = e.svc.ListRemoteFiles(ctx, alice, vault, "", old, false)
	if !errors.Is(err, domain.ErrFailedPrecondition) || !strings.Contains(err.Error(), `worker "aaa-old"`) || !strings.Contains(err.Error(), "upgrade it") {
		t.Errorf("ListRemoteFiles(crypt, chosen outdated worker) = %v", err)
	}
	if _, err := e.svc.RemoteUsage(ctx, alice, vault, old); !errors.Is(err, domain.ErrFailedPrecondition) {
		t.Errorf("RemoteUsage(crypt, chosen outdated worker) = %v", err)
	}
	if e.transport.count() != before {
		t.Error("a probe was sent to the chosen outdated worker")
	}
	e.noSecrets()
}

func TestPickFor(t *testing.T) {
	t.Parallel()
	feats := []string{link.FeatureProbe, link.FeatureRcloneCrypt}
	ops := []string{"list_remote"}
	cand := func(name string, st domain.WorkerStatus, features []string) probe.Candidate {
		return probe.Candidate{ID: domain.NewWorkerID(), Name: name, Status: st, Features: features, ProbeOps: ops}
	}
	old := cand("old", domain.WorkerOnline, []string{link.FeatureProbe})
	cur := cand("cur", domain.WorkerOnline, feats)
	offCur := cand("off", domain.WorkerOffline, feats)
	need := []string{link.FeatureRcloneCrypt}
	if c, err := probe.PickFor([]probe.Candidate{old, cur}, "list_remote", need); err != nil || c.ID != cur.ID {
		t.Errorf("PickFor = (%v, %v), want the current worker", c.Name, err)
	}
	if c, err := probe.PickFor([]probe.Candidate{old}, "list_remote", nil); err != nil || c.ID != old.ID {
		t.Errorf("PickFor(no features) = (%v, %v)", c.Name, err)
	}
	for name, tt := range map[string]struct {
		cands []probe.Candidate
		msg   string
	}{
		"only outdated":              {[]probe.Candidate{old}, "the workers must be upgraded"},
		"the current one is offline": {[]probe.Candidate{old, offCur}, "are offline, and the online ones must be upgraded"},
		"none can probe at all":      {[]probe.Candidate{cand("x", domain.WorkerOnline, nil)}, "upgrade a worker"},
		"none":                       {nil, "there are no workers"},
	} {
		_, err := probe.PickFor(tt.cands, "list_remote", need)
		if !errors.Is(err, probe.ErrNoWorker) || !strings.Contains(err.Error(), tt.msg) {
			t.Errorf("%s: PickFor = %v, want %q", name, err, tt.msg)
		}
	}
	if err := probe.CheckExplicitFor(old, true, "list_remote", need); !errors.Is(err, domain.ErrFailedPrecondition) {
		t.Errorf("CheckExplicitFor(outdated) = %v", err)
	}
	if err := probe.CheckExplicitFor(cur, true, "list_remote", need); err != nil {
		t.Errorf("CheckExplicitFor(current) = %v", err)
	}
}
