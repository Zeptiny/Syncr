package restic

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	pathpkg "path"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/types/known/timestamppb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
	"github.com/Zeptiny/Syncr/internal/worker/runner/tally"
)

// The Milestone 7 probe operations walk a whole snapshot folder or
// repository: restic_measure, restic_find and restic_diff. Each reads with
// --no-lock, like Ls, and reads restic's output as it is printed. When
// ctx's deadline passes (the executor's soft deadline) each stops restic
// and returns what it found with complete=false and no error; when ctx is
// cancelled it fails.

// Probe kinds of the scans' sessions.
const (
	kindProbeMeasure = "probe_restic_measure"
	kindProbeFind    = "probe_restic_find"
	kindProbeDiff    = "probe_restic_diff"
)

// Bounds of the scans, enforced here too (the executor sets the request's
// bounds the same way).
const (
	defaultFindMatches = 1000
	maxFindMatches     = 5000
	defaultDiffChanges = 2000
	maxDiffChanges     = 5000
	// maxFindPatterns and maxPatternBytes bound a search's patterns.
	maxFindPatterns = 10
	maxPatternBytes = 256
	// maxFindSnapshots bounds the snapshots a search names (each is an
	// argument; Windows limits a command line to 32767 characters).
	maxFindSnapshots = 100
	// maxFindObjectBytes bounds one match (or a group without its
	// matches) as restic find --json prints it; a longer one is skipped.
	maxFindObjectBytes = 1 << 20
)

// scanGrace bounds how long a scanning restic may take to stop once its
// context ends before it is killed: what it found must still reach the
// controller before the probe's own timeout. The scans only read
// (--no-lock), so stopping them hard loses nothing.
const scanGrace = time.Second

// errEnoughMatches stops a search once max_matches is passed.
var errEnoughMatches = errors.New("enough matches")

// runWith runs x as a probe command, with the probe's scratch directory,
// remotes and secrets; every error it returns is scrubbed.
func (p *Prober) runWith(ctx context.Context, probeID string, remotes []*syncrv1.RemoteDefinition, x execution) (*session, error) {
	if !jobIDPattern.MatchString(probeID) {
		return nil, runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL, "invalid probe ID %q", probeID)
	}
	x.secrets = append([]string{x.cmd.repo.GetPassword()}, remoteSecrets(remotes)...)
	x.scratch, x.logID, x.remotes = "probe-"+probeID, probeID, remotes
	_, s, sc, err := p.e.exec(ctx, x)
	if err != nil && !runner.Canceled(err) {
		err = sc.Error(err)
	}
	return s, err
}

// scanEnd tells how a scan's restic ended: on its own (complete), stopped
// at ctx's deadline (not complete, no error), or with an error, which a
// cancellation is.
func scanEnd(ctx context.Context, err error) (complete bool, _ error) {
	switch {
	case err == nil:
		return true, nil
	case runner.Canceled(err) && errors.Is(ctx.Err(), context.DeadlineExceeded):
		return false, nil
	}
	return false, err
}

// validSnapshotID reports a snapshot ID the scans accept: hexadecimal, as
// the controller sends them.
func validSnapshotID(id string) bool { return snapshotIDPattern.MatchString(id) }

// Measure measures a folder of a snapshot (ls --json --recursive -- <id>
// <dir>, which lists dir itself and everything below it, one node per
// line): the totals of everything below op.path and of each direct child
// folder, by name, at most maxChildren of them, largest first. Bytes are
// the files' sizes; links and other nodes count as files without bytes. A
// snapshot holds no unreadable entries, so unreadable stays 0. A folder
// the snapshot does not have fails with NOT_FOUND once restic finished.
func (p *Prober) Measure(ctx context.Context, probeID string, remotes []*syncrv1.RemoteDefinition, op *syncrv1.ProbeResticMeasure, maxChildren int) (*syncrv1.ProbeMeasurement, error) {
	id := op.GetSnapshotId()
	if id != "latest" && !validSnapshotID(id) {
		return nil, invalid("snapshot_id must be \"latest\" or a hexadecimal snapshot ID")
	}
	if err := checkTreePath("path", op.GetPath()); err != nil {
		return nil, err
	}
	dir := pathpkg.Clean(op.GetPath())
	n := &nodeTally{dir: dir, t: tally.New(dir, maxChildren), found: dir == "/"}
	_, err := p.runWith(ctx, probeID, remotes, execution{
		kind:   kindProbeMeasure,
		cmd:    command{repo: op.GetRepository(), verb: "ls", global: []string{"--no-lock"}, args: []string{"--recursive", "--", id, dir}},
		stdout: n.line,
		grace:  scanGrace,
	})
	complete, err := scanEnd(ctx, err)
	if err != nil {
		return nil, err
	}
	if complete && !n.found {
		return nil, runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "the snapshot has no folder %q", dir)
	}
	return n.t.Result(complete), nil
}

