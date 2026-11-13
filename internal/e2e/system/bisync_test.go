//go:build integration

package system

import (
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
)

// holdHook is a hook the desktop's worker defines for the serialization
// check: it marks that it started, then waits until the test creates the
// "go" file, so the job that names it stays running for exactly as long as
// the test needs.
const holdHook = `touch "$HELD"; while [ ! -e "$GO" ]; do sleep 0.05; done`

// machine is one computer of the two-way sync scenario: a worker and the
// folder it keeps in step with the bucket.
type machine struct {
	name   string
	w      *worker
	folder string
}

// twoWay is a two-way sync definition between a machine's folder (Folder A)
// and the shared bucket path (Folder B).
func twoWay(folder, remote, path string, filters ...*syncrv1.FilterRule) *syncrv1.JobDefinition {
	var opts *syncrv1.RcloneBisyncOptions
	if len(filters) > 0 {
		opts = &syncrv1.RcloneBisyncOptions{Filters: filters}
	}
	return &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_RcloneBisync{RcloneBisync: &syncrv1.RcloneBisyncDefinition{
		Path1: local(folder), Path2: onRemote(remote, path), Options: opts,
	}}}
}

// cloneDef returns a deep copy of def.
func cloneDef(def *syncrv1.JobDefinition) *syncrv1.JobDefinition {
	out, ok := proto.Clone(def).(*syncrv1.JobDefinition)
	if !ok {
		panic("proto.Clone changed the message type")
	}
	return out
}

// oneShot returns a copy of def with resync or force set for a single run.
func oneShot(def *syncrv1.JobDefinition, resync, force bool) *syncrv1.JobDefinition {
	def = cloneDef(def)
	def.GetRcloneBisync().Resync, def.GetRcloneBisync().Force = resync, force
	return def
}

