package localfs

import (
	"errors"
	"fmt"
	"sync"
)

// ErrInUse reports that a folder a job needs overlaps one that another
// running job of this worker uses, and one of the two may create or move
// links there (Claims). It is wrapped by *InUseError.
var ErrInUse = errors.New("another job on this worker uses an overlapping folder")

// InUseError describes a folder a claim could not take (Claim.Add).
type InUseError struct {
	// Path is the folder asked for, and Other the overlapping folder of
	// the running job, of kind OtherKind.
	Path, Other, OtherKind string
	// Exclusive reports whether the folder was asked for exclusively, and
	// OtherExclusive whether the running job holds its folder so.
	Exclusive, OtherExclusive bool
}

// Error implements error.
func (e *InUseError) Error() string {
	why := "it restores into or moves that folder"
	if e.Exclusive && !e.OtherExclusive {
		why = "this job restores into or moves its folder"
	}
	return fmt.Sprintf("%s overlaps %s, which another job of this worker (%s) uses, and %s: "+
		"a job that can create or move links never runs beside another job on an overlapping folder; "+
		"run it again when that job has ended", e.Path, e.Other, e.OtherKind, why)
}

// Unwrap returns ErrInUse.
func (e *InUseError) Unwrap() error { return ErrInUse }

// Claims records the local folders that the running jobs of a worker use,
// so that a job that can change where the links in a folder lead never
// runs beside another job on an overlapping folder. A restore recreates
// the links of a snapshot, and a move of a local folder can carry links
// to another depth, where a relative link leads elsewhere; a tool that
// writes into a folder follows the links to folders it finds there
// (WriteAreas checks them before the tool starts, and nothing may change
// them until it ends). Such jobs take their folders exclusively; every
// other job shares its folders with other sharing jobs.
//
// Folders overlap when one is the other or lies inside it, as written or
// resolved, compared as Resolve compares (Overlap). A claim that would
// overlap fails at once (InUseError) rather than waiting, so a job never
// holds a slot while it waits for another. Claims is safe for concurrent
// use; its zero value is ready, and one value must be shared by all the
// runners of a worker.
type Claims struct {
	mu   sync.Mutex
	held map[*Claim]struct{}
}

// Claim is the set of folders one job uses. Release it when the job ends.
type Claim struct {
	c    *Claims
	kind string
	uses []use
}

// use is one claimed folder.
type use struct {
	clean, resolved string
	exclusive       bool
}

// Take returns an empty claim for a job of kind (named in other jobs'
// InUseErrors).
func (c *Claims) Take(kind string) *Claim {
	cl := &Claim{c: c, kind: kind}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.held == nil {
		c.held = make(map[*Claim]struct{})
	}
	c.held[cl] = struct{}{}
	return cl
}

// Add claims the folder at the absolute path, exclusively or not. It fails
// with an *InUseError when the folder overlaps one that another claim
// holds and either claims it exclusively, and with a *PathError when path
// is not a clean absolute path or cannot be resolved; the claim is
// unchanged then. A path that does not exist is resolved through its
// deepest existing ancestor (Resolve).
func (cl *Claim) Add(path string, exclusive bool) error {
	clean, err := cleanAbs(path)
	if err != nil {
		return err
	}
	resolved, err := resolve(clean)
	if err != nil {
		return err
	}
	u := use{clean: clean, resolved: resolved, exclusive: exclusive}
	c := cl.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.held[cl]; !ok {
		return errors.New("localfs: claim used after release")
	}
	for other := range c.held {
		if other == cl {
			continue
		}
		for _, o := range other.uses {
			if (exclusive || o.exclusive) && u.overlaps(o) {
				return &InUseError{Path: clean, Other: o.clean, OtherKind: other.kind, Exclusive: exclusive, OtherExclusive: o.exclusive}
			}
		}
	}
	cl.uses = append(cl.uses, u)
	return nil
}

// Release gives the claim's folders back. It may be called more than
// once.
func (cl *Claim) Release() {
	cl.c.mu.Lock()
	defer cl.c.mu.Unlock()
	delete(cl.c.held, cl)
}

// overlaps reports whether u and o are the same folder or one lies inside
// the other, as written or resolved.
func (u use) overlaps(o use) bool {
	return within(u.clean, o.clean) || within(o.clean, u.clean) ||
		within(u.resolved, o.resolved) || within(o.resolved, u.resolved)
}