// lsNode is the part of a "restic ls --json" node that Measure reads.
type lsNode struct {
	MessageType string `json:"message_type"`
	Type        string `json:"type"`
	Path        string `json:"path"`
	Size        int64  `json:"size"`
}

// nodeTally sums the nodes of a recursive listing of dir.
type nodeTally struct {
	dir string
	t   *tally.Tally
	// found reports that dir was listed as a folder (the root never is).
	found bool
}

func (n *nodeTally) line(l proc.Line) {
	data := bytes.TrimSpace(l.Data)
	if l.Truncated || len(data) == 0 || data[0] != '{' {
		return
	}
	var node lsNode
	if json.Unmarshal(data, &node) != nil || node.MessageType != "node" || node.Path == "" {
		return
	}
	p := pathpkg.Clean(node.Path)
	var rel string
	switch {
	case p == n.dir:
		n.found = node.Type == "dir"
		return
	case n.dir == "/":
		rel = p[1:]
	case strings.HasPrefix(p, n.dir+"/"):
		rel = p[len(n.dir)+1:]
	default:
		return
	}
	var size int64
	if node.Type == "file" {
		size = node.Size
	}
	n.t.Add(rel, node.Type == "dir", size)
}

// Find searches the snapshots: those op.snapshot_ids name, or all of them.
//
// It first lists them (snapshots --json [-- <id>...]), which resolves the
// IDs (one the repository does not have fails with NOT_FOUND) and gives
// the order restic searches them in, newest first. Then it runs find
// --json [--ignore-case] [--snapshot=<id>...] -- <pattern>.... restic
// 0.19 prints its result as one JSON array without newlines while it
// searches: per snapshot with matches, an object with the matches, then
// their count and the snapshot's ID (a snapshot without matches prints
// nothing). Find reads it as it comes (findStream), keeps the first
// op.max_matches matches and stops restic once the snapshot in which it
// passed them is done (truncated).
//
// Matches come newest snapshot first, then in restic's order (by path).
// snapshots_searched counts every snapshot when restic finished; when it
// was stopped, those up to the last one it reported (a lower bound).
// Matches of a snapshot restic had not finished are kept only when it
// can only be the last snapshot left (always for a single snapshot).
//
// Patterns are restic's: a pattern without "/" is matched against the
// names in a path, one with "/" against the path's trailing folders (a
// leading "/" anchors it at the root); a match on a folder also matches
// everything below it. Character classes escape (a literal "*" is
// "[*]").
func (p *Prober) Find(ctx context.Context, probeID string, remotes []*syncrv1.RemoteDefinition, op *syncrv1.ProbeResticFind) (*syncrv1.ResticFindOutput, error) {
	if err := checkPatterns(op.GetPatterns()); err != nil {
		return nil, err
	}
	requested := op.GetSnapshotIds()
	if len(requested) > maxFindSnapshots {
		return nil, invalid("a search names at most %d snapshots", maxFindSnapshots)
	}
	for _, id := range requested {
		if !validSnapshotID(id) {
			return nil, invalid("snapshot_ids must be hexadecimal snapshot IDs")
		}
	}
	limit := int(op.GetMaxMatches())
	if limit <= 0 {
		limit = defaultFindMatches
	}
	limit = min(limit, maxFindMatches)
	repo := op.GetRepository()

	order, err := p.findOrder(ctx, probeID, remotes, repo, requested)
	complete, err := scanEnd(ctx, err)
	switch {
	case err != nil:
		return nil, err
	case !complete:
		return &syncrv1.ResticFindOutput{}, nil
	}

	args := []string{}
	if op.GetIgnoreCase() {
		args = append(args, "--ignore-case")
	}
	if len(requested) > 0 {
		for _, sn := range order {
			args = append(args, "--snapshot="+sn)
		}
	}
	args = append(append(args, "--"), op.GetPatterns()...)

	findCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	f := &findStream{max: limit, enough: func() { stop(errEnoughMatches) }}
	_, err = p.runWith(findCtx, probeID, remotes, execution{
		kind:   kindProbeFind,
		cmd:    command{repo: repo, verb: "find", global: []string{"--no-lock"}, args: args},
		stdout: func(l proc.Line) { f.write(l.Data) },
		chunks: true,
		grace:  scanGrace,
	})
	switch {
	case err == nil:
		// restic finished on its own, possibly just after it was asked
		// to stop (what it printed after that was not read).
		if !f.ended && !f.stopped {
			return nil, runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL, "restic find: the result is incomplete or not understood")
		}
		complete = true
	case runner.Canceled(err) && ctx.Err() == nil && errors.Is(context.Cause(findCtx), errEnoughMatches):
		complete = false
	default:
		if complete, err = scanEnd(ctx, err); err != nil {
			return nil, err
		}
		f.attribute(order)
	}
	return &syncrv1.ResticFindOutput{
		Matches:           f.matches,
		Truncated:         f.extra,
		Complete:          complete,
		SnapshotsSearched: int32(f.searched(order, complete)), //nolint:gosec // at most the repository's snapshots
	}, nil
}