// files returns every file below dir as its slash-separated relative path
// and content.
func files(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// sameTree fails unless the folder holds exactly want.
func sameTree(t *testing.T, what, dir string, want map[string]string) {
	t.Helper()
	got := files(t, dir)
	if !maps.Equal(got, want) {
		t.Fatalf("%s holds %v, want %v", what, sortedKeys(got), sortedKeys(want))
	}
}

func sortedKeys(m map[string]string) []string {
	return slices.Sorted(maps.Keys(m))
}

// TestSystemTwoWaySync is the scenario two-way sync was built for: one
// folder on a desktop and a laptop, kept in step through a bucket. One
// controller, two workers with their own local roots, and MinIO in the
// middle; each machine has its own pair between its folder and the same
// bucket path. It shows that both first runs resync and the union lands
// everywhere; that the two pairs share the bucket location's concurrency
// key, so the laptop's job waits for the desktop's (read from the
// dispatcher's state while the desktop's job is held in a hook); that a new
// file and a deletion spread from one machine to the other after one run
// each; that a file edited on both machines keeps both versions as
// conflict copies on all three sides, with the count in the job's output;
// that changing a pair's filters stops it with RESYNC_REQUIRED until a
// one-shot resync, which then succeeds; that schedules refuse a label target
// and the one-shot fields; and that the state lives on each worker and the
// S3 secret in no log.
func TestSystemTwoWaySync(t *testing.T) {
	t.Parallel()
	rclone, restic := requirements(t)
	cbin, wbin := binaries(t)
	endpoint := startMinIO(t)
	c := newController(t, cbin)

	desktop := &machine{name: "desktop", w: setupNamedWorker(t, c, wbin, rclone, restic, "desktop", false)}
	laptop := &machine{name: "laptop", w: setupNamedWorker(t, c, wbin, rclone, restic, "laptop", false)}
	held, release := filepath.Join(desktop.w.dir, "held"), filepath.Join(desktop.w.dir, "go")
	appendConfig(t, desktop.w.config, fmt.Sprintf("\nhooks:\n  hold:\n    command: [\"/bin/sh\", \"-c\", %q]\n    timeout: 2m\n    env:\n      HELD: %q\n      GO: %q\n",
		holdHook, held, release))
	for _, m := range []*machine{desktop, laptop} {
		m.folder = filepath.Join(m.w.root, "sync")
		if err := os.MkdirAll(m.folder, 0o755); err != nil {
			t.Fatal(err)
		}
		m.w.start()
		info := c.waitOnline(m.w.id).GetInfo()
		if !slices.Contains(info.GetFeatures(), "rclone.bisync") || !slices.Contains(info.GetJobKinds(), "rclone_bisync") {
			t.Fatalf("the %s's worker does not offer two-way sync: features %v, kinds %v", m.name, info.GetFeatures(), info.GetJobKinds())
		}
	}

	minio := c.createRemote(&syncrv1.RemoteInput{
		Name: "MinIO", BackendType: "s3",
		Options: map[string]string{
			"provider": "Minio", "endpoint": endpoint, "region": "us-east-1",
			"access_key_id": minioUser, "bucket": "syncr-bisync", "force_path_style": "true",
		},
		Secrets: map[string]string{"secret_access_key": minioPassword},
	})
	const shared = "shared"
	pair := map[*machine]*syncrv1.JobDefinition{
		desktop: twoWay(desktop.folder, minio, shared),
		laptop:  twoWay(laptop.folder, minio, shared),
	}
	// rclone names its state files after both paths; the kind refuses
	// pairs over 200 bytes. Keep this test's well below.
	for m := range pair {
		if n := len(m.folder) + len("r1:syncr-bisync/") + len(shared); n > 150 {
			t.Fatalf("the %s's folder %q makes the pair %d bytes long; use a shorter temporary directory", m.name, m.folder, n)
		}
	}
	probes := syncrv1connect.NewProbeServiceClient(c.client(), c.base)
	bucket := func() []string {
		t.Helper()
		resp, err := probes.ListRemoteFiles(t.Context(), connect.NewRequest(&syncrv1.ListRemoteFilesRequest{RemoteId: minio, Path: shared}))
		if err != nil {
			t.Fatalf("ListRemoteFiles(%s): %v", shared, err)
		}
		var names []string
		for _, e := range resp.Msg.GetEntries() {
			names = append(names, e.GetName())
		}
		slices.Sort(names)
		return names
	}
	// run runs a machine's pair to its end.
	run := func(m *machine, def *syncrv1.JobDefinition) *syncrv1.Job {
		t.Helper()
		j, _ := c.watch(c.runJob(m.w.id, def))
		if j.GetKind() != "rclone_bisync" {
			t.Fatalf("job kind %q", j.GetKind())
		}
		return j
	}
	succeeded := func(m *machine, def *syncrv1.JobDefinition, reason syncrv1.BisyncResyncReason) *syncrv1.Job {
		t.Helper()
		j := run(m, def)
		wantStatus(t, j, syncrv1.JobStatus_JOB_STATUS_SUCCEEDED)
		if got := j.GetOutput().GetRcloneBisync().GetResyncReason(); got != reason {
			t.Fatalf("the %s's run %s: resync reason %v, want %v", m.name, j.GetId(), got, reason)
		}
		return j
	}
	const (
		normal    = syncrv1.BisyncResyncReason_BISYNC_RESYNC_REASON_UNSPECIFIED
		firstRun  = syncrv1.BisyncResyncReason_BISYNC_RESYNC_REASON_FIRST_RUN
		requested = syncrv1.BisyncResyncReason_BISYNC_RESYNC_REASON_REQUESTED
	)

	// The starting point: each machine and the bucket have files of their
	// own. A copy job puts the bucket's there.
	want := map[string]string{
		"desk.txt": "written on the desktop", "docs/a.txt": "a", "docs/b.txt": "b", "docs/c.txt": "c",
		"lap.txt": "written on the laptop", "cloud.txt": "already in the bucket",
	}
	for _, name := range []string{"desk.txt", "docs/a.txt", "docs/b.txt", "docs/c.txt"} {
		write(t, filepath.Join(desktop.folder, name), want[name])
	}
	write(t, filepath.Join(laptop.folder, "lap.txt"), want["lap.txt"])
	seed := filepath.Join(desktop.w.root, "seed")
	write(t, filepath.Join(seed, "cloud.txt"), want["cloud.txt"])
	j, _ := c.watch(c.runJob(desktop.w.id, copyDef(local(seed), onRemote(minio, shared), 0)))
	wantStatus(t, j, syncrv1.JobStatus_JOB_STATUS_SUCCEEDED)

	// 1. Both first runs, queued while the desktop's is held in its hook:
	// the laptop's waits for it (they share the bucket location's key),
	// although the laptop's worker is idle.
	first := c.runJob(desktop.w.id, withHooks(cloneDef(pair[desktop]), &syncrv1.JobHooks{Before: []string{"hold"}}))
	waitFor(t, "the desktop's first run to start its hook", waitLong, func() bool {
		_, err := os.Stat(held)
		return err == nil
	})
	second := c.runJob(laptop.w.id, pair[laptop])
	got, err := c.jobs.GetJob(t.Context(), connect.NewRequest(&syncrv1.GetJobRequest{JobId: second}))
	if err != nil {
		t.Fatal(err)
	}
	queued := got.Msg.GetJob()
	if queued.GetStatus() != syncrv1.JobStatus_JOB_STATUS_QUEUED || queued.GetWaitingForJobId() != first {
		t.Fatalf("the laptop's first run while the desktop's runs: status %v, waiting for %q, want queued behind %s",
			queued.GetStatus(), queued.GetWaitingForJobId(), first)
	}
	write(t, release, "")
	a, _ := c.watch(first)
	b, _ := c.watch(second)
	for m, j := range map[*machine]*syncrv1.Job{desktop: a, laptop: b} {
		wantStatus(t, j, syncrv1.JobStatus_JOB_STATUS_SUCCEEDED)
		if got := j.GetOutput().GetRcloneBisync().GetResyncReason(); got != firstRun {
			t.Errorf("the %s's first run: resync reason %v, want FIRST_RUN", m.name, got)
		}
		if got := detail(j, "Resync"); got != "First run" || !strings.HasPrefix(j.GetTitle(), "Two-way sync ") {
			t.Errorf("the %s's first run: title %q, Resync detail %q", m.name, j.GetTitle(), got)
		}
	}
	if b.GetStartedAt().AsTime().Before(a.GetFinishedAt().AsTime()) || b.GetWaitingForJobId() != "" {
		t.Errorf("the laptop's first run started at %v, before the desktop's finished at %v", b.GetStartedAt().AsTime(), a.GetFinishedAt().AsTime())
	}
	// The desktop ran before the laptop uploaded, so it picks the
	// laptop's file up on its next, normal, run. Then all three agree.
	succeeded(desktop, pair[desktop], normal)
	sameTree(t, "the desktop's folder", desktop.folder, want)
	sameTree(t, "the laptop's folder", laptop.folder, want)
	if got := bucket(); !slices.Equal(got, []string{"cloud.txt", "desk.txt", "docs", "lap.txt"}) {
		t.Fatalf("the bucket holds %v", got)
	}

	// 2. A file created on the desktop reaches the laptop after one run
	// each.
	want["new.txt"] = "new on the desktop"
	write(t, filepath.Join(desktop.folder, "new.txt"), want["new.txt"])
	succeeded(desktop, pair[desktop], normal)
	succeeded(laptop, pair[laptop], normal)
	sameTree(t, "the laptop's folder", laptop.folder, want)

	// 3. A file deleted on the laptop disappears from the desktop.
	if err := os.Remove(filepath.Join(laptop.folder, "desk.txt")); err != nil {
		t.Fatal(err)
	}
	delete(want, "desk.txt")
	succeeded(laptop, pair[laptop], normal)
	succeeded(desktop, pair[desktop], normal)
	sameTree(t, "the desktop's folder", desktop.folder, want)
	if got := bucket(); slices.Contains(got, "desk.txt") {
		t.Fatalf("the deleted file is still in the bucket: %v", got)
	}

	// 4. A file edited on both machines between runs: the desktop's run
	// uploads its version, the laptop's finds both changed and keeps both,
	// renamed (its own version is Path1's, conflict1), on its side and in
	// the bucket; the desktop's next run brings the copies there too.
	write(t, filepath.Join(desktop.folder, "cloud.txt"), "edited on the desktop")
	write(t, filepath.Join(laptop.folder, "cloud.txt"), "edited on the laptop, differently")
	succeeded(desktop, pair[desktop], normal)
	conflict := succeeded(laptop, pair[laptop], normal)
	out := conflict.GetOutput().GetRcloneBisync()
	if out.GetConflicts() != 1 || !slices.Equal(out.GetConflictPaths(), []string{"cloud.txt"}) {
		t.Errorf("the laptop's run with a conflict: output %v, want 1 conflict on cloud.txt", out)
	}
	if detail(conflict, "Conflicts") != "1" || !slices.ContainsFunc(conflict.GetWarnings(), func(w string) bool { return strings.Contains(w, "changed on both sides") }) {
		t.Errorf("the laptop's run with a conflict: details %v, warnings %q", conflict.GetDetails(), conflict.GetWarnings())
	}
	succeeded(desktop, pair[desktop], normal)
	delete(want, "cloud.txt")
	want["cloud.txt.conflict1"] = "edited on the laptop, differently"
	want["cloud.txt.conflict2"] = "edited on the desktop"
	sameTree(t, "the laptop's folder", laptop.folder, want)
	sameTree(t, "the desktop's folder", desktop.folder, want)
	if got := bucket(); !slices.Equal(got, []string{"cloud.txt.conflict1", "cloud.txt.conflict2", "docs", "lap.txt", "new.txt"}) {
		t.Fatalf("the bucket after the conflict holds %v", got)
	}

	// 5. New filters stop the desktop's pair until a resync accepts them:
	// every run fails with RESYNC_REQUIRED, a one-shot resync succeeds, and
	// the pair then runs normally with the filters applied.
	filtered := twoWay(desktop.folder, minio, shared, &syncrv1.FilterRule{Action: syncrv1.FilterAction_FILTER_ACTION_EXCLUDE, Pattern: "*.tmp"})
	write(t, filepath.Join(desktop.folder, "scratch.tmp"), "stays on the desktop")
	for range 2 {
		stopped := run(desktop, filtered)
		wantStatus(t, stopped, syncrv1.JobStatus_JOB_STATUS_FAILED)
		if stopped.GetError().GetCode() != syncrv1.JobErrorCode_JOB_ERROR_CODE_RESYNC_REQUIRED || !strings.Contains(stopped.GetError().GetMessage(), "resync") {
			t.Fatalf("a run after the filters changed: error %v, want RESYNC_REQUIRED", stopped.GetError())
		}
	}
	resynced := succeeded(desktop, oneShot(filtered, true, false), requested)
	if detail(resynced, "Resync") != "Requested" || !strings.HasSuffix(resynced.GetTitle(), " · Resync") {
		t.Errorf("the resync: title %q, details %v", resynced.GetTitle(), resynced.GetDetails())
	}
	succeeded(desktop, filtered, normal)
	if got := bucket(); slices.Contains(got, "scratch.tmp") {
		t.Errorf("an excluded file reached the bucket: %v", got)
	}
	want["scratch.tmp"] = "stays on the desktop"
	sameTree(t, "the desktop's folder", desktop.folder, want)

	// 6. Schedules: one per machine, on its worker. A label target and the
	// one-shot fields are refused.
	schedules := syncrv1connect.NewScheduleServiceClient(c.client(), c.base)
	schedule := func(name string, target *syncrv1.ScheduleTarget, def *syncrv1.JobDefinition) error {
		_, err := schedules.CreateSchedule(t.Context(), connect.NewRequest(&syncrv1.CreateScheduleRequest{Input: &syncrv1.ScheduleInput{
			Name: name, Cron: "0 3 1 1 *", Timezone: "UTC", Enabled: true, Target: target,
			MissedRun:  &syncrv1.MissedRunPolicy{Action: syncrv1.MissedRunAction_MISSED_RUN_ACTION_RUN_LATE, MaxDelay: durationpb.New(time.Hour)},
			Overlap:    syncrv1.OverlapPolicy_OVERLAP_POLICY_SKIP,
			Definition: def,
		}}))
		return err
	}
	onWorker := func(m *machine) *syncrv1.ScheduleTarget {
		return &syncrv1.ScheduleTarget{Target: &syncrv1.ScheduleTarget_WorkerId{WorkerId: m.w.id}}
	}
	for _, m := range []*machine{desktop, laptop} {
		if err := schedule("Two-way sync on the "+m.name, onWorker(m), pair[m]); err != nil {
			t.Errorf("a schedule on the %s's worker: %v", m.name, err)
		}
	}
	label := &syncrv1.ScheduleTarget{Target: &syncrv1.ScheduleTarget_WorkerLabel{WorkerLabel: "pc"}}
	if err := schedule("On a label", label, pair[desktop]); !slices.Equal(violationFields(err), []string{"target.worker_label"}) {
		t.Errorf("a two-way sync schedule on a label = %v, want a field error on target.worker_label", err)
	}
	if err := schedule("Resync every time", onWorker(desktop), oneShot(pair[desktop], true, true)); !slices.Equal(violationFields(err),
		[]string{"definition.rclone_bisync.resync", "definition.rclone_bisync.force"}) {
		t.Errorf("a schedule with the one-shot fields = %v (%q), want field errors on both", err, violations(err))
	}

	// The state lives on each worker, one directory per pair (the
	// filters are not part of its key), named by the controller's key.
	keys := map[string]bool{}
	for _, m := range []*machine{desktop, laptop} {
		entries, err := os.ReadDir(filepath.Join(m.w.dir, "state", "bisync"))
		if err != nil {
			t.Fatalf("the %s's two-way sync state: %v", m.name, err)
		}
		if len(entries) != 1 || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(entries[0].Name()) {
			t.Fatalf("the %s's two-way sync state holds %v, want one pair", m.name, entries)
		}
		keys[entries[0].Name()] = true
		if _, err := os.Stat(filepath.Join(m.w.dir, "state", "bisync", entries[0].Name(), "work")); err != nil {
			t.Errorf("the %s's pair has no work directory: %v", m.name, err)
		}
	}
	if len(keys) != 2 {
		t.Errorf("the two machines' pairs share a state key: %v", keys)
	}

	for _, m := range []*machine{desktop, laptop} {
		if strings.Contains(m.w.proc.out.String(), minioPassword) {
			t.Errorf("the S3 secret appears in the %s's worker log", m.name)
		}
		if err := m.w.proc.stop(30 * time.Second); err != nil {
			t.Errorf("stop the %s's worker: %v", m.name, err)
		}
		// No rclone of this worker outlives it.
		waitNoneMatching(t, m.w.dir, 10*time.Second)
	}
	if strings.Contains(c.proc.out.String(), minioPassword) {
		t.Error("the S3 secret appears in the controller's log")
	}
}

// violationFields returns the fields of an API error's violations, in
// order.
func violationFields(err error) []string {
	var out []string
	for _, v := range violations(err) {
		field, _, _ := strings.Cut(v, ": ")
		out = append(out, field)
	}
	return out
}

// appendConfig appends YAML to a worker's configuration file.
func appendConfig(t *testing.T, path, yaml string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(yaml); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
