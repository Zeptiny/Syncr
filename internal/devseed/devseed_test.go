package devseed_test

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"errors"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/adapters/broker"
	"github.com/Zeptiny/Syncr/internal/adapters/idgen"
	"github.com/Zeptiny/Syncr/internal/adapters/memstore"
	notifybuiltin "github.com/Zeptiny/Syncr/internal/adapters/notify/builtin"
	"github.com/Zeptiny/Syncr/internal/adapters/password"
	"github.com/Zeptiny/Syncr/internal/adapters/secrets"
	"github.com/Zeptiny/Syncr/internal/app/contacts"
	"github.com/Zeptiny/Syncr/internal/app/jobs"
	"github.com/Zeptiny/Syncr/internal/app/notifications"
	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/app/users"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/backend/builtin"
	"github.com/Zeptiny/Syncr/internal/devseed"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
	"github.com/Zeptiny/Syncr/internal/link"
	"github.com/Zeptiny/Syncr/internal/notify"
	"github.com/Zeptiny/Syncr/internal/platform/clock"
	"github.com/Zeptiny/Syncr/internal/platform/ratelimit"
)

var t0 = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

// env is a controller's services on memstore, as cmd/syncr-controller
// wires them for dev.seed.
type env struct {
	deps   devseed.Deps
	store  *memstore.Store
	clock  *clock.Fake
	broker *broker.Broker
	logs   *bytes.Buffer
}

type nopWaker struct{}

// refuse fails every request: the seed must never send.
type refuse struct{}

var errRefused = errors.New("the seed tried to send")

func (refuse) RoundTrip(*http.Request) (*http.Response, error) { return nil, errRefused }

func (nopWaker) Wake(domain.WorkerID) {}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{store: memstore.New(), clock: clock.NewFake(t0), broker: broker.New(), logs: &bytes.Buffer{}}
	log := slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	key := secrets.Key{ID: 1, Bytes: make([]byte, secrets.KeySize)}
	if _, err := crand.Read(key.Bytes); err != nil {
		t.Fatal(err)
	}
	ring, err := secrets.NewKeyring(key)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := backend.NewRegistry(builtin.Types()...)
	if err != nil {
		t.Fatal(err)
	}
	kinds, err := jobkind.NewRegistry(jobkind.Builtin()...)
	if err != nil {
		t.Fatal(err)
	}
	ids := &idgen.Sequential{}
	rs, err := remotes.New(remotes.Deps{Remotes: e.store, Cipher: ring, Clock: e.clock, IDs: ids, Backends: reg, Logger: log}, remotes.Config{})
	if err != nil {
		t.Fatal(err)
	}
	js, err := jobs.New(jobs.Deps{
		Jobs: e.store, Samples: e.store, Clock: e.clock, IDs: ids, Broker: e.broker,
		Notifier: jobs.NopNotifier{}, Waker: nopWaker{}, Remotes: rs, Logger: log,
	}, jobs.Config{})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := workers.New(workers.Deps{
		Workers: e.store, Tokens: e.store, Cipher: ring, Clock: e.clock, IDs: ids, Jobs: js, Random: crand.Reader, Logger: log,
	}, workers.Config{})
	if err != nil {
		t.Fatal(err)
	}
	hasher, err := password.New(password.Config{Params: password.Params{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}})
	if err != nil {
		t.Fatal(err)
	}
	lim := func() *ratelimit.Limiter {
		l, err := ratelimit.New(ratelimit.Config{Burst: 10, Every: time.Minute, MaxKeys: 100}, e.clock)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	us, err := users.New(users.Deps{
		Users: e.store, Sessions: e.store, Tokens: e.store, Hasher: hasher,
		Limiters: users.Limiters{ClientIP: lim(), Username: lim(), Setup: lim()},
		Clock:    e.clock, IDs: ids, Random: crand.Reader, Logger: log,
	}, users.Config{})
	if err != nil {
		t.Fatal(err)
	}
	channels, err := notify.NewRegistry(notifybuiltin.Channels(notifybuiltin.Deps{
		HTTP: &http.Client{Transport: refuse{}}, Dial: func(context.Context, string, string) (net.Conn, error) { return nil, errRefused },
	})...)
	if err != nil {
		t.Fatal(err)
	}
	channels = notify.DryRun(channels, log)
	cs, err := contacts.New(contacts.Deps{
		Contacts: e.store, Channels: channels, Cipher: ring, Limiter: lim(), Clock: e.clock, IDs: ids, Logger: log,
	}, contacts.Config{})
	if err != nil {
		t.Fatal(err)
	}
	e.deps = devseed.Deps{
		Users: us, Remotes: rs, Workers: ws, Jobs: js, Kinds: kinds, Contacts: cs, Channels: channels,
		Cipher: ring, Probes: devseed.NewProbes(e.clock),
		Store: e.store, Clock: e.clock, Logger: log,
	}
	return e
}