// findOrder lists the snapshots a search covers (the requested ones, or
// all), newest first, as full IDs. A requested ID that matches none fails
// with NOT_FOUND.
func (p *Prober) findOrder(ctx context.Context, probeID string, remotes []*syncrv1.RemoteDefinition, repo *syncrv1.ResticRepository, requested []string) ([]string, error) {
	args := []string{}
	if len(requested) > 0 {
		args = append(append(args, "--"), requested...)
	}
	s, err := p.runWith(ctx, probeID, remotes, execution{
		kind: kindSnapshots,
		cmd:  command{repo: repo, verb: "snapshots", global: []string{"--no-lock"}, args: args},
	})
	if err != nil {
		return nil, err
	}
	out, err := s.snapshotsOutput(0)
	if err != nil {
		return nil, scrubbed(repo, remotes, runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL, "restic snapshots: %w", err))
	}
	snaps := out.GetSnapshots()
	for _, id := range requested {
		if !slices.ContainsFunc(snaps, func(sn *syncrv1.ResticSnapshot) bool { return strings.HasPrefix(sn.GetId(), id) }) {
			return nil, runner.Errorf(syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "the repository has no snapshot %s", id)
		}
	}
	slices.SortFunc(snaps, func(a, b *syncrv1.ResticSnapshot) int {
		if c := b.GetTime().AsTime().Compare(a.GetTime().AsTime()); c != 0 {
			return c
		}
		return cmp.Compare(a.GetId(), b.GetId())
	})
	order := make([]string, 0, len(snaps))
	for _, sn := range snaps {
		if !slices.Contains(order, sn.GetId()) {
			order = append(order, sn.GetId())
		}
	}
	return order, nil
}

// checkPatterns checks a search's patterns as the controller does: 1 to
// maxFindPatterns of them, each 1 to maxPatternBytes bytes of valid UTF-8
// without NUL or line breaks.
func checkPatterns(patterns []string) error {
	if len(patterns) == 0 || len(patterns) > maxFindPatterns {
		return invalid("a search needs 1 to %d patterns", maxFindPatterns)
	}
	for _, pat := range patterns {
		switch {
		case pat == "" || len(pat) > maxPatternBytes:
			return invalid("a pattern must be 1 to %d bytes long", maxPatternBytes)
		case !utf8.ValidString(pat):
			return invalid("a pattern must be valid UTF-8")
		case strings.ContainsAny(pat, "\x00\n\r"):
			return invalid("a pattern must not contain NUL or line breaks")
		}
	}
	return nil
}

// findMatch is the part of a match of "restic find --json" that is read.
type findMatch struct {
	Path        string    `json:"path"`
	Type        string    `json:"type"`
	Size        int64     `json:"size"`
	Mtime       time.Time `json:"mtime"`
	Permissions string    `json:"permissions"`
}

// findGroup is a snapshot's object of "restic find --json", read without
// its matches.
type findGroup struct {
	Snapshot string `json:"snapshot"`
}

// findStream reads the output of "restic find --json" as it arrives, in
// chunks that may end anywhere:
//
//	[{"matches":[{match},...],"hits":N,"snapshot":"<id>"},...]
//
// It tracks the nesting of brackets outside strings: an object at depth 4
// is a match, an object at depth 2 a snapshot's group, read without what
// is nested deeper than its matches array so that only its small fields
// are kept. A group's matches are kept until the group ends with its
// snapshot's ID.
type findStream struct {
	max int
	// enough is called when a group ends after more than max matches.
	enough func()

	depth            int
	inString, escape bool
	match, group     []byte
	matchOver        bool // the match being read is too long; skipped
	groupOver        bool

	pending []*syncrv1.ProbeEntry // matches of the open group
	matches []*syncrv1.ResticFindMatch
	groups  []string // snapshot IDs of the ended groups, in order
	extra   bool     // more than max matches were found
	stopped bool     // enough was called: ignore what follows
	ended   bool     // the closing "]" was read
}

