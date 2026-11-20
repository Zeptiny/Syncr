package scheduler_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
)

// addCrypt makes a live encrypted remote of alice that stores its data at
// path on wrapped.
func (e *env) addCrypt(name string, wrapped domain.RemoteID, path string) domain.RemoteID {
	e.t.Helper()
	id := e.addRemote(name)
	e.remotes.setInfo(id, remotes.Info{Type: "crypt", Wrapped: remotes.Wrapped{RemoteID: wrapped, Path: path},
		Capabilities: backend.Capabilities{Wrapper: true, NoChecksum: true}})
	return id
}

// TestLaunchChecksWithRemotes: a launch checks the stored definition with
// what its remotes are (a definition stored before a check existed, or
// through another path, cannot run): a checksum comparison with a remote
// that keeps none fails the run and pauses the schedule; a two-way sync
// side on an encrypted remote holds the key of the folder its data is in;
// a lookup that fails is retried.
func TestLaunchChecksWithRemotes(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	w := e.addWorker(worker{name: "nas", online: true, kinds: []string{"rclone_transfer", "rclone_bisync"}, features: []string{"rclone.bisync"}})
	bucket := e.addRemote("bucket")
	vault := e.addCrypt("vault", bucket, "vault")

	in := e.input("checksum", schedules.Target{WorkerID: w})
	in.Definition = []byte(fmt.Sprintf(`{"rclone_transfer":{"mode":"RCLONE_TRANSFER_MODE_COPY","source":{"path":"/data"},`+
		`"destination":{"remote_id":%q},"options":{"checksum":true}}}`, vault))
	in.RemoteIDs = []domain.RemoteID{vault}
	sc := e.create(in)

	pair := e.input("pair", schedules.Target{WorkerID: w})
	pair.Kind = "rclone_bisync"
	pair.Definition = []byte(fmt.Sprintf(`{"rclone_bisync":{"path1":{"path":"/data/docs"},"path2":{"remote_id":%q,"path":"docs"}}}`, vault))
	pair.RemoteIDs = []domain.RemoteID{vault}
	bisync := e.create(pair)

	// The lookup fails first: nothing is decided, the pass reports it.
	e.clock.Set(t0.Add(time.Hour))
	e.remotes.failInfos(errors.New("store down"))
	s := e.newScheduler(nil, nil)
	e.heartbeat()
	if err := s.Tick(context.Background()); err == nil || !strings.Contains(err.Error(), "store down") {
		t.Errorf("Tick with a failing lookup = %v", err)
	}
	if js := e.jobsOf(sc.ID); len(js) != 0 {
		t.Fatalf("jobs = %v, want none yet", js)
	}
	e.remotes.failInfos(nil)
	e.tick(s)

	r := e.onlyRun(sc.ID)
	if r.Status != schedules.RunFailed || r.Reason != schedules.ReasonInvalidDefinition ||
		!strings.Contains(r.Message, "is not available: a remote that keeps no checksums") {
		t.Errorf("checksum run = %+v, want failed (invalid definition)", r)
	}
	if got := e.schedule(sc.ID); got.Enabled || got.PausedReason != schedules.ReasonInvalidDefinition {
		t.Errorf("schedule = %+v, want paused", got)
	}
	if js := e.jobsOf(sc.ID); len(js) != 0 {
		t.Errorf("jobs = %v, want none", js)
	}

	// The two-way sync launched, with the key of vault/docs on the bucket.
	js := e.jobsOf(bisync.ID)
	if len(js) != 1 {
		t.Fatalf("pair jobs = %d", len(js))
	}
	_, plain, err := e.kinds.Check(mustDefinition(t, fmt.Sprintf(`{"rclone_bisync":{"path1":{"path":"/data/docs"},"path2":{"remote_id":%q,"path":"vault/docs"}}}`, bucket)))
	if err != nil || !slices.Equal(js[0].ConcurrencyKeys, plain.ConcurrencyKeys) {
		t.Errorf("pair keys = %q, want those of the wrapped folder %q (%v)", js[0].ConcurrencyKeys, plain.ConcurrencyKeys, err)
	}
}

func mustDefinition(t *testing.T, raw string) *syncrv1.JobDefinition {
	t.Helper()
	def, err := jobkind.Unmarshal([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return def
}
