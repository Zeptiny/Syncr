package devseed

import (
	"cmp"
	"crypto/sha256"
	pathpkg "path"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Milestone 7's scans on the demo: folder sizes of the demo workers'
// folders, of the demo remotes and of the demo snapshots, searches over
// the demo snapshots and comparisons of two of them. Everything is
// computed from the same trees the listings show, so the numbers add up.
//
// The demo snapshots differ (filesAt): the newest holds the repository's
// files as the snapshot browser has always shown them (the same names and
// sizes), and older ones
// differ from it the way backups do: some files appeared later, some
// copies were cleaned up, a few files changed size from time to time (each
// change is a new version for "Show versions"), and the shortest path of
// the others was a symbolic link until the newest snapshot (a changed
// type). The newest two
// snapshots of a repository differ in every kind restic diff reports
// without --metadata (added, removed, changed, type changed), where the
// repository has files enough. The files that change get modification
// times of their own, shortly before the snapshot that first held each
// version.
//
// One measurement stops early (Complete false): any folder whose walk
// reaches /mnt/disk1/old-nas (that is /mnt and /mnt/disk1 of nas-01), as
// a scan that ran into the scan timeout. Two folders cannot be read
// (unreadableDirs); measurements above them count them.

// Scan bounds as a worker applies them.
const (
	scanMaxChildren    = 5000
	scanDefaultMatches = 1000
	scanMaxMatches     = 5000
	scanDefaultChanges = 2000
	scanMaxChanges     = 5000
)

// unreadableDirs are folders of the demo workers a walk cannot read; they
// are not in the listings (a listing would fail on them the same way).
func unreadableDirs() []string { return []string{"/mnt/disk1/lost+found", "/srv/home/alice/.ssh"} }

// partialFrom is where a walk of nas-01's disks runs into the scan timeout:
// everything from it on (in walk order) is not counted.
const partialFrom = "/mnt/disk1/old-nas"

// scanElapsed is how long a scan took on the "worker": a moment, or, for
// one that stopped early, up to the soft deadline of its timeout.
func scanElapsed(run *syncrv1.RunProbe, complete bool) *durationpb.Duration {
	if complete {
		return durationpb.New(1400 * time.Millisecond)
	}
	timeout := run.GetTimeout().AsDuration()
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	margin := min(max(2*time.Second, timeout/10), timeout/2)
	return durationpb.New(timeout - margin)
}

// ---------------------------------------------------------------------------
// Measuring
// ---------------------------------------------------------------------------

// measure sums the tree below dir (a folder of t): its totals and each
// direct child folder's, largest first, at most maxChildren of them. Paths
// skip reports are left out, as a walk that never reached them. Links
// count as files without bytes.
func (t tree) measure(dir string, maxChildren int, skip func(string) bool) *syncrv1.ProbeMeasurement {
	m := &syncrv1.ProbeMeasurement{Path: dir, Complete: true}
	var walk func(d string, child *syncrv1.ProbeChildMeasurement)
	walk = func(d string, child *syncrv1.ProbeChildMeasurement) {
		for _, e := range t[d] {
			if skip != nil && skip(e.GetPath()) {
				continue
			}
			isDir := e.GetType() == syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_DIR
			if isDir {
				m.Dirs++
			} else {
				m.Files++
				m.Bytes += e.GetSize()
			}
			if child != nil {
				if isDir {
					child.Dirs++
				} else {
					child.Files++
					child.Bytes += e.GetSize()
				}
			}
			if isDir {
				next := child
				if d == dir {
					next = &syncrv1.ProbeChildMeasurement{Name: e.GetName()}
					m.Children = append(m.Children, next)
				}
				walk(e.GetPath(), next)
			}
		}
	}
	walk(dir, nil)
	slices.SortFunc(m.Children, func(a, b *syncrv1.ProbeChildMeasurement) int {
		return cmp.Or(cmp.Compare(b.GetBytes(), a.GetBytes()), cmp.Compare(b.GetFiles(), a.GetFiles()), strings.Compare(a.GetName(), b.GetName()))
	})
	if len(m.Children) > maxChildren {
		m.Children, m.ChildrenTruncated = m.Children[:maxChildren], true
	}
	return m
}

// isFile reports whether p is a file (or link) of t.
func (t tree) isFile(p string) bool {
	return slices.ContainsFunc(t[pathpkg.Dir(p)], func(e *syncrv1.ProbeEntry) bool {
		return e.GetPath() == p && e.GetType() != syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_DIR
	})
}

// folder checks that dir is a folder of t: NOT_FOUND if it is missing,
// INVALID_SPEC if it is a file, as a worker's measurement fails.
func (t tree) folder(dir, what string) error {
	if _, ok := t[dir]; ok {
		return nil
	}
	if t.isFile(dir) {
		return failf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, "%s is not a folder", dir)
	}
	return failf(syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "%s: no such folder%s", dir, what)
}

