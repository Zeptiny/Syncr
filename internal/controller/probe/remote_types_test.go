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
	"github.com/Zeptiny/Syncr/internal/backend/builtin"
	"github.com/Zeptiny/Syncr/internal/controller/probe"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/link"
)

// typed adds a remote of the built-in type id, mapped from the type's
// first example as the remotes service maps a stored remote.
func (f *fakeRemotes) typed(t *testing.T, id string, mutate ...func(*remotes.Resolved)) domain.RemoteID {
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
	r := remotes.Resolved{Name: "My " + id, Type: id, Rclone: rc, Capabilities: typ.Capabilities()}
	for _, m := range mutate {
		m(&r)
	}
	return f.put(r)
}

// infoWithout is a current worker's info without the features drop.
func infoWithout(drop ...string) workers.Info {
	info := current()
	info.Features = slices.DeleteFunc(slices.Clone(info.Features), func(f string) bool { return slices.Contains(drop, f) })
	return info
}

// TestRemoteTypesProbesPickByFeature: a probe on a remote of a type of
// 2.3, directly, as a union member or under a crypt remote, goes only to a
// worker that reports the type's link feature; with none, the probe is not
// sent and the error says the workers must be upgraded, and a chosen
// worker without it is refused as outdated.
func TestRemoteTypesProbesPickByFeature(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	type tcase struct {
		name    string
		add     func(*fakeRemotes) domain.RemoteID
		feature string
	}
	var cases []tcase
	for id, feature := range map[string]string{
		"smb": link.FeatureRcloneSMB, "ftp": link.FeatureRcloneFTP, "azureblob": link.FeatureRcloneAzureBlob, "gcs": link.FeatureRcloneGCS,
	} {
		cases = append(cases, tcase{id, func(f *fakeRemotes) domain.RemoteID { return f.typed(t, id) }, feature})
	}
	cases = append(cases,
		tcase{"union with an SMB member", func(f *fakeRemotes) domain.RemoteID {
			members := []domain.RemoteID{f.s3("bucket"), f.typed(t, "smb")}
			return f.typed(t, "union", func(r *remotes.Resolved) {
				for _, m := range members {
					r.Members = append(r.Members, remotes.Member{RemoteID: m, Mode: backend.UnionReadWrite})
				}
			})
		}, link.FeatureRcloneSMB},
		tcase{"crypt over FTP", func(f *fakeRemotes) domain.RemoteID {
			ftp := f.typed(t, "ftp")
			return f.typed(t, "crypt", func(r *remotes.Resolved) { r.Wrapped = remotes.Wrapped{RemoteID: ftp, Path: "vault"} })
		}, link.FeatureRcloneFTP},
	)
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			e.transport.answer = listing("a")
			target := tt.add(e.remotes)
			old := e.workers.add("aaa-old", domain.WorkerOnline, infoWithout(tt.feature))

			_, err := e.svc.ListRemoteFiles(ctx, alice, target, "", domain.WorkerID{}, false)
			if !errors.Is(err, probe.ErrNoWorker) || !strings.Contains(err.Error(), `"`+tt.feature+`"`) ||
				!strings.Contains(err.Error(), "must be upgraded") {
				t.Errorf("ListRemoteFiles(outdated workers) = %v", err)
			}
			if e.transport.count() != 0 {
				t.Fatal("a probe was sent to an outdated worker")
			}

			nas := e.workers.add("zzz-nas", domain.WorkerOnline, current())
			if _, err := e.svc.ListRemoteFiles(ctx, alice, target, "", domain.WorkerID{}, false); err != nil {
				t.Fatal(err)
			}
			if sent := e.transport.last(t); sent.worker != nas {
				t.Errorf("sent to %s, want the current worker", sent.worker)
			}

			before := e.transport.count()
			_, err = e.svc.ListRemoteFiles(ctx, alice, target, "", old, false)
			if !errors.Is(err, domain.ErrFailedPrecondition) || !strings.Contains(err.Error(), "upgrade it") || e.transport.count() != before {
				t.Errorf("ListRemoteFiles(chosen outdated worker) = %v", err)
			}
		})
	}
}