func (e *env) seed(t *testing.T) *devseed.Seed {
	t.Helper()
	s, err := devseed.Run(context.Background(), e.deps)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSeed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	s := e.seed(t)

	// Users: both demo logins work, setup is complete.
	if required, err := e.deps.Users.SetupRequired(ctx); err != nil || required {
		t.Errorf("SetupRequired = (%v, %v), want false", required, err)
	}
	for _, c := range [][2]string{{devseed.AdminUsername, devseed.AdminPassword}, {devseed.AliceUsername, devseed.AlicePassword}} {
		if _, _, err := e.deps.Users.Login(ctx, users.LoginParams{Username: c[0], Password: c[1], ClientIP: "127.0.0.1"}); err != nil {
			t.Errorf("Login(%s): %v", c[0], err)
		}
	}
	if !s.Admin.IsAdmin() || s.Alice.IsAdmin() || s.Alice.UserID.IsZero() {
		t.Errorf("Admin %+v, Alice %+v", s.Admin, s.Alice)
	}

	// Workers in every connection state.
	want := map[string]domain.WorkerStatus{
		"nas-01": domain.WorkerOnline, "office-pc": domain.WorkerOffline, "backup-host": domain.WorkerOnline, "laptop": domain.WorkerPending,
		"attic-nas": domain.WorkerOnline,
	}
	ws, err := e.deps.Workers.List(ctx, workers.ListFilter{})
	if err != nil || len(ws) != len(want) {
		t.Fatalf("workers = %v (%v)", ws, err)
	}
	for _, w := range ws {
		if got := e.deps.Workers.Status(w); got != want[w.Name] {
			t.Errorf("worker %s: status %s, want %s", w.Name, got, want[w.Name])
		}
		if w.Name == "backup-host" && (w.Mode != domain.WorkerPassive || w.URL == "") {
			t.Errorf("backup-host = %+v", w)
		}
		if w.Name == "office-pc" && (w.Info.OS != "windows" || !e.clock.Now().Add(-3*time.Hour).Equal(w.Link.LastSeen)) {
			t.Errorf("office-pc = %+v", w)
		}
	}

	// Remotes: the admin's of every type, alice's own; strictly scoped.
	adminRemotes, err := e.deps.Remotes.List(ctx, s.Admin, remotes.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]bool{}
	var names []string
	for _, r := range adminRemotes {
		types[r.Type] = true
		names = append(names, r.Name)
	}
	for _, n := range []string{"MinIO archive", "Storage box", "Photos", "B2 offsite", "Nextcloud", "Pooled disks"} {
		if !slices.Contains(names, n) {
			t.Errorf("admin remotes %v lack %q", names, n)
		}
	}
	for _, typ := range builtin.Types() {
		if !types[typ.ID()] {
			t.Errorf("no demo remote of type %s", typ.ID())
		}
	}
	if aliceRemotes, err := e.deps.Remotes.List(ctx, s.Alice, remotes.ListFilter{}); err != nil || len(aliceRemotes) != 2 {
		t.Errorf("alice's remotes = %v (%v)", aliceRemotes, err)
	}
	pool, err := e.deps.Remotes.Get(ctx, s.Admin, s.Remotes["Pooled disks"])
	if err != nil || pool.Type != "union" || len(pool.Members) != 2 {
		t.Errorf("Pooled disks = %+v (%v)", pool, err)
	}

	// Jobs: about 25, every status, newest first, one running with samples.
	page, err := e.deps.Jobs.List(ctx, s.Admin, jobs.ListFilter{AllUsers: true, Limit: jobs.MaxPageSize})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(page.Jobs); n < 20 || n != len(s.Jobs) {
		t.Fatalf("%d jobs listed, %d seeded", n, len(s.Jobs))
	}
	statuses := map[domain.JobStatus]int{}
	var withWarnings, withErrors, aliceJobs int
	for i, j := range page.Jobs {
		statuses[j.Status]++
		if len(j.Warnings) > 0 {
			withWarnings++
		}
		if !j.Error.IsZero() {
			withErrors++
		}
		if j.OwnerID == s.Alice.UserID {
			aliceJobs++
		}
		if i > 0 && !j.CreatedAt.Before(page.Jobs[i-1].CreatedAt) {
			t.Errorf("jobs are not newest first: %v then %v", page.Jobs[i-1].CreatedAt, j.CreatedAt)
		}
		if j.ID != s.Jobs[len(s.Jobs)-1-i] {
			t.Errorf("job %d is %s, want %s (IDs follow creation times)", i, j.ID, s.Jobs[len(s.Jobs)-1-i])
		}
		if j.Status.Terminal() && j.Status != domain.JobCancelled && j.Error.Code != "dispatch_timeout" && j.Error.Code != "path_not_allowed" && j.StartedAt.IsZero() {
			t.Errorf("finished job %s never started", j.ID)
		}
		if j.CreatedAt.After(t0) || j.FinishedAt.After(t0) {
			t.Errorf("job %s lies in the future: %+v", j.ID, j)
		}
	}
	for _, st := range []domain.JobStatus{domain.JobQueued, domain.JobDispatched, domain.JobRunning, domain.JobSucceeded,
		domain.JobFailed, domain.JobCancelled, domain.JobInterrupted} {
		if statuses[st] == 0 {
			t.Errorf("no %s job (statuses %v)", st, statuses)
		}
	}
	// One job is animated; a backup and a check are shown running.
	if statuses[domain.JobRunning] != 3 || withWarnings == 0 || withErrors == 0 || aliceJobs == 0 {
		t.Errorf("running %d, with warnings %d, with errors %d, alice's %d", statuses[domain.JobRunning], withWarnings, withErrors, aliceJobs)
	}
	if mine, err := e.deps.Jobs.List(ctx, s.Alice, jobs.ListFilter{AllUsers: true, Limit: jobs.MaxPageSize}); err != nil || len(mine.Jobs) != aliceJobs {
		t.Errorf("alice sees %d jobs (%v), owns %d", len(mine.Jobs), err, aliceJobs)
	}
	running, err := e.deps.Jobs.Get(ctx, s.Admin, s.Running)
	if err != nil || running.Status != domain.JobRunning {
		t.Fatalf("running job = %+v (%v)", running, err)
	}
	samples, err := e.deps.Jobs.Samples(ctx, s.Admin, s.Running)
	if err != nil || len(samples) < 100 {
		t.Fatalf("running job has %d samples (%v)", len(samples), err)
	}
	for i := 1; i < len(samples); i++ {
		if samples[i].Stats.Bytes < samples[i-1].Stats.Bytes || !samples[i].Time.After(samples[i-1].Time) {
			t.Fatalf("samples %d and %d go backwards: %+v, %+v", i-1, i, samples[i-1], samples[i])
		}
	}
	if last := samples[len(samples)-1]; last.Time.After(t0) || last.Stats.Bytes <= 0 || last.Stats.TotalBytes <= last.Stats.Bytes {
		t.Errorf("last sample = %+v", last)
	}

	// The demo remotes' secrets are stored encrypted, never logged (the
	// demo passwords are public and logged on purpose).
	for _, secret := range []string{"syncr-demo-minio-secret", "syncr-demo-sftp-password", "syncr-demo-webdav-password", "K001syncrDemo", "syncr-demo-offsite-password"} {
		if strings.Contains(e.logs.String(), secret) {
			t.Errorf("the log contains %q", secret)
		}
	}
	if !strings.Contains(e.logs.String(), "DEVELOPMENT MODE") {
		t.Errorf("no warning logged:\n%s", e.logs.String())
	}
}

