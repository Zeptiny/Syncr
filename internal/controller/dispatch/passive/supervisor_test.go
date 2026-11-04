package passive_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/workers"
	"github.com/Zeptiny/Syncr/internal/controller/dispatch"
	"github.com/Zeptiny/Syncr/internal/controller/dispatch/passive"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// fakeTargets is a mutable list of passive workers.
type fakeTargets struct {
	mu      sync.Mutex
	workers map[domain.WorkerID]workers.Worker
	listErr error
}

func (f *fakeTargets) set(w workers.Worker) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.workers[w.ID] = w
}

func (f *fakeTargets) remove(id domain.WorkerID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.workers, id)
}

func (f *fakeTargets) failList(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listErr = err
}

func (f *fakeTargets) List(_ context.Context, filter workers.ListFilter) ([]workers.Worker, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if filter.Mode != domain.WorkerPassive {
		return nil, errors.New("supervisor must list passive workers only")
	}
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []workers.Worker
	for _, w := range f.workers {
		out = append(out, w)
	}
	return out, nil
}

func (f *fakeTargets) PassiveTarget(_ context.Context, id domain.WorkerID) (workers.PassiveTarget, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w, ok := f.workers[id]
	if !ok {
		return workers.PassiveTarget{}, domain.ErrNotFound
	}
	return workers.PassiveTarget{WorkerID: id, URL: w.URL, CertFingerprint: w.CertFingerprint, Secret: string(w.SecretEnc)}, nil
}

// loops records the loops a supervisor creates and runs. A loop blocks
// until its context ends, unless its worker is set to crash or panic.
type loops struct {
	mu      sync.Mutex
	created map[domain.WorkerID]int
	running map[domain.WorkerID]int
	starts  map[domain.WorkerID]int
	crash   map[domain.WorkerID]string // "error", "return", "panic"
}

func newLoops() *loops {
	return &loops{created: map[domain.WorkerID]int{}, running: map[domain.WorkerID]int{}, starts: map[domain.WorkerID]int{}, crash: map[domain.WorkerID]string{}}
}

type fakeLoop struct {
	l  *loops
	id domain.WorkerID
}

func (f fakeLoop) Run(ctx context.Context) error {
	f.l.mu.Lock()
	f.l.running[f.id]++
	f.l.starts[f.id]++
	mode := f.l.crash[f.id]
	f.l.mu.Unlock()
	defer func() {
		f.l.mu.Lock()
		f.l.running[f.id]--
		f.l.mu.Unlock()
	}()
	switch mode {
	case "error":
		return errors.New("boom")
	case "return":
		return nil
	case "panic":
		panic("kaboom")
	}
	<-ctx.Done()
	return nil
}