func (f *findStream) write(data []byte) {
	for _, b := range data {
		if f.stopped {
			return
		}
		f.byte(b)
	}
}

func (f *findStream) byte(b byte) {
	if f.inString {
		f.keep(f.depth, b)
		switch {
		case f.escape:
			f.escape = false
		case b == '\\':
			f.escape = true
		case b == '"':
			f.inString = false
		}
		return
	}
	switch b {
	case '"':
		f.inString = true
		f.keep(f.depth, b)
	case '{', '[':
		f.depth++
		switch {
		case f.depth == 2 && b == '{':
			f.group, f.groupOver = f.group[:0], false
		case f.depth == 4 && b == '{':
			f.match, f.matchOver = f.match[:0], false
		}
		f.keep(f.depth, b)
	case '}', ']':
		f.keep(f.depth, b)
		switch {
		case f.depth == 4 && b == '}':
			f.endMatch()
		case f.depth == 2 && b == '}':
			f.endGroup()
		case f.depth == 1 && b == ']':
			f.ended = true
		}
		f.depth = max(f.depth-1, 0)
	default:
		f.keep(f.depth, b)
	}
}

// keep adds b, read at depth, to the object it belongs to.
func (f *findStream) keep(depth int, b byte) {
	switch {
	case depth >= 4:
		if len(f.match) < maxFindObjectBytes {
			f.match = append(f.match, b)
		} else {
			f.matchOver = true
		}
	case depth == 2, depth == 3 && (b == '[' || b == ']' || b == '{' || b == '}') && !f.inString:
		if len(f.group) < maxFindObjectBytes {
			f.group = append(f.group, b)
		} else {
			f.groupOver = true
		}
	}
}

func (f *findStream) endMatch() {
	if f.matchOver {
		return
	}
	var m findMatch
	if json.Unmarshal(f.match, &m) != nil || m.Path == "" {
		return
	}
	if len(f.matches)+len(f.pending) >= f.max {
		f.extra = true
		return
	}
	e := &syncrv1.ProbeEntry{
		Name: runner.ValidText(pathpkg.Base(m.Path)),
		Path: runner.ValidText(m.Path),
		Mode: m.Permissions,
	}
	switch m.Type {
	case "file":
		e.Type = syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_FILE
		e.Size = m.Size
	case "dir":
		e.Type = syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_DIR
	case "symlink":
		e.Type = syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_SYMLINK
	default:
		e.Type = syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_OTHER
	}
	if !m.Mtime.IsZero() {
		e.ModTime = timestamppb.New(m.Mtime)
	}
	f.pending = append(f.pending, e)
}

func (f *findStream) endGroup() {
	var g findGroup
	if f.groupOver || json.Unmarshal(f.group, &g) != nil || !resticIDPattern.MatchString(g.Snapshot) {
		f.pending = f.pending[:0] // matches of an unknown snapshot
		return
	}
	f.assign(g.Snapshot)
	f.groups = append(f.groups, g.Snapshot)
	if f.extra {
		f.stopped = true
		f.enough()
	}
}

// assign gives the pending matches their snapshot.
func (f *findStream) assign(id string) {
	for _, e := range f.pending {
		f.matches = append(f.matches, &syncrv1.ResticFindMatch{SnapshotId: id, Entry: e})
	}
	f.pending = nil
}

// last returns the position in order (newest first) of the last snapshot
// restic reported, -1 if none; one order does not hold (made since it was
// listed) counts by the number of groups.
func (f *findStream) last(order []string) int {
	if len(f.groups) == 0 {
		return -1
	}
	return max(slices.Index(order, f.groups[len(f.groups)-1]), len(f.groups)-1)
}

// attribute gives the matches of the group restic was printing when it
// was stopped their snapshot, when only one snapshot was left to search.
func (f *findStream) attribute(order []string) {
	if rest := order[f.last(order)+1:]; len(rest) == 1 && len(f.pending) > 0 {
		f.assign(rest[0])
	}
}

// searched is the number of snapshots searched: all when restic finished,
// else those up to the last one it reported.
func (f *findStream) searched(order []string, complete bool) int {
	if complete {
		return max(len(order), len(f.groups))
	}
	return f.last(order) + 1
}

