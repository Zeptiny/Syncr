package jobkind

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/backend"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Limits of RcloneTransferOptions. They mirror the worker's rclone runner
// (internal/worker/runner/rclone), which enforces them again: checking here
// only gives the caller a field error instead of a failed job.
const (
	maxTransfers       = 64
	maxCheckers        = 256
	maxRetries         = 100
	maxLowLevelRetries = 1000
	maxFilters         = 1000
	maxPatternBytes    = 4096
	maxDuration        = 366 * 24 * time.Hour
)

// transferField is the field path of the transfer in a definition.
const transferField = "definition.rclone_transfer"

// Transfer is the "rclone_transfer" kind: an rclone copy, sync or move
// between two locations, each on one of the owner's remotes or on the
// worker's filesystem.
type Transfer struct{}

var (
	_ Kind    = Transfer{}
	_ Labeled = Transfer{}
)

// Name implements Kind.
func (Transfer) Name() string { return "rclone_transfer" }

// Label implements Labeled.
func (Transfer) Label() string { return "Transfer" }

// Check implements Kind. It checks the mode, both locations (a valid remote
// ID and a relative path without ".." segments, or an absolute local path),
// that the destination differs from the source, and the options (ranges,
// mode-specific flags, filter rules). Refs are the source's then the
// destination's remote; the search terms are the mode and both paths.
func (Transfer) Check(def *syncrv1.JobDefinition) (Refs, error) {
	tr := def.GetRcloneTransfer()
	var v domain.Validation
	if tr == nil {
		v.Add(transferField, "is required")
		return Refs{}, v.Err()
	}
	mode, modeOK := modeName(tr.GetMode())
	if !modeOK {
		v.Add(transferField+".mode", "must be copy, sync or move")
	}
	src := checkLocation(&v, transferField+".source", tr.GetSource())
	dst := checkLocation(&v, transferField+".destination", tr.GetDestination())
	if src.ok && dst.ok && src.remote == dst.remote && src.key == dst.key {
		v.Add(transferField+".destination", "must differ from the source")
	}
	if modeOK {
		checkTransferOptions(&v, transferField+".options", tr.GetMode(), tr.GetOptions())
	}
	if err := v.Err(); err != nil {
		return Refs{}, err
	}
	var refs Refs
	for _, l := range []location{src, dst} {
		if !l.remote.IsZero() && !slices.Contains(refs.RemoteIDs, l.remote) {
			refs.RemoteIDs = append(refs.RemoteIDs, l.remote)
		}
	}
	refs.SearchTerms = []string{mode}
	for _, p := range []string{src.path, dst.path} {
		if p != "" {
			refs.SearchTerms = append(refs.SearchTerms, p)
		}
	}
	return refs, nil
}

// Build implements Kind: the transfer with its two locations on the
// aliased remotes (see the package documentation). It uses in.Remotes
// only.
func (k Transfer) Build(def *syncrv1.JobDefinition, in Resolved) (*syncrv1.JobSpec, error) {
	// Stored definitions are checked again: a spec is never built from a
	// definition the current rules reject.
	refs, err := k.Check(def)
	if err != nil {
		return nil, err
	}
	tr := def.GetRcloneTransfer()
	rs, err := newSpecRemotes(refs.RemoteIDs, in.Remotes)
	if err != nil {
		return nil, fmt.Errorf("build %s: %w", k.Name(), err)
	}
	src, err := rs.location(transferField+".source", tr.GetSource())
	if err != nil {
		return nil, fmt.Errorf("build %s: %w", k.Name(), err)
	}
	dst, err := rs.location(transferField+".destination", tr.GetDestination())
	if err != nil {
		return nil, fmt.Errorf("build %s: %w", k.Name(), err)
	}
	defs, err := rs.definitions()
	if err != nil {
		return nil, fmt.Errorf("build %s: %w", k.Name(), err)
	}
	op := &syncrv1.RcloneTransfer{Mode: tr.GetMode(), Source: src, Destination: dst}
	if opts := tr.GetOptions(); opts != nil {
		op.Options = proto.CloneOf(opts)
	}
	return &syncrv1.JobSpec{Remotes: defs, Operation: &syncrv1.JobSpec_RcloneTransfer{RcloneTransfer: op}}, nil
}

// Describe implements Kind: the mode, then the source and destination, e.g.
// "Copy Photos → B2 offsite" or "Sync /srv/data → B2 offsite". A location
// on a remote is the remote's name, one on the worker's filesystem its
// path ("Worker filesystem" without one). It uses names.Remotes only.
func (Transfer) Describe(def *syncrv1.JobDefinition, names Names) string {
	tr := def.GetRcloneTransfer()
	mode := "Transfer"
	if m, ok := modeName(tr.GetMode()); ok {
		mode = strings.ToUpper(m[:1]) + m[1:]
	}
	return mode + " " + describeLocation(tr.GetSource(), names.Remotes) + " → " + describeLocation(tr.GetDestination(), names.Remotes)
}

// describeLocation names a definition location for people (see
// Transfer.Describe).
func describeLocation(loc *syncrv1.JobLocation, names map[domain.RemoteID]string) string {
	raw := loc.GetRemoteId()
	if raw == "" {
		return cmp.Or(loc.GetPath(), "Worker filesystem")
	}
	if id, err := domain.ParseRemoteID(raw); err == nil {
		if name := names[id]; name != "" {
			return name
		}
	}
	return "Unknown remote"
}

// modeName returns the lower-case name of a transfer mode, or false if
// the mode is not one to run.
func modeName(m syncrv1.RcloneTransferMode) (string, bool) {
	switch m {
	case syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_COPY:
		return "copy", true
	case syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_SYNC:
		return "sync", true
	case syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_MOVE:
		return "move", true
	}
	return "", false
}

