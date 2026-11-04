package proc

import (
	"os"
	"time"
)

// Backoff for RemoveAll: the first retry comes after removeFirstDelay, each
// later one after twice the previous delay, at most removeMaxDelay, until
// the delays add up to removeRetryBudget.
const (
	removeFirstDelay  = 10 * time.Millisecond
	removeMaxDelay    = 500 * time.Millisecond
	removeRetryBudget = 5 * time.Second
)

// RemoveAll removes path and everything below it, like os.RemoveAll. Use it
// for a directory a child ran in (its working or temporary directory).
//
// On Windows, an open handle keeps a file or directory from being deleted,
// and such handles can outlive the child for a moment after Run returns:
// restic does not wait for the rclone process it started, which inherited
// the same working directory, and antivirus scanners and the search indexer
// briefly open files a child wrote. RemoveAll therefore retries a removal
// that failed because of such a handle (a sharing or lock violation, a denied
// access, or a directory left non-empty by a deleted file that is still
// open), with backoff, for up to five seconds in total before it returns the
// error. On other systems
// open files never block removal, and it makes a single attempt.
func RemoveAll(path string) error {
	r := remover{removeAll: os.RemoveAll, sleep: time.Sleep, transient: transientRemoveError}
	return r.remove(path)
}

// remover is RemoveAll with its effects injected, for tests.
type remover struct {
	removeAll func(string) error
	sleep     func(time.Duration)
	// transient reports whether a failed removal may succeed if retried.
	transient func(error) bool
}

func (r remover) remove(path string) error {
	delay, slept := removeFirstDelay, time.Duration(0)
	for {
		err := r.removeAll(path)
		if err == nil || !r.transient(err) || slept >= removeRetryBudget {
			return err
		}
		d := min(delay, removeRetryBudget-slept)
		r.sleep(d)
		slept += d
		delay = min(2*delay, removeMaxDelay)
	}
}