// Diff compares two snapshots, or one folder of each (diff --json --
// <from>[:<path>] <to>[:<path>]), and returns at most op.max_changes
// changes, in restic's order (by path), and restic's statistics when it
// finished. It reads every change line so that the statistics, printed
// last, arrive even when the changes were cut (truncated).
//
// Changes of metadata only are not asked for: with --metadata restic marks
// every folder above a changed file as changed ("U"), which would bury the
// real changes and use up max_changes. With a folder, restic prints the
// paths relative to it; Diff makes them snapshot tree paths again. A
// folder that either snapshot lacks, or that is a file, fails with
// NOT_FOUND.
func (p *Prober) Diff(ctx context.Context, probeID string, remotes []*syncrv1.RemoteDefinition, op *syncrv1.ProbeResticDiff) (*syncrv1.ResticDiffOutput, error) {
	from, to := op.GetFromSnapshotId(), op.GetToSnapshotId()
	if !validSnapshotID(from) || !validSnapshotID(to) {
		return nil, invalid("from_snapshot_id and to_snapshot_id must be hexadecimal snapshot IDs")
	}
	dir := "/"
	if op.GetPath() != "" {
		if err := checkTreePath("path", op.GetPath()); err != nil {
			return nil, err
		}
		dir = pathpkg.Clean(op.GetPath())
	}
	if dir != "/" {
		from, to = from+":"+dir, to+":"+dir
	}
	limit := int(op.GetMaxChanges())
	if limit <= 0 {
		limit = defaultDiffChanges
	}
	d := &diffReader{dir: dir, max: min(limit, maxDiffChanges), out: &syncrv1.ResticDiffOutput{}}
	_, err := p.runWith(ctx, probeID, remotes, execution{
		kind:   kindProbeDiff,
		cmd:    command{repo: op.GetRepository(), verb: "diff", global: []string{"--no-lock"}, args: []string{"--", from, to}},
		stdout: d.line,
		grace:  scanGrace,
	})
	complete, err := scanEnd(ctx, err)
	if err != nil {
		return nil, err
	}
	if !complete {
		d.out.Stats = nil
	}
	d.out.Complete = d.out.GetStats() != nil
	return d.out, nil
}

// diffMessage is the part of a "restic diff --json" message that is read.
type diffMessage struct {
	MessageType  string    `json:"message_type"`
	Path         string    `json:"path"`
	Modifier     string    `json:"modifier"`
	ChangedFiles int64     `json:"changed_files"`
	Added        diffStats `json:"added"`
	Removed      diffStats `json:"removed"`
}

type diffStats struct {
	Files int64 `json:"files"`
	Dirs  int64 `json:"dirs"`
	Bytes int64 `json:"bytes"`
}

// diffReader reads the lines of "restic diff --json".
type diffReader struct {
	dir string
	max int
	out *syncrv1.ResticDiffOutput
}

func (d *diffReader) line(l proc.Line) {
	data := bytes.TrimSpace(l.Data)
	if l.Truncated || len(data) == 0 || data[0] != '{' {
		return
	}
	var m diffMessage
	if json.Unmarshal(data, &m) != nil {
		return
	}
	switch m.MessageType {
	case "change":
		if m.Path == "" {
			return
		}
		if len(d.out.Changes) >= d.max {
			d.out.Truncated = true
			return
		}
		d.out.Changes = append(d.out.Changes, &syncrv1.ResticDiffChange{
			Path:     runner.ValidText(treePathIn(d.dir, m.Path)),
			Modifier: diffModifier(m.Modifier),
		})
	case "statistics":
		d.out.Stats = &syncrv1.ResticDiffStats{
			ChangedFiles: m.ChangedFiles,
			AddedFiles:   m.Added.Files, AddedDirs: m.Added.Dirs, AddedBytes: m.Added.Bytes,
			RemovedFiles: m.Removed.Files, RemovedDirs: m.Removed.Dirs, RemovedBytes: m.Removed.Bytes,
		}
	}
}

// treePathIn turns a path restic diff printed relative to the compared
// folder dir into a snapshot tree path, keeping a folder's trailing "/".
func treePathIn(dir, p string) string {
	if dir == "/" {
		return p
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return dir + p
}

// diffModifier maps restic diff's modifier: "+", "-", "M" (content), "T"
// (type), and those restic prints only with --metadata, which Diff does
// not pass: "U" (metadata only) and "TU" (type and metadata). Anything
// else, such as "?" (the content changed while the metadata did not), is
// OTHER.
func diffModifier(m string) syncrv1.ResticDiffModifier {
	switch {
	case m == "+":
		return syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_ADDED
	case m == "-":
		return syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_REMOVED
	case strings.Contains(m, "T"):
		return syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_TYPE
	case strings.Contains(m, "M"):
		return syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_CHANGED
	case m == "U":
		return syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_METADATA
	}
	return syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_OTHER
}