// measureLocal measures a folder of a demo worker, inside its local roots.
func (p *Probes) measureLocal(workerID domain.WorkerID, op *syncrv1.ProbeMeasureLocal) (*syncrv1.ProbeMeasurement, error) {
	p.mu.Lock()
	roots := slices.Clone(p.workers[workerID])
	p.mu.Unlock()
	if !strings.HasPrefix(op.GetPath(), "/") {
		return nil, failf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, "path must be an absolute local path")
	}
	dir := pathpkg.Clean(op.GetPath())
	if !slices.ContainsFunc(roots, func(r string) bool { return dir == r || strings.HasPrefix(dir, r+"/") }) {
		return nil, failf(syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED, "%s: path is outside the worker's local_roots", op.GetPath())
	}
	t := workerTree(p.seedStart())
	if err := t.folder(dir, ""); err != nil {
		return nil, err
	}
	var skip func(string) bool
	partial := dir == "/mnt" || dir == "/mnt/disk1"
	if partial {
		skip = func(p string) bool { return p >= partialFrom }
	}
	m := t.measure(dir, scanMaxChildren, skip)
	m.Complete = !partial
	for _, u := range unreadableDirs() {
		if strings.HasPrefix(u, dir+"/") && (!partial || u < partialFrom) {
			m.Unreadable++
		}
	}
	return m, nil
}

// measureRemote measures a folder of a demo remote.
func (p *Probes) measureRemote(remotes []*syncrv1.RemoteDefinition, op *syncrv1.ProbeMeasureRemote) (*syncrv1.ProbeMeasurement, error) {
	t, err := remoteTree(backendOf(remotes), p.seedStart())
	if err != nil {
		return nil, err
	}
	rel := remoteRel(op.GetLocation())
	dir := pathpkg.Clean("/" + rel)
	if _, ok := t[dir]; !ok {
		return nil, failf(syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "directory not found: %s", rel)
	}
	m := t.measure(dir, scanMaxChildren, nil)
	m.Path = op.GetLocation().GetPath() // the location as the worker got it
	return m, nil
}

// resticMeasure measures a folder of a demo snapshot.
func (p *Probes) resticMeasure(remotes []*syncrv1.RemoteDefinition, op *syncrv1.ProbeResticMeasure) (*syncrv1.ProbeMeasurement, error) {
	r, err := p.open(remotes, op.GetRepository())
	if err != nil {
		return nil, err
	}
	start := p.seedStart()
	snap, ok := r.snapshot(start, op.GetSnapshotId())
	if !ok {
		return nil, failf(syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "restic ls: no matching ID found for prefix %q", op.GetSnapshotId())
	}
	dir := pathpkg.Clean("/" + op.GetPath())
	t := r.demo.treeAt(start, snap)
	if _, ok := t[dir]; !ok {
		return nil, failf(syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "the snapshot has no folder %q", dir)
	}
	return t.measure(dir, scanMaxChildren, nil), nil
}

// ---------------------------------------------------------------------------
// Snapshots that differ
// ---------------------------------------------------------------------------

// snapRef is one snapshot of a demo repository and its age: 0 is the
// newest.
type snapRef struct {
	restic.Snapshot
	age int
}

