package localfs

import (
	"context"
	"errors"
	"fmt"
)

// Measurement is a measured folder (Policy.Measure): its totals and each
// direct child folder's.
type Measurement struct {
	// Path is the measured folder, cleaned.
	Path string
	// Bytes is the size of every regular file below Path; Files and Dirs
	// count the files (links included, as entries of their own) and the
	// folders below it.
	Bytes, Files, Dirs int64
	// Children are the direct child folders' totals, by name; at most the
	// maxChildren Measure was given.
	Children []ChildMeasurement
	// ChildrenTruncated reports that more child folders were found than
	// returned.
	ChildrenTruncated bool
	// Complete reports that the walk covered everything. It is false when
	// ctx's deadline ended the walk early; every number is then a lower
	// bound.
	Complete bool
	// Unreadable counts the entries the walk could not read (permission
	// denied, vanished while walking) and skipped; links are never
	// followed, so their targets do not count. With any, the totals are
	// lower bounds even when Complete is true.
	Unreadable int64
}

// ChildMeasurement is one direct child folder's totals.
type ChildMeasurement struct {
	// Name is the child folder's base name.
	Name string
	// Bytes, Files and Dirs are as Measurement's, below the child.
	Bytes, Files, Dirs int64
}

// Measure walks path, which Resolve must accept, and sums what is below it
// per direct child folder. Links are counted, never followed, and the walk
// never leaves path. An entry that cannot be read is skipped and counted
// (Unreadable), not fatal.
// When ctx's deadline passes the walk stops and Measure returns what it
// found with Complete false and no error; when ctx is cancelled it returns
// ctx's error.
//
// Not implemented yet: it fails with errors.ErrUnsupported.
func (p *Policy) Measure(ctx context.Context, path string, _ int) (Measurement, error) {
	if err := ctx.Err(); err != nil {
		return Measurement{}, err
	}
	return Measurement{}, fmt.Errorf("measure %q: %w", path, errors.ErrUnsupported)
}
