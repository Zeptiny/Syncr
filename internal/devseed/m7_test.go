package devseed_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/link"
)

// TestSeedMilestone7Probes: the demo workers of the current build list
// Milestone 7's scans and attic-nas, built before Milestone 6, does not.
// The canned answers come from the demo trees and snapshots: sizes add up
// to what the listings show, one walk stops early, searches find versions
// that differ and the newest two snapshots differ in every way restic diff
// reports.
func TestSeedMilestone7Probes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	s := e.seed(t)

	nas, err := e.deps.Workers.Get(ctx, s.Workers["nas-01"])
	if err != nil {
		t.Fatal(err)
	}
	attic, err := e.deps.Workers.Get(ctx, s.Workers["attic-nas"])
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range link.HeavyProbeOps() {
		if !slices.Contains(nas.Info.ProbeOps, op) || slices.Contains(attic.Info.ProbeOps, op) {
			t.Errorf("%s: nas-01 lists %v, attic-nas %v; want only nas-01 to list it", op, nas.Info.ProbeOps, attic.Info.ProbeOps)
		}
	}

	run := func(spec *syncrv1.ProbeSpec) *syncrv1.ProbeResult {
		t.Helper()
		res, err := e.deps.Probes.Probe(ctx, nas.ID, &syncrv1.RunProbe{ProbeId: "scan", Spec: spec, Timeout: durationpb.New(45 * time.Second)})
		if err != nil || res.GetProbeId() != "scan" {
			t.Fatalf("Probe = %v, %v", res, err)
		}
		return res
	}
	failedWith := func(res *syncrv1.ProbeResult, code syncrv1.JobErrorCode, msg string) bool {
		return res.GetStatus() == syncrv1.ProbeStatus_PROBE_STATUS_FAILED && res.GetError().GetCode() == code && strings.Contains(res.GetError().GetMessage(), msg)
	}

	// A worker's folders: totals from the tree the listings show.
	local := func(path string) *syncrv1.ProbeResult {
		return run(&syncrv1.ProbeSpec{Op: &syncrv1.ProbeSpec_MeasureLocal{MeasureLocal: &syncrv1.ProbeMeasureLocal{Path: path}}})
	}
	photos := local("/srv/photos").GetMeasurement()
	if photos.GetPath() != "/srv/photos" || photos.GetFiles() != 107 || photos.GetDirs() != 14 || !photos.GetComplete() || photos.GetUnreadable() != 0 ||
		len(photos.GetChildren()) != 2 || photos.GetChildren()[0].GetName() != "2026" || photos.GetChildren()[1].GetName() != "2025" ||
		photos.GetChildren()[0].GetBytes()+photos.GetChildren()[1].GetBytes()+420 != photos.GetBytes() {
		t.Errorf("/srv/photos = %v", photos)
	}
	if srv := local("/srv").GetMeasurement(); !srv.GetComplete() || srv.GetUnreadable() != 1 || srv.GetBytes() <= photos.GetBytes() ||
		srv.GetChildren()[0].GetName() != "media" {
		t.Errorf("/srv = %v", srv)
	}
	// The big disks: the walk stops at the soft deadline of the scan
	// timeout with what it counted.
	mnt := local("/mnt")
	if m := mnt.GetMeasurement(); m.GetComplete() || m.GetUnreadable() != 1 || m.GetFiles() != 1 || len(m.GetChildren()) != 1 ||
		mnt.GetElapsed().AsDuration() != 40500*time.Millisecond {
		t.Errorf("/mnt = %v", mnt)
	}
	if m := local("/mnt/disk2").GetMeasurement(); !m.GetComplete() || m.GetFiles() != 1 || m.GetDirs() != 2 {
		t.Errorf("/mnt/disk2 = %v", m)
	}
	for path, want := range map[string]syncrv1.JobErrorCode{
		"/etc": syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED, "/srv/missing": syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND,
		"/srv/docs/budget.xlsx": syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC,
	} {
		if res := local(path); !failedWith(res, want, "") {
			t.Errorf("measure %s = %v, want %v", path, res, want)
		}
	}

	// A remote's folder.
	remote := func(backend string) []*syncrv1.RemoteDefinition {
		return []*syncrv1.RemoteDefinition{{Alias: "r1", BackendType: backend}}
	}
	measureRemote := func(backend, path string) *syncrv1.ProbeResult {
		return run(&syncrv1.ProbeSpec{Remotes: remote(backend), Op: &syncrv1.ProbeSpec_MeasureRemote{MeasureRemote: &syncrv1.ProbeMeasureRemote{
			Location: &syncrv1.Location{Remote: "r1", Path: path},
		}}})
	}
	if m := measureRemote("s3", "archive").GetMeasurement(); m.GetPath() != "archive" || !m.GetComplete() || m.GetBytes() < 27*(1<<30) ||
		m.GetChildren()[0].GetName() != "archive" {
		t.Errorf("MinIO = %v", m)
	}
	if res := measureRemote("s3", "archive/nope"); !failedWith(res, syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "nope") {
		t.Errorf("a missing remote folder = %v", res)
	}

	// The photo repository's snapshots.
	repo := &syncrv1.ResticRepository{Location: &syncrv1.Location{Remote: "r1", Path: "syncr-offsite/restic/photos"}, Password: "syncr-demo-restic"}
	snaps := run(&syncrv1.ProbeSpec{Remotes: remote("b2"), Op: &syncrv1.ProbeSpec_ResticSnapshots{ResticSnapshots: &syncrv1.ProbeResticSnapshots{
		Repository: repo,
	}}}).GetSnapshots().GetSnapshots() // newest first
	if len(snaps) != 30 {
		t.Fatalf("%d snapshots", len(snaps))
	}
	newest, previous, oldest := snaps[0].GetId(), snaps[1].GetId(), snaps[29].GetId()
	resticMeasure := func(snapshot, path string) *syncrv1.ProbeResult {
		return run(&syncrv1.ProbeSpec{Remotes: remote("b2"), Op: &syncrv1.ProbeSpec_ResticMeasure{ResticMeasure: &syncrv1.ProbeResticMeasure{
			Repository: repo, SnapshotId: snapshot, Path: path,
		}}})
	}
	// The newest snapshot holds what the worker has; an older one less.
	if m := resticMeasure(newest[:8], "/srv/photos").GetMeasurement(); m.GetBytes() != photos.GetBytes() || m.GetFiles() != photos.GetFiles() || !m.GetComplete() {
		t.Errorf("the newest snapshot's photos = %v, want the worker's %v", m, photos)
	}
	if m := resticMeasure(oldest, "/").GetMeasurement(); m.GetPath() != "/" || m.GetFiles() == photos.GetFiles() || m.GetChildren()[0].GetName() != "srv" {
		t.Errorf("the oldest snapshot = %v", m)
	}
	if res := resticMeasure(strings.Repeat("0", 8), "/"); !failedWith(res, syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "no matching ID") {
		t.Errorf("an unknown snapshot = %v", res)
	}
	if res := resticMeasure(newest, "/nope"); !failedWith(res, syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "has no folder") {
		t.Errorf("a missing folder = %v", res)
	}

	// Searches: every snapshot has the album list, in versions that differ
	// in size and time.
	find := func(patterns []string, ignoreCase bool, ids ...string) *syncrv1.ProbeResult {
		return run(&syncrv1.ProbeSpec{Remotes: remote("b2"), Op: &syncrv1.ProbeSpec_ResticFind{ResticFind: &syncrv1.ProbeResticFind{
			Repository: repo, Patterns: patterns, IgnoreCase: ignoreCase, SnapshotIds: ids,
		}}})
	}
	albums := find([]string{"/srv/photos/2026/albums.json"}, false).GetFind()
	versions := map[string]bool{}
	for i, m := range albums.GetMatches() {
		if m.GetSnapshotId() != snaps[i].GetId() || m.GetEntry().GetPath() != "/srv/photos/2026/albums.json" || m.GetEntry().GetModTime().AsTime().After(snaps[i].GetTime().AsTime()) {
			t.Errorf("match %d = %v", i, m)
		}
		versions[m.GetEntry().GetModTime().AsTime().String()+" "+string(rune(m.GetEntry().GetSize()))] = true
	}
	if len(albums.GetMatches()) != 30 || !albums.GetComplete() || albums.GetTruncated() || albums.GetSnapshotsSearched() != 30 || len(versions) < 5 {
		t.Errorf("albums.json: %d matches, %d versions, complete %v, %d searched", len(albums.GetMatches()), len(versions), albums.GetComplete(), albums.GetSnapshotsSearched())
	}
	// Folders match with everything below them; case can be ignored.
	if f := find([]string{"03"}, false, newest).GetFind(); len(f.GetMatches()) != 10 || f.GetSnapshotsSearched() != 1 {
		t.Errorf("a folder: %d matches", len(f.GetMatches()))
	}
	if f := find([]string{"readme.TXT"}, true, newest[:8], oldest).GetFind(); len(f.GetMatches()) != 2 || f.GetSnapshotsSearched() != 2 ||
		f.GetMatches()[0].GetEntry().GetType() != syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_FILE || f.GetMatches()[1].GetEntry().GetType() != syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_SYMLINK {
		t.Errorf("readme, ignoring case = %v", f)
	}
	// Too many matches: cut, and the search says it stopped.
	if f := find([]string{"*.jpg"}, false).GetFind(); len(f.GetMatches()) != 1000 || !f.GetTruncated() || f.GetComplete() || f.GetSnapshotsSearched() >= 30 {
		t.Errorf("every photo: %d matches, truncated %v, complete %v, %d searched", len(f.GetMatches()), f.GetTruncated(), f.GetComplete(), f.GetSnapshotsSearched())
	}
	if res := find([]string{"x"}, false, "deadbeef"); !failedWith(res, syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "has no snapshot deadbeef") {
		t.Errorf("an unknown snapshot = %v", res)
	}

	// Comparisons: the newest two snapshots differ in every way.
	diff := func(from, to, path string) *syncrv1.ProbeResult {
		return run(&syncrv1.ProbeSpec{Remotes: remote("b2"), Op: &syncrv1.ProbeSpec_ResticDiff{ResticDiff: &syncrv1.ProbeResticDiff{
			Repository: repo, FromSnapshotId: from, ToSnapshotId: to, Path: path,
		}}})
	}
	d := diff(previous, newest, "").GetDiff()
	kinds := map[syncrv1.ResticDiffModifier][]string{}
	for _, c := range d.GetChanges() {
		kinds[c.GetModifier()] = append(kinds[c.GetModifier()], c.GetPath())
	}
	if !slices.Contains(kinds[syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_TYPE], "/srv/photos/README.txt") ||
		!slices.Contains(kinds[syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_CHANGED], "/srv/photos/2026/albums.json") ||
		len(kinds[syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_ADDED]) == 0 || len(kinds[syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_REMOVED]) == 0 ||
		!d.GetComplete() || d.GetTruncated() || d.GetStats().GetChangedFiles() != int64(len(kinds[syncrv1.ResticDiffModifier_RESTIC_DIFF_MODIFIER_CHANGED])) ||
		d.GetStats().GetAddedFiles() == 0 || d.GetStats().GetRemovedFiles() == 0 || d.GetStats().GetAddedBytes() == 0 {
		t.Errorf("newest comparison = %v", d)
	}
	if !slices.IsSortedFunc(d.GetChanges(), func(a, b *syncrv1.ResticDiffChange) int { return strings.Compare(a.GetPath(), b.GetPath()) }) {
		t.Errorf("changes not in path order: %v", d.GetChanges())
	}
	// Oldest to newest: more changes.
	if whole := diff(oldest, newest, "/").GetDiff(); len(whole.GetChanges()) <= len(d.GetChanges()) || whole.GetStats().GetAddedFiles() <= d.GetStats().GetAddedFiles() {
		t.Errorf("oldest to newest = %v", whole)
	}
	// One folder.
	sub := diff(oldest, newest, "/srv/photos/2026").GetDiff()
	for _, c := range sub.GetChanges() {
		if !strings.HasPrefix(c.GetPath(), "/srv/photos/2026/") {
			t.Errorf("a change outside the folder: %v", c)
		}
	}
	if res := diff(previous, newest, "/srv/nope"); !failedWith(res, syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "path srv/nope: not found") {
		t.Errorf("a missing folder = %v", res)
	}
	if res := diff(previous, "deadbeef", ""); !failedWith(res, syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, `no matching ID found for prefix "deadbeef"`) {
		t.Errorf("an unknown snapshot = %v", res)
	}

	// The documents repository on MinIO too, through the S3 remote.
	docs := &syncrv1.ResticRepository{Location: &syncrv1.Location{Remote: "r1", Path: "archive/restic/nas-docs"}, Password: "syncr-demo-restic"}
	ds := run(&syncrv1.ProbeSpec{Remotes: remote("s3"), Op: &syncrv1.ProbeSpec_ResticSnapshots{ResticSnapshots: &syncrv1.ProbeResticSnapshots{Repository: docs}}}).GetSnapshots().GetSnapshots()
	dd := run(&syncrv1.ProbeSpec{Remotes: remote("s3"), Op: &syncrv1.ProbeSpec_ResticDiff{ResticDiff: &syncrv1.ProbeResticDiff{
		Repository: docs, FromSnapshotId: ds[1].GetId(), ToSnapshotId: ds[0].GetId(),
	}}}).GetDiff()
	if len(dd.GetChanges()) < 4 || dd.GetStats() == nil {
		t.Errorf("documents comparison = %v", dd)
	}
}