// snapshot finds the snapshot id (the full ID or a prefix of at least 8
// digits) of r.
func (r probeRepo) snapshot(start time.Time, id string) (snapRef, bool) {
	if r.demo == nil {
		return snapRef{}, false
	}
	for i, s := range r.demo.snapshots(start) {
		if s.ID == id || len(id) >= 8 && strings.HasPrefix(s.ID, id) {
			return snapRef{s, i}, true
		}
	}
	return snapRef{}, false
}

// livingFiles are the files that change from time to time in every demo
// repository that has them.
func livingFiles() []string {
	return []string{"albums.json", "inventory.csv", "return-draft.ods", "thesis.pdf", "[x] todo.md", "budget.xlsx", "notes.md"}
}

// hashOf is a fixed number for a path (24 bits).
func hashOf(p string) int {
	sum := sha256.Sum256([]byte("syncr devseed scan " + p))
	return int(sum[0])<<16 | int(sum[1])<<8 | int(sum[2])
}

// treeAt is the tree of snapshot s of d.
func (d demoRepo) treeAt(start time.Time, s snapRef) tree {
	return buildTree(start, d.filesAt(start, s.age))
}

// filesAt are the files of d's snapshot of age age (0 is the newest; see
// the file comment for how they differ).
func (d demoRepo) filesAt(start time.Time, age int) []treeFile {
	var base []treeFile
	for _, f := range d.files() {
		base = append(base, treeFile{demoFile: f})
	}
	snaps := d.snapshots(start)
	if len(base) == 0 || len(snaps) < 2 {
		return base
	}
	slices.SortFunc(base, func(a, b treeFile) int { return strings.Compare(a.path, b.path) })
	at := func(a int) time.Time { return snaps[min(a, len(snaps)-1)].Time }
	span := len(snaps) - 1

	living := func(f treeFile) bool {
		return slices.Contains(livingFiles(), pathpkg.Base(f.path)) || hashOf(f.path)%18 == 1
	}
	var ordinary []treeFile
	for _, f := range base {
		if !living(f) {
			ordinary = append(ordinary, f)
		}
	}
	// The changed type: the shortest ordinary path, a link until the
	// newest snapshot. Of the others, the newest snapshot adds the last
	// and drops the copy of the first (when there are two).
	var typed, newest, cleaned string
	if len(ordinary) > 0 {
		t := slices.MinFunc(ordinary, func(a, b treeFile) int {
			return cmp.Or(cmp.Compare(len(a.path), len(b.path)), strings.Compare(a.path, b.path))
		})
		typed = t.path
		ordinary = slices.DeleteFunc(ordinary, func(f treeFile) bool { return f.path == typed })
	}
	if len(ordinary) >= 2 {
		cleaned, newest = ordinary[0].path, ordinary[len(ordinary)-1].path
	}

	var out []treeFile
	for _, f := range base {
		h := hashOf(f.path)
		switch {
		case f.path == typed && age >= 1:
			out = append(out, treeFile{demoFile: demoFile{path: f.path}, link: true, mod: at(len(snaps) - 1).Add(-time.Hour)})
			continue
		case living(f):
			out = append(out, version(f, h, age, at))
			continue
		}
		appears := 1 + h/9%span // absent from snapshots this old and older
		if f.path == newest {
			appears = 1
		}
		if (f.path == newest || h%9 == 4 && f.path != cleaned) && age >= appears {
			continue
		}
		out = append(out, f)
		gone := 1 + h/9%span // the copy is in snapshots this old and older
		if f.path == cleaned {
			gone = 1
		}
		if (f.path == cleaned || h%9 == 7) && age >= gone {
			ext := pathpkg.Ext(f.path)
			out = append(out, treeFile{demoFile: demoFile{path: strings.TrimSuffix(f.path, ext) + " (1)" + ext, size: f.size}, mod: at(len(snaps) - 1).Add(-2 * time.Hour)})
		}
	}
	return out
}

