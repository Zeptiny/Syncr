package rclone

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
)

// Limits and defaults of RcloneBisyncOptions beyond those of transfers.
const (
	// defaultMaxDeletePercent is --max-delete when the option is unset
	// (rclone's default too, passed explicitly so that the safety limit
	// never depends on it).
	defaultMaxDeletePercent = 50
	// maxConflictSuffixBytes bounds each of the two conflict suffixes.
	maxConflictSuffixBytes = 32
	// maxBisyncPathBytes bounds the two rclone paths of a pair together.
	// rclone names its state files after both ("a_b..r1_x.path1.lst-new"),
	// and a name over 255 bytes makes it fail with a misleading "prior lock
	// file found"; 200 leaves room for the joiner and the suffixes.
	maxBisyncPathBytes = 200
)

// bisyncState is what the runner adds to a two-way sync's arguments from
// the pair's state on the worker, rather than from its options.
type bisyncState struct {
	// workDir is rclone's work directory (--workdir) and filtersFile the
	// pair's filters file (--filters-file), both in the pair's state
	// directory.
	workDir, filtersFile string
	// resync rebuilds the pair's state from both sides (--resync).
	resync bool
	// force runs past the delete limit and the "all files changed" check
	// (--force).
	force bool
}

// bisyncArgs returns rclone's arguments for a two-way sync of path1 and
// path2 (both already validated rclone paths). As for transfers, every
// option maps to one fixed flag after validation; the state's paths are
// the values of "--flag=value" arguments, and path1 and path2 follow "--".
//
// The fixed flags: --color=NEVER keeps ANSI escapes out of the JSON
// messages; --recover lets a run after an interrupted one finish without a
// resync; --resilient lets rclone retry the errors it can instead of
// requiring a resync. The filters file is always passed, empty without
// rules, so that rclone's hash of it notices any change of the filters.
// --max-delete, --conflict-resolve and --conflict-loser are always passed
// with Syncr's defaults, so that no data-safety default is rclone's.
func bisyncArgs(opts *syncrv1.RcloneBisyncOptions, st bisyncState, path1, path2 string) ([]string, error) {
	flags, err := bisyncOptionFlags(opts, st.resync)
	if err != nil {
		return nil, err
	}
	args := make([]string, 0, 32)
	args = append(args, "bisync")
	args = append(args, baseFlags()...)
	args = append(args,
		"--color=NEVER",
		"--workdir="+st.workDir,
		"--filters-file="+st.filtersFile,
		"--recover",
		"--resilient",
	)
	if st.resync {
		args = append(args, "--resync")
	}
	if st.force {
		args = append(args, "--force")
	}
	args = append(args, flags...)
	return append(args, "--", path1, path2), nil
}