func TestSeedRefusesToRunTwice(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.seed(t)
	_, err := devseed.Run(context.Background(), e.deps)
	if !errors.Is(err, devseed.ErrNotEmpty) || !errors.Is(err, domain.ErrFailedPrecondition) {
		t.Fatalf("second Run = %v, want ErrNotEmpty", err)
	}

	// Nor into a controller that already has a user.
	f := newEnv(t)
	if err := f.deps.Users.ArmSetup("ABCDE-FGHIJ-KLMNO-PQRS2"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.deps.Users.CompleteSetup(context.Background(), users.SetupParams{
		Code: "ABCDE-FGHIJ-KLMNO-PQRS2", Username: "owner", Password: "a long enough password",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := devseed.Run(context.Background(), f.deps); !errors.Is(err, devseed.ErrNotEmpty) {
		t.Fatalf("Run on a set-up store = %v, want ErrNotEmpty", err)
	}
	if _, err := devseed.Run(context.Background(), devseed.Deps{}); err == nil {
		t.Error("Run without dependencies succeeded")
	}
}

// TestSeedIsDeterministic: with the same clock and ID sources two seeds
// produce the same jobs, IDs included.
func TestSeedIsDeterministic(t *testing.T) {
	t.Parallel()
	a, b := newEnv(t), newEnv(t)
	sa, sb := a.seed(t), b.seed(t)
	if !slices.Equal(sa.Jobs, sb.Jobs) || sa.Running != sb.Running || !maps.Equal(sa.Schedules, sb.Schedules) {
		t.Fatalf("job IDs differ:\n%v\n%v", sa.Jobs, sb.Jobs)
	}
	ja, err := a.deps.Jobs.Get(context.Background(), sa.Admin, sa.Running)
	if err != nil {
		t.Fatal(err)
	}
	jb, err := b.deps.Jobs.Get(context.Background(), sb.Admin, sb.Running)
	if err != nil {
		t.Fatal(err)
	}
	if ja.SearchText != jb.SearchText || !bytes.Equal(ja.Definition, jb.Definition) || !ja.StartedAt.Equal(jb.StartedAt) {
		t.Errorf("running jobs differ:\n%+v\n%+v", ja, jb)
	}
}

// TestAnimate: each step keeps the online workers online and publishes the
// running job's progress (stored every few seconds), which only grows.
func TestAnimate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	s := e.seed(t)
	sub := e.broker.Subscribe(s.Running)
	defer sub.Close()

	before, err := e.deps.Jobs.Samples(ctx, s.Admin, s.Running)
	if err != nil {
		t.Fatal(err)
	}
	var last int64
	for i := range 10 {
		e.clock.Advance(time.Second)
		if err := s.Step(ctx); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		u, err := sub.Next(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if u.Progress == nil || u.Progress.Stats.Bytes <= last || !u.Progress.Time.Equal(e.clock.Now()) {
			t.Fatalf("step %d published %+v", i, u)
		}
		last = u.Progress.Stats.Bytes
	}
	after, err := e.deps.Jobs.Samples(ctx, s.Admin, s.Running)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) <= len(before) {
		t.Errorf("no sample stored in 10 s: %d before, %d after", len(before), len(after))
	}

	// Without the animation the online workers would be offline by now.
	e.clock.Advance(10 * time.Minute)
	if err := s.Step(ctx); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"nas-01", "backup-host"} {
		w, err := e.deps.Workers.Get(ctx, s.Workers[name])
		if err != nil {
			t.Fatal(err)
		}
		if st := e.deps.Workers.Status(w); st != domain.WorkerOnline {
			t.Errorf("%s is %s after a step", name, st)
		}
	}

	// A cancelled demo job stops animating without errors.
	if _, err := e.deps.Jobs.Cancel(ctx, s.Admin, s.Running); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.deps.Jobs.Finish(ctx, s.Workers["nas-01"], s.Running, "devseed-nas-01", jobs.Outcome{Status: domain.JobCancelled}); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(time.Second)
	if err := s.Step(ctx); err != nil {
		t.Errorf("step after the job ended: %v", err)
	}
}

func TestAnimateStopsWithItsContext(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	s := e.seed(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		s.Animate(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Animate did not return after its context ended")
	}
}

// TestSeedSchedulesAndNotifications: the demo schedules cover every target
// type and policy with a month of runs (launched, missed for every reason,
// coalesced, failed, a Run now), one paused by its owner and one because
// its worker was removed; the contacts cover every built-in channel; their
// deliveries are in every status; the workers have labels.
func TestSeedSchedulesAndNotifications(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	s := e.seed(t)

	// Workers: labels, and old-nas was removed.
	for name, want := range map[string][]string{"nas-01": {"linux", "nas"}, "office-pc": {"desktop", "windows"}, "backup-host": {"offsite"}} {
		w, err := e.deps.Workers.Get(ctx, s.Workers[name])
		if err != nil || !slices.Equal(w.Labels, want) {
			t.Errorf("%s labels = %v (%v), want %v", name, w.Labels, err, want)
		}
	}
	if _, err := e.deps.Workers.Get(ctx, s.Workers["old-nas"]); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("old-nas = %v, want removed", err)
	}

	// Contacts: one per channel; Home Assistant hears of failed manual jobs.
	wantContacts := map[string]string{"Ops Discord": "discord", "Admin mail": "email", "Home Assistant": "webhook"}
	cs, err := e.deps.Contacts.List(ctx, s.Admin, contacts.ListFilter{})
	if err != nil || len(cs) != len(wantContacts) {
		t.Fatalf("contacts = %v (%v)", cs, err)
	}
	for _, c := range cs {
		if wantContacts[c.Name] != c.Type || !c.Enabled {
			t.Errorf("contact %q is %s (enabled %v)", c.Name, c.Type, c.Enabled)
		}
		if c.Name == "Home Assistant" && !slices.Equal(c.JobEvents, domain.EventSet{domain.EventFailed}) {
			t.Errorf("Home Assistant job events = %v", c.JobEvents)
		}
	}

	// Schedules.
	want := map[string]struct {
		enabled bool
		paused  string
		label   string
		worker  string
	}{
		"Nightly photos → B2": {enabled: true, label: "nas"},
		"Office docs hourly":  {enabled: true, worker: "office-pc"},
		"Weekly archive":      {enabled: true, worker: "backup-host"},
		"Offsite mirror":      {enabled: true, label: "offsite"},
		"Old NAS sync":        {paused: "worker_removed", worker: "old-nas"},
		"Alice's documents":   {label: "linux"},
	}
	// The schedules on repositories are checked in TestSeedRestic.
	if len(s.Schedules) != len(want)+5 {
		t.Fatalf("schedules = %v", s.Schedules)
	}
	missed := map[schedules.MissedAction]bool{}
	overlap := map[schedules.Overlap]bool{}
	reasons := map[string]bool{}
	var coalescedPending, manual, failedJobs bool
	for name, w := range want {
		id, ok := s.Schedules[name]
		if !ok {
			t.Fatalf("no schedule %q", name)
		}
		sc, err := e.store.GetSchedule(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if sc.Enabled != w.enabled || sc.PausedReason != w.paused || sc.Target.Label != w.label ||
			(w.worker != "" && sc.Target.WorkerID != s.Workers[w.worker]) {
			t.Errorf("%s = %+v", name, sc)
		}
		if sc.Enabled && !sc.NextRunAt.After(t0) {
			t.Errorf("%s next run %v is not after the seed", name, sc.NextRunAt)
		}
		missed[sc.Missed.Action], overlap[sc.Overlap] = true, true
		runs, err := e.store.ListRuns(ctx, schedules.RunFilter{ScheduleID: id, Limit: -1})
		if err != nil || len(runs) < 4 {
			t.Fatalf("%s: %d runs (%v)", name, len(runs), err)
		}
		for _, r := range runs {
			reasons[r.Reason] = reasons[r.Reason] || r.Status == schedules.RunMissed || r.Status == schedules.RunFailed
			manual = manual || r.Trigger == schedules.TriggerManual
			if name == "Office docs hourly" && r.Status == schedules.RunPending && r.Coalesced > 0 {
				coalescedPending = true
			}
			if r.Status == schedules.RunLaunched && r.JobStatus == domain.JobFailed {
				failedJobs = true
			}
			if r.ScheduledFor.After(t0) || r.UpdatedAt.After(t0) {
				t.Errorf("%s run %+v lies in the future", name, r)
			}
		}
		page, err := e.deps.Jobs.List(ctx, actorOf(s, sc.OwnerID), jobs.ListFilter{ScheduleID: id, Limit: jobs.MaxPageSize})
		if err != nil || len(page.Jobs) == 0 {
			t.Errorf("%s has no jobs (%v)", name, err)
		}
	}
	if len(missed) != 2 || len(overlap) != 3 {
		t.Errorf("policies: missed %v, overlap %v", missed, overlap)
	}
	for _, r := range []string{"worker_offline", "overlap", "worker_removed"} {
		if !reasons[r] {
			t.Errorf("no run missed or failed for %s (%v)", r, reasons)
		}
	}
	if !coalescedPending || !manual || !failedJobs {
		t.Errorf("coalesced pending run %v, manual run %v, failed scheduled job %v", coalescedPending, manual, failedJobs)
	}

	// Enough jobs for the dashboard's 7 and 30 days.
	page, err := e.deps.Jobs.List(ctx, s.Admin, jobs.ListFilter{AllUsers: true, Limit: jobs.MaxPageSize})
	if err != nil {
		t.Fatal(err)
	}
	week, month := 0, 0
	for _, j := range page.Jobs {
		if j.CreatedAt.After(t0.Add(-7 * 24 * time.Hour)) {
			week++
		}
		if j.CreatedAt.Before(t0.Add(-25 * 24 * time.Hour)) {
			month++
		}
	}
	if len(page.Jobs) < 120 || week < 50 || month < 3 {
		t.Errorf("%d jobs, %d in the last 7 days, %d older than 25 days", len(page.Jobs), week, month)
	}

	// Deliveries in every status: delivered, retrying (pending after a
	// failed attempt), pending (never attempted since queued again),
	// sending and dead, the failed ones with their reason.
	ds, err := e.store.ListDeliveries(ctx, notifications.DeliveryFilter{Limit: -1})
	if err != nil {
		t.Fatal(err)
	}
	var delivered, retrying, pending, sending, dead int
	for _, d := range ds {
		switch {
		case d.Status == notifications.StatusDelivered:
			delivered++
		case d.Status == notifications.StatusPending && d.Attempts > 0 && d.LastError != "" && d.NextAttemptAt.After(t0):
			retrying++
		case d.Status == notifications.StatusPending && d.Attempts == 0 && d.NextAttemptAt.After(t0):
			pending++
		case d.Status == notifications.StatusSending && d.LeaseUntil.After(t0.Add(30*time.Minute)):
			sending++
		case d.Status == notifications.StatusDead && d.LastError != "":
			dead++
		default:
			t.Errorf("unexpected delivery %+v", d)
		}
	}
	if delivered < 20 || retrying != 1 || pending != 1 || sending != 1 || dead != 1 {
		t.Errorf("deliveries: %d delivered, %d retrying, %d pending, %d sending, %d dead", delivered, retrying, pending, sending, dead)
	}

	// Nothing was sent, and no contact secret was logged.
	if strings.Contains(e.logs.String(), "the seed tried to send") {
		t.Error("the seed sent a notification")
	}
	for _, secret := range []string{"syncr-demo-discord-token", "syncr-demo-smtp-password", "syncr-demo-hook", "whsec_c3luY3I"} {
		if strings.Contains(e.logs.String(), secret) {
			t.Errorf("the log contains %q", secret)
		}
	}
}

// actorOf is the demo user id.
func actorOf(s *devseed.Seed, id domain.UserID) domain.Actor {
	if id == s.Alice.UserID {
		return s.Alice
	}
	return s.Admin
}

// TestSeedRestic: the demo repositories with their snapshot caches, the
// restic jobs with their results, the schedules on repositories, the live
// state (a backup waiting behind a running one, a data deletion waiting
// behind a running check) and the repository whose data was deleted.
func TestSeedRestic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	s := e.seed(t)

	// Up-to-date workers report every feature and probe operation.
	nas, err := e.deps.Workers.Get(ctx, s.Workers["nas-01"])
	if err != nil || !slices.Equal(nas.Info.Features, link.Features()) || !slices.Equal(nas.Info.ProbeOps, link.ProbeOps()) {
		t.Errorf("nas-01 = %+v (%v)", nas.Info, err)
	}

	repos, err := e.store.ListRepos(ctx, restic.ListFilter{OwnerID: s.Admin.UserID})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]restic.Repo{}
	for _, r := range repos {
		byName[r.Name] = r
	}
	for name, snapshots := range map[string]int{"Photos vault": 30, "NAS documents": 14, "Office laptop": 10, "Old laptop": 6} {
		r, ok := byName[name]
		if !ok {
			t.Fatalf("no repository %q among %v", name, slices.Collect(maps.Keys(byName)))
		}
		if r.Stats.SnapshotCount != snapshots || r.Stats.LatestSnapshotAt.IsZero() || r.Stats.LatestSnapshotAt.After(t0) ||
			r.Stats.SnapshotsRefreshedAt.IsZero() || len(r.PasswordEnc) == 0 || len(r.ResticID) != 64 || r.CreatedAt.After(t0) {
			t.Errorf("%s = %+v", name, r)
		}
		if r.Stats.LastBackup.ID.IsZero() {
			t.Errorf("%s has no backup job", name)
		}
		if strings.Contains(string(r.PasswordEnc), "syncr-demo-restic") {
			t.Errorf("%s stores its password in the clear", name)
		}
	}
	if _, ok := byName["Retired NAS"]; ok || len(repos) != 4 {
		t.Errorf("the admin's live repositories = %v", slices.Collect(maps.Keys(byName)))
	}
	if alice, err := e.store.ListRepos(ctx, restic.ListFilter{OwnerID: s.Alice.UserID}); err != nil || len(alice) != 1 || alice[0].Name != "Alice's backups" {
		t.Errorf("alice's repositories = %v (%v)", alice, err)
	}
	list, err := e.store.ListSnapshots(ctx, s.Repositories["Office laptop"])
	if err != nil || len(list.Snapshots) != 10 || list.Snapshots[0].Hostname != "OFFICE-PC" ||
		!slices.Equal(list.Snapshots[0].Paths, []string{`C:\Users\ada\Documents`}) || list.Snapshots[0].Summary == nil ||
		!list.Snapshots[0].Time.After(list.Snapshots[1].Time) {
		t.Errorf("Office laptop snapshots = %+v (%v)", list, err)
	}

	// The repository whose data was deleted: gone, password wiped, with
	// its succeeded deletion job and that job's result.
	retired, err := e.store.GetRepo(ctx, s.Repositories["Retired NAS"])
	if err != nil || !retired.Deleted() || retired.DataDeletedAt.IsZero() || retired.PasswordEnc != nil || retired.Deleting {
		t.Fatalf("Retired NAS = %+v (%v)", retired, err)
	}
	gone, err := e.deps.Jobs.Get(ctx, s.Admin, retired.DeletionJobID)
	if err != nil || gone.Kind != "restic_delete_data" || gone.Status != domain.JobSucceeded || len(gone.Output) == 0 {
		t.Errorf("its deletion job = %+v (%v)", gone, err)
	}

	// The repository whose data is being deleted: its schedule is paused,
	// and the queued deletion job waits for the check running on it.
	old := byName["Old laptop"]
	if !old.Deleting || old.DeletionJobID.IsZero() || old.DeletionRequestedAt.After(t0) {
		t.Fatalf("Old laptop = %+v", old)
	}
	del, err := e.deps.Jobs.Get(ctx, s.Admin, old.DeletionJobID)
	if err != nil || del.Kind != "restic_delete_data" || del.Status != domain.JobQueued || del.WorkerID != s.Workers["nas-01"] {
		t.Fatalf("its deletion job = %+v (%v)", del, err)
	}
	blockers, err := e.deps.Jobs.Blockers(ctx, []domain.JobID{del.ID})
	if err != nil {
		t.Fatal(err)
	}
	check, err := e.deps.Jobs.Get(ctx, s.Admin, blockers[del.ID])
	if err != nil || check.Kind != "restic_check" || check.Status != domain.JobRunning || check.RepositoryID != old.ID {
		t.Errorf("the deletion waits for %+v (%v), want the running check", check, err)
	}
	if sc, err := e.store.GetSchedule(ctx, s.Schedules["Old laptop weekly backup"]); err != nil || sc.Enabled ||
		sc.PausedReason != schedules.ReasonRepositoryDeleting || sc.RepositoryID != old.ID {
		t.Errorf("Old laptop weekly backup = %+v (%v)", sc, err)
	}

	// A backup waits behind the running backup of the same repository.
	docs := byName["NAS documents"]
	page, err := e.deps.Jobs.List(ctx, s.Admin, jobs.ListFilter{RepositoryID: docs.ID, Statuses: []domain.JobStatus{domain.JobQueued, domain.JobRunning}, Limit: jobs.MaxPageSize})
	if err != nil || len(page.Jobs) != 2 || page.Jobs[0].Status != domain.JobQueued || page.Jobs[1].Status != domain.JobRunning {
		t.Fatalf("unfinished jobs on NAS documents = %+v (%v)", page.Jobs, err)
	}
	if b, err := e.deps.Jobs.Blockers(ctx, []domain.JobID{page.Jobs[0].ID}); err != nil || b[page.Jobs[0].ID] != page.Jobs[1].ID {
		t.Errorf("the queued backup waits for %v (%v), want %s", b, err, page.Jobs[1].ID)
	}

	// The schedules on repositories ran, and their jobs carry results; the
	// repository's remote is one of the schedule's remotes.
	for name, w := range map[string]struct {
		kind, repo string
		minJobs    int
	}{
		"Nightly photos backup":      {"restic_backup", "Photos vault", 25},
		"Weekly retention: photos":   {"restic_forget", "Photos vault", 3},
		"Monthly deep check: photos": {"restic_check", "Photos vault", 1},
		"Office documents backup":    {"restic_backup", "Office laptop", 5},
	} {
		sc, err := e.store.GetSchedule(ctx, s.Schedules[name])
		if err != nil || !sc.Enabled || sc.Kind != w.kind || sc.RepositoryID != byName[w.repo].ID || !slices.Contains(sc.RemoteIDs, byName[w.repo].RemoteID) {
			t.Errorf("%s = %+v (%v)", name, sc, err)
			continue
		}
		page, err := e.deps.Jobs.List(ctx, s.Admin, jobs.ListFilter{ScheduleID: sc.ID, Limit: jobs.MaxPageSize})
		if err != nil || len(page.Jobs) < w.minJobs {
			t.Errorf("%s: %d jobs (%v), want at least %d", name, len(page.Jobs), err, w.minJobs)
			continue
		}
		for _, j := range page.Jobs {
			if j.Kind != w.kind || j.RepositoryID != sc.RepositoryID || len(j.ConcurrencyKeys) != 1 || (j.Status == domain.JobSucceeded && len(j.Output) == 0) {
				t.Errorf("%s job %s = kind %s, repository %s, keys %v, %d bytes of output", name, j.ID, j.Kind, j.RepositoryID, j.ConcurrencyKeys, len(j.Output))
				break
			}
		}
	}
	// Every restic kind a user can start has a job, the restore included.
	kinds := map[string]bool{}
	all, err := e.deps.Jobs.List(ctx, s.Admin, jobs.ListFilter{AllUsers: true, Limit: jobs.MaxPageSize})
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range all.Jobs {
		kinds[j.Kind] = true
	}
	for _, k := range []string{"rclone_transfer", "restic_backup", "restic_forget", "restic_check", "restic_restore", "restic_delete_data"} {
		if !kinds[k] {
			t.Errorf("no %s job", k)
		}
	}
	if strings.Contains(e.logs.String(), "syncr-demo-restic") {
		t.Error("the log contains the demo repositories' password")
	}
}