func (l *loops) newLoop(id domain.WorkerID, c dispatch.PassiveClient) passive.Loop {
	if c == nil {
		panic("nil client")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.created[id]++
	return fakeLoop{l: l, id: id}
}

func (l *loops) counts(id domain.WorkerID) (created, running int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.created[id], l.running[id]
}

func (l *loops) setCrash(id domain.WorkerID, mode string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.crash[id] = mode
}

func worker(n int) workers.Worker {
	return workers.Worker{
		ID: domain.NewWorkerID(), Mode: domain.WorkerPassive, URL: "https://w" + strings.Repeat("x", n) + ":7443",
		CertFingerprint: strings.Repeat("ab", 32), SecretEnc: []byte("secret-secret-secret"),
	}
}

func TestSupervisor(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		targets := &fakeTargets{workers: map[domain.WorkerID]workers.Worker{}}
		ls := newLoops()
		wake := make(chan struct{}, 1)
		sup, err := passive.NewSupervisor(passive.SupervisorOptions{
			Workers: targets, NewLoop: ls.newLoop, Wake: wake, Resync: time.Minute,
			MinRestart: time.Second, MaxRestart: 8 * time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		a, b := worker(1), worker(2)
		targets.set(a)
		targets.set(b)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error)
		go func() { done <- sup.Run(ctx) }()
		synctest.Wait()

		check := func(what string, id domain.WorkerID, created, running int) {
			t.Helper()
			if c, r := ls.counts(id); c != created || r != running {
				t.Errorf("%s: created %d running %d, want %d %d", what, c, r, created, running)
			}
		}
		check("a started", a.ID, 1, 1)
		check("b started", b.ID, 1, 1)
		if got := sup.Running(); len(got) != 2 {
			t.Errorf("running = %v", got)
		}

		// Unchanged workers keep their loop across resyncs.
		time.Sleep(3 * time.Minute)
		synctest.Wait()
		check("a after resyncs", a.ID, 1, 1)

		// A changed URL, fingerprint or secret restarts the loop (on wake).
		for _, change := range []func(*workers.Worker){
			func(w *workers.Worker) { w.URL = "https://new:7443" },
			func(w *workers.Worker) { w.CertFingerprint = strings.Repeat("cd", 32) },
			func(w *workers.Worker) { w.SecretEnc = []byte("another-secret-value") },
		} {
			change(&a)
			targets.set(a)
			wake <- struct{}{}
			synctest.Wait()
		}
		check("a after changes", a.ID, 4, 1)
		check("b untouched", b.ID, 1, 1)

		// A removed worker's loop stops at the next resync.
		targets.remove(b.ID)
		time.Sleep(time.Minute)
		synctest.Wait()
		check("b removed", b.ID, 1, 0)
		if got := sup.Running(); !slices.Equal(got, []domain.WorkerID{a.ID}) {
			t.Errorf("running = %v", got)
		}

		// A failing list leaves the loops alone.
		targets.failList(errors.New("db down"))
		time.Sleep(time.Minute)
		synctest.Wait()
		check("a during list failure", a.ID, 4, 1)
		targets.failList(nil)

		// A worker with an unusable target gets no loop until fixed.
		bad := worker(3)
		bad.URL = "http://plain:80"
		targets.set(bad)
		wake <- struct{}{}
		synctest.Wait()
		check("bad target", bad.ID, 0, 0)
		bad.URL = "https://fixed:7443"
		targets.set(bad)
		wake <- struct{}{}
		synctest.Wait()
		check("fixed target", bad.ID, 1, 1)

		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run = %v", err)
		}
		check("a stopped", a.ID, 4, 0)
		check("fixed stopped", bad.ID, 1, 0)
	})
}

func TestSupervisorRestartsCrashedLoops(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"error", "return", "panic"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				targets := &fakeTargets{workers: map[domain.WorkerID]workers.Worker{}}
				ls := newLoops()
				sup, err := passive.NewSupervisor(passive.SupervisorOptions{
					Workers: targets, NewLoop: ls.newLoop, Resync: time.Hour,
					MinRestart: time.Second, MaxRestart: 8 * time.Second,
				})
				if err != nil {
					t.Fatal(err)
				}
				w := worker(1)
				targets.set(w)
				ls.setCrash(w.ID, mode)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				go func() { _ = sup.Run(ctx) }()

				// Backoff 1+2+4+8+8 s: 5 restarts in 23 s, never a spin.
				time.Sleep(23*time.Second + time.Millisecond)
				synctest.Wait()
				ls.mu.Lock()
				created, starts := ls.created[w.ID], ls.starts[w.ID]
				ls.mu.Unlock()
				if created != 1 || starts != 6 {
					t.Errorf("created %d, started %d times; want 1 and 6 (runs at 0, 1, 3, 7, 15, 23 s)", created, starts)
				}

				// Once healthy, it stays up.
				ls.setCrash(w.ID, "")
				time.Sleep(10 * time.Second)
				synctest.Wait()
				if _, running := ls.counts(w.ID); running != 1 {
					t.Errorf("running = %d after recovery", running)
				}
			})
		})
	}
}
