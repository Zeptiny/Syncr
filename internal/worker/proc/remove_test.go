package proc

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var (
	errInUse     = errors.New("in use by another process")
	errPermanent = errors.New("permanent")
)

// flakyRemover fails its first failures calls with err, then succeeds, and
// records the sleeps between attempts.
type flakyRemover struct {
	failures int
	err      error
	calls    int
	slept    []time.Duration
}

func (f *flakyRemover) remover() remover {
	return remover{
		removeAll: func(string) error {
			f.calls++
			if f.calls <= f.failures {
				return f.err
			}
			return nil
		},
		sleep:     func(d time.Duration) { f.slept = append(f.slept, d) },
		transient: func(err error) bool { return errors.Is(err, errInUse) },
	}
}

func (f *flakyRemover) totalSlept() time.Duration {
	var total time.Duration
	for _, d := range f.slept {
		total += d
	}
	return total
}

func TestRemoveRetriesTransientErrorsWithBackoff(t *testing.T) {
	f := &flakyRemover{failures: 3, err: errInUse}
	if err := f.remover().remove("dir"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if f.calls != 4 {
		t.Errorf("attempts = %d, want 4", f.calls)
	}
	want := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond}
	if len(f.slept) != len(want) {
		t.Fatalf("sleeps = %v, want %v", f.slept, want)
	}
	for i := range want {
		if f.slept[i] != want[i] {
			t.Errorf("sleeps = %v, want %v", f.slept, want)
			break
		}
	}
}

func TestRemoveGivesUpAfterTheRetryBudget(t *testing.T) {
	f := &flakyRemover{failures: 1_000, err: errInUse}
	err := f.remover().remove("dir")
	if !errors.Is(err, errInUse) {
		t.Fatalf("remove = %v, want the last error", err)
	}
	if got := f.totalSlept(); got != removeRetryBudget {
		t.Errorf("slept %v in total, want exactly the budget %v", got, removeRetryBudget)
	}
	for _, d := range f.slept {
		if d > removeMaxDelay {
			t.Errorf("a delay of %v exceeds %v", d, removeMaxDelay)
		}
	}
	if f.calls != len(f.slept)+1 {
		t.Errorf("%d attempts for %d sleeps: the last attempt must follow the last sleep", f.calls, len(f.slept))
	}
}

func TestRemoveDoesNotRetryOtherErrors(t *testing.T) {
	f := &flakyRemover{failures: 1, err: errPermanent}
	if err := f.remover().remove("dir"); !errors.Is(err, errPermanent) {
		t.Fatalf("remove = %v", err)
	}
	if f.calls != 1 || len(f.slept) != 0 {
		t.Errorf("attempts = %d, sleeps = %v; want one attempt and no sleep", f.calls, f.slept)
	}
}

func TestRemoveAllRemovesATree(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "job")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat after RemoveAll: %v", err)
	}
	if err := RemoveAll(dir); err != nil {
		t.Errorf("RemoveAll of a missing path: %v", err)
	}
}