// TestProbes: the canned answers a seeded controller gives instead of
// asking its demo workers.
func TestProbes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	s := e.seed(t)
	p := e.deps.Probes
	nas := s.Workers["nas-01"]
	run := func(worker domain.WorkerID, spec *syncrv1.ProbeSpec) *syncrv1.ProbeResult {
		t.Helper()
		res, err := p.Probe(ctx, worker, &syncrv1.RunProbe{ProbeId: "p1", Spec: spec})
		if err != nil || res.GetProbeId() != "p1" {
			t.Fatalf("Probe = %v, %v", res, err)
		}
		return res
	}
	names := func(l *syncrv1.ProbeListing) []string {
		var out []string
		for _, en := range l.GetEntries() {
			out = append(out, en.GetName())
		}
		return out
	}
	local := func(path string, dirsOnly bool) *syncrv1.ProbeSpec {
		return &syncrv1.ProbeSpec{Op: &syncrv1.ProbeSpec_ListLocal{ListLocal: &syncrv1.ProbeListLocal{Path: path, DirsOnly: dirsOnly}}}
	}

	// A worker's folders, inside its local roots only.
	if got := names(run(nas, local("", false)).GetListing()); !slices.Equal(got, []string{"/srv", "/mnt"}) {
		t.Errorf("roots of nas-01 = %v", got)
	}
	if got := names(run(nas, local("/srv", true)).GetListing()); !slices.Equal(got, []string{"docs", "home", "incoming", "media", "photos", "restore"}) {
		t.Errorf("/srv = %v", got)
	}
	month := run(nas, local("/srv/photos/2026/03", false)).GetListing()
	if len(month.GetEntries()) != 9 || month.GetEntries()[0].GetType() != syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_FILE ||
		month.GetEntries()[0].GetPath() != "/srv/photos/2026/03/"+month.GetEntries()[0].GetName() || month.GetEntries()[0].GetSize() == 0 ||
		month.GetEntries()[0].GetModTime().AsTime().After(t0) {
		t.Errorf("/srv/photos/2026/03 = %v", month)
	}
	for path, code := range map[string]syncrv1.JobErrorCode{
		"/etc":         syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED,
		"/backup":      syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED, // backup-host's root, not nas-01's
		"/srv/missing": syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND,
	} {
		if res := run(nas, local(path, false)); res.GetStatus() != syncrv1.ProbeStatus_PROBE_STATUS_FAILED || res.GetError().GetCode() != code {
			t.Errorf("%s = %v, want %s", path, res, code)
		}
	}

	// Remotes: a listing, and what each backend says about its usage.
	remote := func(backend string) []*syncrv1.RemoteDefinition {
		return []*syncrv1.RemoteDefinition{{Alias: "r1", BackendType: backend}}
	}
	minio := run(nas, &syncrv1.ProbeSpec{Remotes: remote("s3"), Op: &syncrv1.ProbeSpec_ListRemote{ListRemote: &syncrv1.ProbeListRemote{
		Location: &syncrv1.Location{Remote: "r1", Path: "archive"}, DirsOnly: true,
	}}})
	if got := names(minio.GetListing()); !slices.Contains(got, "restic") || !slices.Contains(got, "archive") {
		t.Errorf("MinIO archive = %v", got)
	}
	for backend, want := range map[string]func(*syncrv1.ProbeResult) bool{
		"b2":     func(r *syncrv1.ProbeResult) bool { return r.GetUsage().GetUsed() > 0 && !r.GetUsage().GetUnsupported() },
		"s3":     func(r *syncrv1.ProbeResult) bool { return r.GetUsage().GetUnsupported() },
		"sftp":   func(r *syncrv1.ProbeResult) bool { return r.GetUsage().GetFree() > 0 },
		"webdav": func(r *syncrv1.ProbeResult) bool { return r.GetStatus() == syncrv1.ProbeStatus_PROBE_STATUS_FAILED },
	} {
		res := run(nas, &syncrv1.ProbeSpec{Remotes: remote(backend), Op: &syncrv1.ProbeSpec_About{About: &syncrv1.ProbeAbout{Location: &syncrv1.Location{Remote: "r1"}}}})
		if !want(res) {
			t.Errorf("about %s = %v", backend, res)
		}
	}

	// Repositories: the demo ones answer with their password only; a new
	// one can be created where none is, once.
	repo := func(path, password string) *syncrv1.ResticRepository {
		return &syncrv1.ResticRepository{Location: &syncrv1.Location{Remote: "r1", Path: path}, Password: password}
	}
	config := func(backend, path, password string) *syncrv1.ProbeResult {
		return run(nas, &syncrv1.ProbeSpec{Remotes: remote(backend), Op: &syncrv1.ProbeSpec_ResticConfig{ResticConfig: &syncrv1.ProbeResticConfig{Repository: repo(path, password)}}})
	}
	photos, err := e.store.GetRepo(ctx, s.Repositories["Photos vault"])
	if err != nil {
		t.Fatal(err)
	}
	if res := config("b2", "syncr-offsite/restic/photos", "syncr-demo-restic"); res.GetRepositoryConfig().GetId() != photos.ResticID {
		t.Errorf("config of Photos vault = %v, want the stored ID %s", res, photos.ResticID)
	}
	for name, res := range map[string]*syncrv1.ProbeResult{
		"wrong_password":       config("b2", "syncr-offsite/restic/photos", "nope"),
		"repository_not_found": config("b2", "syncr-offsite/elsewhere", "syncr-demo-restic"),
		// Its data was deleted.
		"repository_not_found (deleted)": config("s3", "archive/restic/retired-nas", "syncr-demo-restic"),
	} {
		if res.GetStatus() != syncrv1.ProbeStatus_PROBE_STATUS_FAILED || !strings.HasPrefix(name, link.ErrorCodeName(res.GetError().GetCode())) {
			t.Errorf("%s = %v", name, res)
		}
	}
	initAt := func(path string) *syncrv1.ProbeResult {
		return run(nas, &syncrv1.ProbeSpec{Remotes: remote("s3"), Op: &syncrv1.ProbeSpec_ResticInit{ResticInit: &syncrv1.ProbeResticInit{Repository: repo(path, "a new password")}}})
	}
	if res := initAt("archive/restic/nas-docs"); res.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_ALREADY_EXISTS {
		t.Errorf("init over NAS documents = %v", res)
	}
	created := initAt("archive/restic/new")
	if len(created.GetRepositoryConfig().GetId()) != 64 || initAt("archive/restic/new").GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_ALREADY_EXISTS {
		t.Errorf("init of a new repository = %v", created)
	}
	if res := config("s3", "archive/restic/new", "a new password"); res.GetRepositoryConfig().GetId() != created.GetRepositoryConfig().GetId() {
		t.Errorf("config of the new repository = %v", res)
	}

	// Snapshots match the seeded cache, and their trees can be walked.
	snaps := run(nas, &syncrv1.ProbeSpec{Remotes: remote("b2"), Op: &syncrv1.ProbeSpec_ResticSnapshots{ResticSnapshots: &syncrv1.ProbeResticSnapshots{
		Repository: repo("syncr-offsite/restic/photos", "syncr-demo-restic"),
	}}}).GetSnapshots().GetSnapshots()
	cached, err := e.store.ListSnapshots(ctx, photos.ID)
	if err != nil || len(snaps) != 30 || len(cached.Snapshots) != 30 || snaps[0].GetId() != cached.Snapshots[0].ID ||
		!snaps[0].GetTime().AsTime().Equal(cached.Snapshots[0].Time) {
		t.Fatalf("snapshots of Photos vault: %d listed, %d cached (%v)", len(snaps), len(cached.Snapshots), err)
	}
	ls := func(snapshot, path string) *syncrv1.ProbeResult {
		return run(nas, &syncrv1.ProbeSpec{Remotes: remote("b2"), Op: &syncrv1.ProbeSpec_ResticLs{ResticLs: &syncrv1.ProbeResticLs{
			Repository: repo("syncr-offsite/restic/photos", "syncr-demo-restic"), SnapshotId: snapshot, Path: path,
		}}})
	}
	if got := names(ls(snaps[0].GetId(), "/").GetListing()); !slices.Equal(got, []string{"srv"}) {
		t.Errorf("the snapshot's top = %v", got)
	}
	if got := names(ls(snaps[0].GetId()[:8], "/srv/photos").GetListing()); !slices.Equal(got, []string{"2025", "2026", "README.txt"}) {
		t.Errorf("/srv/photos in the snapshot = %v", got)
	}
	for what, res := range map[string]*syncrv1.ProbeResult{"an unknown snapshot": ls(strings.Repeat("0", 64), "/"), "a missing folder": ls(snaps[0].GetId(), "/nope")} {
		if res.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND {
			t.Errorf("%s = %v", what, res)
		}
	}
}
