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
	"github.com/Zeptiny/Syncr/internal/app/schedules"
	"github.com/Zeptiny/Syncr/internal/app/users"
	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/backend/builtin"
	"github.com/Zeptiny/Syncr/internal/devseed"
	"github.com/Zeptiny/Syncr/internal/domain"
	"github.com/Zeptiny/Syncr/internal/jobkind"
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
	if statuses[domain.JobRunning] != 1 || withWarnings == 0 || withErrors == 0 || aliceJobs == 0 {
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
	for _, secret := range []string{"syncr-demo-minio-secret", "syncr-demo-sftp-password", "syncr-demo-webdav-password", "K001syncrDemo"} {
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
	if len(s.Schedules) != len(want) {
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
