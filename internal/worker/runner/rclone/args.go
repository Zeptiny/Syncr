package rclone

import (
	"errors"
	"fmt"
	"strconv"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/runner/rclonetool"
)

// Limits for RcloneTransferOptions (shared with every rclone run, see
// rclonetool).
const (
	maxTransfers       = rclonetool.MaxTransfers
	maxCheckers        = rclonetool.MaxCheckers
	maxRetries         = rclonetool.MaxRetries
	maxLowLevelRetries = rclonetool.MaxLowLevelRetries
	maxFilters         = rclonetool.MaxFilters
	maxPatternBytes    = rclonetool.MaxPatternBytes
	maxDuration        = rclonetool.MaxDuration
)

// baseFlags make rclone log JSON lines with its stats (rclonetool.BaseFlags).
func baseFlags() []string { return rclonetool.BaseFlags() }

// command returns the rclone command for mode.
func command(mode syncrv1.RcloneTransferMode) (string, error) {
	switch mode {
	case syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_COPY:
		return "copy", nil
	case syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_SYNC:
		return "sync", nil
	case syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_MOVE:
		return "move", nil
	}
	return "", fmt.Errorf("unsupported transfer mode %v", mode)
}

// buildArgs returns rclone's arguments for a transfer from src to dst (both
// already validated rclone paths). Every option maps to one fixed flag; the
// only user-controlled text that reaches the command line is filter patterns
// (as the value of a --filter=... argument) and the two paths, which follow
// "--" so they are never parsed as flags.
func buildArgs(mode syncrv1.RcloneTransferMode, opts *syncrv1.RcloneTransferOptions, src, dst string) ([]string, error) {
	cmd, err := command(mode)
	if err != nil {
		return nil, err
	}
	flags, err := optionFlags(mode, opts)
	if err != nil {
		return nil, err
	}
	args := make([]string, 0, 1+len(baseFlags())+len(flags)+3)
	args = append(args, cmd)
	args = append(args, baseFlags()...)
	args = append(args, flags...)
	return append(args, "--", src, dst), nil
}

// optionFlags validates opts and maps them to flags. A nil opts means rclone
// defaults.
func optionFlags(mode syncrv1.RcloneTransferMode, opts *syncrv1.RcloneTransferOptions) ([]string, error) {
	if opts == nil {
		return nil, nil
	}
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
	boolFlag := func(name string, v bool) {
		if v {
			flags = append(flags, "--"+name)
		}
	}

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

	boolFlag("dry-run", opts.GetDryRun())
	if opts.GetChecksum() && opts.GetSizeOnly() {
		errs = append(errs, errors.New("checksum and size_only are mutually exclusive"))
	}
	boolFlag("checksum", opts.GetChecksum())
	boolFlag("size-only", opts.GetSizeOnly())
	boolFlag("ignore-existing", opts.GetIgnoreExisting())
	boolFlag("update", opts.GetUpdate())
	boolFlag("create-empty-src-dirs", opts.GetCreateEmptySrcDirs())
	if opts.GetDeleteEmptySrcDirs() && mode != syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_MOVE {
		errs = append(errs, errors.New("delete_empty_src_dirs is only valid for move"))
	}
	boolFlag("delete-empty-src-dirs", opts.GetDeleteEmptySrcDirs())
	if opts.GetTrackRenames() && mode != syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_SYNC {
		errs = append(errs, errors.New("track_renames is only valid for sync"))
	}
	boolFlag("track-renames", opts.GetTrackRenames())
	boolFlag("fast-list", opts.GetFastList())

	if v := opts.MaxTransferBytes; v != nil {
		if *v <= 0 {
			errs = append(errs, errors.New("max_transfer_bytes must be positive"))
		} else {
			flags = append(flags, "--max-transfer="+strconv.FormatInt(*v, 10)+"B")
		}
	}
	if d := opts.GetMaxDuration(); d != nil {
		if err := d.CheckValid(); err != nil {
			errs = append(errs, fmt.Errorf("max_duration: %w", err))
		} else if dur := d.AsDuration(); dur <= 0 || dur > maxDuration {
			errs = append(errs, fmt.Errorf("max_duration must be positive and at most %v", maxDuration))
		} else {
			flags = append(flags, "--max-duration="+dur.String())
		}
	}
	intFlag("retries", opts.Retries, 1, maxRetries)
	intFlag("low-level-retries", opts.LowLevelRetries, 0, maxLowLevelRetries)

	filters, err := filterFlags(opts.GetFilters())
	if err != nil {
		errs = append(errs, err)
	}
	flags = append(flags, filters...)

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return flags, nil
}

// filterFlags maps filter rules to --filter=<+|-> <pattern> arguments, in
// order. The "=" form keeps a rule starting with "-" from being read as a
// flag.
func filterFlags(rules []*syncrv1.FilterRule) ([]string, error) {
	lines, err := filterLines(rules)
	if err != nil {
		return nil, err
	}
	for i, l := range lines {
		lines[i] = "--filter=" + l
	}
	return lines, nil
}

// filterLines checks filter rules and returns them as rclone filter lines
// ("+ pattern" or "- pattern"), in order: the values of --filter, and the
// lines of a two-way sync's filters file (rclonetool.FilterLines).
func filterLines(rules []*syncrv1.FilterRule) ([]string, error) {
	return rclonetool.FilterLines(rules)
}