// bisyncOptionFlags validates opts and maps them to flags. A nil opts
// means Syncr's defaults. --resync-mode is part of a resync only, but an
// invalid mode is refused either way.
func bisyncOptionFlags(opts *syncrv1.RcloneBisyncOptions, resync bool) ([]string, error) {
	var flags []string
	var errs []error
	intFlag := func(name string, v *int32, lo, hi int32) {
		if v == nil {
			return
		}
		if *v < lo || *v > hi {
			errs = append(errs, fmt.Errorf("%s must be between %d and %d", name, lo, hi))
			return
		}
		flags = append(flags, "--"+name+"="+strconv.FormatInt(int64(*v), 10))
	}
	enumFlag := func(name string, value func() (string, error)) {
		v, err := value()
		if err != nil {
			errs = append(errs, err)
			return
		}
		flags = append(flags, "--"+name+"="+v)
	}

	mode, err := resyncMode(opts.GetResyncMode())
	if err == nil && resync {
		flags = append(flags, "--resync-mode="+mode)
	}
	errs = append(errs, err)
	if opts.GetDryRun() {
		flags = append(flags, "--dry-run")
	}
	if cmp, err := compareList(opts.GetCompare()); err != nil {
		errs = append(errs, err)
	} else if cmp != "" {
		flags = append(flags, "--compare="+cmp)
	}
	enumFlag("conflict-resolve", func() (string, error) { return conflictResolve(opts.GetConflictResolve()) })
	enumFlag("conflict-loser", func() (string, error) { return conflictLoser(opts.GetConflictLoser()) })
	if s := opts.GetConflictSuffix(); s != "" {
		if err := checkConflictSuffix(s); err != nil {
			errs = append(errs, err)
		} else {
			flags = append(flags, "--conflict-suffix="+s)
		}
	}
	maxDelete := int32(defaultMaxDeletePercent)
	if opts != nil && opts.MaxDeletePercent != nil {
		maxDelete = opts.GetMaxDeletePercent()
	}
	intFlag("max-delete", &maxDelete, 1, 100)
	if opts.GetCheckAccess() {
		flags = append(flags, "--check-access")
	}
	if opts.GetCreateEmptySrcDirs() {
		flags = append(flags, "--create-empty-src-dirs")
	}
	if opts != nil {
		intFlag("transfers", opts.Transfers, 1, maxTransfers)
		intFlag("checkers", opts.Checkers, 1, maxCheckers)
		if v := opts.BandwidthLimitBytesPerSecond; v != nil {
			switch {
			case *v < 0:
				errs = append(errs, errors.New("bandwidth_limit_bytes_per_second must not be negative"))
			case *v > 0:
				// A bare number would mean KiB/s; the B suffix means bytes.
				flags = append(flags, "--bwlimit="+strconv.FormatInt(*v, 10)+"B")
			}
		}
		intFlag("low-level-retries", opts.LowLevelRetries, 0, maxLowLevelRetries)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return flags, nil
}

// resyncMode returns the --resync-mode value of m; UNSPECIFIED is NEWER.
func resyncMode(m syncrv1.BisyncResyncMode) (string, error) {
	switch m {
	case syncrv1.BisyncResyncMode_BISYNC_RESYNC_MODE_UNSPECIFIED, syncrv1.BisyncResyncMode_BISYNC_RESYNC_MODE_NEWER:
		return "newer", nil
	case syncrv1.BisyncResyncMode_BISYNC_RESYNC_MODE_PATH1:
		return "path1", nil
	case syncrv1.BisyncResyncMode_BISYNC_RESYNC_MODE_PATH2:
		return "path2", nil
	case syncrv1.BisyncResyncMode_BISYNC_RESYNC_MODE_OLDER:
		return "older", nil
	case syncrv1.BisyncResyncMode_BISYNC_RESYNC_MODE_LARGER:
		return "larger", nil
	case syncrv1.BisyncResyncMode_BISYNC_RESYNC_MODE_SMALLER:
		return "smaller", nil
	}
	return "", fmt.Errorf("resync_mode %d is not supported", m)
}

// conflictResolve returns the --conflict-resolve value of r; UNSPECIFIED
// is NONE.
func conflictResolve(r syncrv1.BisyncConflictResolve) (string, error) {
	switch r {
	case syncrv1.BisyncConflictResolve_BISYNC_CONFLICT_RESOLVE_UNSPECIFIED, syncrv1.BisyncConflictResolve_BISYNC_CONFLICT_RESOLVE_NONE:
		return "none", nil
	case syncrv1.BisyncConflictResolve_BISYNC_CONFLICT_RESOLVE_PATH1:
		return "path1", nil
	case syncrv1.BisyncConflictResolve_BISYNC_CONFLICT_RESOLVE_PATH2:
		return "path2", nil
	case syncrv1.BisyncConflictResolve_BISYNC_CONFLICT_RESOLVE_NEWER:
		return "newer", nil
	case syncrv1.BisyncConflictResolve_BISYNC_CONFLICT_RESOLVE_OLDER:
		return "older", nil
	case syncrv1.BisyncConflictResolve_BISYNC_CONFLICT_RESOLVE_LARGER:
		return "larger", nil
	case syncrv1.BisyncConflictResolve_BISYNC_CONFLICT_RESOLVE_SMALLER:
		return "smaller", nil
	}
	return "", fmt.Errorf("conflict_resolve %d is not supported", r)
}

// conflictLoser returns the --conflict-loser value of l; UNSPECIFIED is
// NUM. rclone's "delete" has no enum value, so it can never be passed.
func conflictLoser(l syncrv1.BisyncConflictLoser) (string, error) {
	switch l {
	case syncrv1.BisyncConflictLoser_BISYNC_CONFLICT_LOSER_UNSPECIFIED, syncrv1.BisyncConflictLoser_BISYNC_CONFLICT_LOSER_NUM:
		return "num", nil
	case syncrv1.BisyncConflictLoser_BISYNC_CONFLICT_LOSER_PATHNAME:
		return "pathname", nil
	}
	return "", fmt.Errorf("conflict_loser %d is not supported", l)
}

// compareList returns the --compare value of c: its entries in rclone's
// order (size, modtime, checksum), or "" for an empty list (rclone's
// default, size and modtime). Each entry may appear once.
func compareList(c []syncrv1.BisyncCompare) (string, error) {
	entries := []struct {
		v    syncrv1.BisyncCompare
		name string
	}{
		{syncrv1.BisyncCompare_BISYNC_COMPARE_SIZE, "size"},
		{syncrv1.BisyncCompare_BISYNC_COMPARE_MODTIME, "modtime"},
		{syncrv1.BisyncCompare_BISYNC_COMPARE_CHECKSUM, "checksum"},
	}
	count := make(map[syncrv1.BisyncCompare]int, len(c))
	for _, v := range c {
		count[v]++
	}
	var names []string
	for _, e := range entries {
		switch count[e.v] {
		case 0:
		case 1:
			names = append(names, e.name)
		default:
			return "", fmt.Errorf("compare lists %s more than once", e.name)
		}
		delete(count, e.v)
	}
	for v := range count {
		return "", fmt.Errorf("compare entry %d is not supported", v)
	}
	return strings.Join(names, ","), nil
}

// checkConflictSuffix accepts one suffix, or two separated by a comma
// (Path1's, then Path2's), each 1 to 32 letters, digits, ".", "_" or "-".
func checkConflictSuffix(s string) error {
	parts := strings.Split(s, ",")
	if len(parts) > 2 {
		return errors.New("conflict_suffix has more than two suffixes")
	}
	for _, p := range parts {
		if p == "" || len(p) > maxConflictSuffixBytes {
			return fmt.Errorf("each conflict_suffix must be 1 to %d characters", maxConflictSuffixBytes)
		}
		for _, c := range p {
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '.' && c != '_' && c != '-' {
				return errors.New("conflict_suffix may contain only letters, digits, \".\", \"_\" and \"-\"")
			}
		}
	}
	return nil
}