// version is file f as a snapshot of age age holds it: the newest
// snapshot has a version of its own, and older ones share one every few
// snapshots, each with its own size and time.
func version(f treeFile, h, age int, at func(int) time.Time) treeFile {
	every := 2 + h>>8%3
	v := (age + every - 1) / every // ages (v-1)*every+1 to v*every; 0 is the newest alone
	f.size -= int64(v) * max(f.size/25, 37)
	if f.size < 1 {
		f.size = 1 + int64(v)
	}
	// Written shortly before the oldest snapshot that holds this version.
	f.mod = at(v * every).Add(-time.Duration(30+h%150) * time.Minute)
	return f
}

// ---------------------------------------------------------------------------
// Searching
// ---------------------------------------------------------------------------

// nodes are the entries below dir of t, folders and files, by path.
func (t tree) nodes(dir string) []*syncrv1.ProbeEntry {
	var out []*syncrv1.ProbeEntry
	var walk func(d string)
	walk = func(d string) {
		for _, e := range t[d] {
			out = append(out, e)
			if e.GetType() == syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_DIR {
				walk(e.GetPath())
			}
		}
	}
	walk(dir)
	slices.SortFunc(out, func(a, b *syncrv1.ProbeEntry) int { return strings.Compare(a.GetPath(), b.GetPath()) })
	return out
}

// findMatch reports whether restic find's pattern matches the node at p:
// a pattern without "/" matches any name in the path; one with "/" matches
// consecutive folders and names (anywhere, or from the top when it starts
// with "/"), "**" any number of them. A match on a folder matches
// everything below it, as restic's does.
func findMatch(pattern, p string, ignoreCase bool) bool {
	if ignoreCase {
		pattern, p = strings.ToLower(pattern), strings.ToLower(p)
	}
	names := strings.Split(strings.Trim(p, "/"), "/")
	anchored := strings.HasPrefix(pattern, "/")
	parts := strings.Split(strings.Trim(pattern, "/"), "/")
	for i := range names {
		if matchPrefix(parts, names[i:]) {
			return true
		}
		if anchored {
			break
		}
	}
	return false
}

// matchPrefix reports whether the pattern parts match the first names.
func matchPrefix(parts, names []string) bool {
	if len(parts) == 0 {
		return true
	}
	if parts[0] == "**" {
		for i := 0; i <= len(names); i++ {
			if matchPrefix(parts[1:], names[i:]) {
				return true
			}
		}
		return false
	}
	if len(names) == 0 {
		return false
	}
	ok, err := pathpkg.Match(parts[0], names[0])
	return err == nil && ok && matchPrefix(parts[1:], names[1:])
}