// location is a checked definition location.
type location struct {
	// remote is zero for the worker's filesystem.
	remote domain.RemoteID
	// path is as given; key is its normalized form, for comparisons.
	path, key string
	ok        bool
}

// checkLocation checks loc, recording problems on field.
func checkLocation(v *domain.Validation, field string, loc *syncrv1.JobLocation) location {
	if loc == nil {
		v.Add(field, "is required")
		return location{}
	}
	out := location{path: loc.GetPath()}
	if raw := loc.GetRemoteId(); raw != "" {
		id, err := domain.ParseRemoteID(raw)
		if err != nil || id.IsZero() {
			v.Add(field+".remote_id", "is not a valid remote ID")
			return out
		}
		out.remote = id
		key, err := backend.JoinRemote("", out.path)
		if err == nil {
			// The remote may turn out to be local (a Windows root splits
			// at backslashes too): reject ".." there now, not at dispatch.
			err = rejectDotDot(out.path, `/\`)
		}
		if err != nil {
			v.Add(field+".path", pathReason(err))
			return out
		}
		out.key = key
		out.ok = true
		return out
	}
	switch {
	case out.path == "":
		v.Add(field+".path", "is required for a location on the worker (or choose a remote)")
		return out
	case !absoluteLocal(out.path):
		v.Add(field+".path", `must be an absolute path on the worker, e.g. "/srv/data" or "C:\Data"`)
		return out
	}
	key, err := backend.JoinLocal(out.path, "")
	if err != nil {
		v.Add(field+".path", pathReason(err))
		return out
	}
	out.key = key
	out.ok = true
	return out
}

// absoluteLocal reports whether p is an absolute POSIX path or a Windows
// path with a drive letter ("C:\", "C:/") or a UNC path (`\\server\share`).
func absoluteLocal(p string) bool {
	switch {
	case strings.HasPrefix(p, "/"), strings.HasPrefix(p, `\\`):
		return true
	case len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/'):
		c := p[0] | 0x20
		return 'a' <= c && c <= 'z'
	}
	return false
}

// rejectDotDot rejects ".." segments of p, split at any of seps.
func rejectDotDot(p, seps string) error {
	for seg := range strings.FieldsFuncSeq(p, func(r rune) bool { return strings.ContainsRune(seps, r) }) {
		if seg == ".." {
			return &backend.PathError{Reason: `path must not contain ".." segments`}
		}
	}
	return nil
}

// pathReason returns the message of a path error (which never contains the
// path).
func pathReason(err error) string {
	if pe := (*backend.PathError)(nil); errors.As(err, &pe) {
		return pe.Reason
	}
	return "is not a valid path"
}

// checkTransferOptions checks the options like the worker's rclone runner
// does; nil options are rclone's defaults.
func checkTransferOptions(v *domain.Validation, field string, mode syncrv1.RcloneTransferMode, o *syncrv1.RcloneTransferOptions) {
	if o == nil {
		return
	}
	intRange := func(name string, p *int32, lo, hi int32) {
		if p != nil && (*p < lo || *p > hi) {
			v.Add(field+"."+name, fmt.Sprintf("must be between %d and %d", lo, hi))
		}
	}
	intRange("transfers", o.Transfers, 1, maxTransfers)
	intRange("checkers", o.Checkers, 1, maxCheckers)
	intRange("retries", o.Retries, 1, maxRetries)
	intRange("low_level_retries", o.LowLevelRetries, 0, maxLowLevelRetries)
	if p := o.BandwidthLimitBytesPerSecond; p != nil && *p < 0 {
		v.Add(field+".bandwidth_limit_bytes_per_second", "must not be negative")
	}
	if p := o.MaxTransferBytes; p != nil && *p <= 0 {
		v.Add(field+".max_transfer_bytes", "must be positive")
	}
	if d := o.GetMaxDuration(); d != nil {
		if d.CheckValid() != nil || d.AsDuration() <= 0 || d.AsDuration() > maxDuration {
			v.Add(field+".max_duration", "must be positive and at most 366 days")
		}
	}
	if o.GetChecksum() && o.GetSizeOnly() {
		v.Add(field+".size_only", "cannot be combined with checksum")
	}
	if o.GetDeleteEmptySrcDirs() && mode != syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_MOVE {
		v.Add(field+".delete_empty_src_dirs", "is only valid for move")
	}
	if o.GetTrackRenames() && mode != syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_SYNC {
		v.Add(field+".track_renames", "is only valid for sync")
	}
	rules := o.GetFilters()
	if len(rules) > maxFilters {
		v.Add(field+".filters", fmt.Sprintf("must have at most %d rules", maxFilters))
		return
	}
	for i, r := range rules {
		f := fmt.Sprintf("%s.filters[%d]", field, i)
		switch r.GetAction() {
		case syncrv1.FilterAction_FILTER_ACTION_INCLUDE, syncrv1.FilterAction_FILTER_ACTION_EXCLUDE:
		default:
			v.Add(f+".action", "must be include or exclude")
		}
		switch p := r.GetPattern(); {
		case strings.TrimSpace(p) == "":
			v.Add(f+".pattern", "is required")
		case len(p) > maxPatternBytes:
			v.Add(f+".pattern", fmt.Sprintf("must be at most %d bytes", maxPatternBytes))
		case strings.ContainsAny(p, "\x00\r\n"):
			v.Add(f+".pattern", "must not contain NUL bytes or line breaks")
		}
	}
}