// resticFind searches demo snapshots, newest first.
func (p *Probes) resticFind(remotes []*syncrv1.RemoteDefinition, op *syncrv1.ProbeResticFind) (*syncrv1.ResticFindOutput, error) {
	r, err := p.open(remotes, op.GetRepository())
	if err != nil {
		return nil, err
	}
	if len(op.GetPatterns()) == 0 {
		return nil, failf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, "patterns must not be empty")
	}
	start := p.seedStart()
	var order []snapRef
	if r.demo != nil {
		for i, s := range r.demo.snapshots(start) {
			if len(op.GetSnapshotIds()) == 0 || slices.ContainsFunc(op.GetSnapshotIds(), func(id string) bool { return strings.HasPrefix(s.ID, id) }) {
				order = append(order, snapRef{s, i})
			}
		}
	}
	for _, id := range op.GetSnapshotIds() {
		if _, ok := r.snapshot(start, id); !ok {
			return nil, failf(syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "the repository has no snapshot %s", id)
		}
	}
	limit := int(op.GetMaxMatches())
	if limit <= 0 {
		limit = scanDefaultMatches
	}
	limit = min(limit, scanMaxMatches)
	out := &syncrv1.ResticFindOutput{Complete: true}
	for _, s := range order {
		for _, e := range r.demo.treeAt(start, s).nodes("/") {
			if !slices.ContainsFunc(op.GetPatterns(), func(pat string) bool { return findMatch(pat, e.GetPath(), op.GetIgnoreCase()) }) {
				continue
			}
			if len(out.Matches) == limit {
				out.Truncated, out.Complete = true, false
				break
			}
			out.Matches = append(out.Matches, &syncrv1.ResticFindMatch{SnapshotId: s.ID, Entry: e})
		}
		out.SnapshotsSearched++
		if out.Truncated {
			break
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Comparing
// ---------------------------------------------------------------------------

// resticDiff compares two demo snapshots, the whole tree or one folder, as
// restic diff without --metadata reports it.
func (p *Probes) resticDiff(remotes []*syncrv1.RemoteDefinition, op *syncrv1.ProbeResticDiff) (*syncrv1.ResticDiffOutput, error) {
	r, err := p.open(remotes, op.GetRepository())
	if err != nil {
		return nil, err
	}
	start := p.seedStart()
	from, ok := r.snapshot(start, op.GetFromSnapshotId())
	if !ok {
		return nil, failf(syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "restic diff: no matching ID found for prefix %q", op.GetFromSnapshotId())
	}
	to, ok := r.snapshot(start, op.GetToSnapshotId())
	if !ok {
		return nil, failf(syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "restic diff: no matching ID found for prefix %q", op.GetToSnapshotId())
	}
	dir := pathpkg.Clean("/" + op.GetPath())
	older, newer := r.demo.treeAt(start, from), r.demo.treeAt(start, to)
	for _, t := range []tree{older, newer} {
		if _, ok := t[dir]; !ok {
			reason := "not found"
			if t.isFile(dir) {
				reason = "not a directory"
			}
			return nil, failf(syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "restic diff: path %s: %s", strings.TrimPrefix(dir, "/"), reason)
		}
	}
	before, after := byPath(older.nodes(dir)), byPath(newer.nodes(dir))
	paths := make([]string, 0, len(before)+len(after))
	for p := range before {
		paths = append(paths, p)
	}
	for p := range after {
		if _, ok := before[p]; !ok {
			paths = append(paths, p)
		}
	}
	slices.Sort(paths)

	limit := int(op.GetMaxChanges())
	if limit <= 0 {
		limit = scanDefaultChanges
	}
	limit = min(limit, scanMaxChanges)
	out := &syncrv1.ResticDiffOutput{Stats: &syncrv1.ResticDiffStats{}, Complete: true}
	st := out.Stats
	add := func(e *syncrv1.ProbeEntry, m syncrv1.ResticDiffModifier) {
		if len(out.Changes) == limit {
			out.Truncated = true // restic reads on: the statistics cover everything
			return
		}
		name := e.GetPath()
		if e.GetType() == syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_DIR {
			name += "/"
		}
		out.Changes = append(out.Changes, &syncrv1.ResticDiffChange{Path: name, Modifier: m})
	}
	for _, p := range paths {
		a, b := before[p], after[p]
		switch {
		case a == nil:
			add(b, syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_ADDED)
			if b.GetType() == syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_DIR {
				st.AddedDirs++
			} else {
				st.AddedFiles++
				st.AddedBytes += b.GetSize()
			}
		case b == nil:
			add(a, syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_REMOVED)
			if a.GetType() == syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_DIR {
				st.RemovedDirs++
			} else {
				st.RemovedFiles++
				st.RemovedBytes += a.GetSize()
			}
		case a.GetType() != b.GetType():
			add(b, syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_TYPE)
		case b.GetType() == syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_FILE && a.GetSize() != b.GetSize():
			add(b, syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_CHANGED)
			st.ChangedFiles++
			st.AddedBytes += b.GetSize()
			st.RemovedBytes += a.GetSize()
		}
	}
	return out, nil
}

func byPath(es []*syncrv1.ProbeEntry) map[string]*syncrv1.ProbeEntry {
	out := make(map[string]*syncrv1.ProbeEntry, len(es))
	for _, e := range es {
		out[e.GetPath()] = e
	}
	return out
}
