package restic

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/mount"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
	"github.com/Zeptiny/Syncr/internal/worker/sourcestate"
)

const (
	sourceKey    = "0123456789abcdef0123456789abcdef"
	sourceSecret = "source-remote-secret-value"
	remoteTag    = "syncr-remote:0f8e2a7c-1d3b-4c5e-9f60-718293a4b5c6"
)

var (
	mirrorMethod = syncrv1.ResticRemoteSourceMethod_RESTIC_REMOTE_SOURCE_METHOD_MIRROR
	mountMethod  = syncrv1.ResticRemoteSourceMethod_RESTIC_REMOTE_SOURCE_METHOD_MOUNT
)

// scriptedSupervisor plays rclone and restic: script answers each run
// (nil: the default script, a successful run of each command). It records
// every spec.
type scriptedSupervisor struct {
	mu     sync.Mutex
	specs  []proc.Spec
	script func(ctx context.Context, spec proc.Spec) (proc.Result, error)
}

func (s *scriptedSupervisor) Run(ctx context.Context, spec proc.Spec) (proc.Result, error) {
	s.mu.Lock()
	s.specs = append(s.specs, spec)
	script := s.script
	s.mu.Unlock()
	return script(ctx, spec)
}

func (s *scriptedSupervisor) commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, sp := range s.specs {
		out = append(out, commandOf(sp))
	}
	return out
}

func (s *scriptedSupervisor) spec(cmd string) proc.Spec {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sp := range s.specs {
		if commandOf(sp) == cmd {
			return sp
		}
	}
	return proc.Spec{}
}

// commandOf names a run: "rclone <verb>" or "restic <verb>".
func commandOf(spec proc.Spec) string {
	if spec.Path == fakeRcloneBin {
		return "rclone " + spec.Args[0]
	}
	for _, verb := range []string{"backup", "snapshots", "init"} {
		if slices.Contains(spec.Args, verb) {
			return "restic " + verb
		}
	}
	return "restic ?"
}

// replayLines sends lines to a stream.
func replayLines(f proc.LineFunc, lines ...string) {
	for _, l := range lines {
		f(proc.Line{Data: []byte(l)})
	}
}

// defaultRun is a successful run of each command: lsf lists two entries,
// sync prints rclone v1.75.2's stats of a first mirror, restic backs up
// (its status, then its summary).
func defaultRun(t *testing.T) func(context.Context, proc.Spec) (proc.Result, error) {
	sync := fixture(t, "rclone-1.75.2_sync_mirror.jsonl")
	summary := fixture(t, "backup_summary.out")
	return func(_ context.Context, spec proc.Spec) (proc.Result, error) {
		switch commandOf(spec) {
		case "rclone lsf":
			replayLines(spec.Stdout, "a.bin", "sub/")
		case "rclone sync":
			replayLines(spec.Stderr, sync...)
		case "restic backup":
			replayLines(spec.Stdout, `{"message_type":"status","percent_done":0.5,"total_files":4,"files_done":2,"total_bytes":10,"bytes_done":5}`)
			replayLines(spec.Stdout, summary...)
		}
		return proc.Result{}, nil
	}
}

// fakeMounter records mount requests; err fails them.
type fakeMounter struct {
	mu         sync.Mutex
	reqs       []mount.Request
	err        error
	unmounts   int
	unmountErr error
}

func (f *fakeMounter) Mount(_ context.Context, req mount.Request) (Mounted, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	if f.err != nil {
		return nil, f.err
	}
	return fakeMounted{f}, nil
}

type fakeMounted struct{ f *fakeMounter }

func (m fakeMounted) Unmount() error {
	m.f.mu.Lock()
	defer m.f.mu.Unlock()
	m.f.unmounts++
	return m.f.unmountErr
}

// rsEnv is a restic engine with a scripted supervisor, a real source store
// and a fake mounter.
type rsEnv struct {
	*testEnv
	sup     *scriptedSupervisor
	store   *sourcestate.Store
	mounter *fakeMounter
}

func bothMethods() *syncrv1.RemoteSourceSupport {
	return &syncrv1.RemoteSourceSupport{Mirror: true, Mount: true}
}

func newRSEnv(t *testing.T, support *syncrv1.RemoteSourceSupport) *rsEnv {
	t.Helper()
	sup := &scriptedSupervisor{}
	sup.script = defaultRun(t)
	store, err := sourcestate.New(sourcestate.Config{Root: filepath.Join(t.TempDir(), "data", "sources")})
	if err != nil {
		t.Fatal(err)
	}
	mnt := &fakeMounter{}
	te := newTestEnv(t, func(c *Config) {
		c.Supervisor = sup
		c.RemoteSources = RemoteSources{Store: store, Mounter: mnt, Support: support, MountTimeout: 30 * time.Second}
	})
	return &rsEnv{testEnv: te, sup: sup, store: store, mounter: mnt}
}

// sourceRemote is the remote that holds the folder to back up.
func sourceRemote() *syncrv1.RemoteDefinition {
	return &syncrv1.RemoteDefinition{
		Alias: "src", BackendType: "s3",
		Options: map[string]string{"provider": "Minio", "endpoint": "http://127.0.0.1:9000"},
		Secrets: map[string]string{"secret_access_key": sourceSecret},
	}
}

// remoteBackup is a mirrored backup of src:bucket/photos into the
// repository on bk, changed by mutate.
func remoteBackup(mutate ...func(b *syncrv1.ResticBackup, src *syncrv1.ResticBackupRemoteSource)) *syncrv1.JobSpec {
	src := &syncrv1.ResticBackupRemoteSource{
		Remote: "src", Path: "bucket/photos", Method: mirrorMethod, SourceKey: sourceKey,
		Filters: []*syncrv1.FilterRule{{Action: syncrv1.FilterAction_FILTER_ACTION_EXCLUDE, Pattern: "*.tmp"}},
	}
	b := &syncrv1.ResticBackup{Repository: remoteRepo(), Tags: []string{"daily", remoteTag}, Host: "nas", RemoteSource: src}
	for _, m := range mutate {
		m(b, src)
	}
	return specWith([]*syncrv1.RemoteDefinition{s3Remote(), sourceRemote()}, func(s *syncrv1.JobSpec) {
		s.Operation = &syncrv1.JobSpec_ResticBackup{ResticBackup: b}
	})
}

func (e *rsEnv) mirrorDir() string  { return filepath.Join(e.store.MirrorsDir(), sourceKey) }
func (e *rsEnv) jobScratch() string { return filepath.Join(e.scratch, "restic-job-1") }

// TestRemoteSourceMirror: check, sync into the mirror, back up "." there,
// with the phases on the progress and the stats on the output.
func TestRemoteSourceMirror(t *testing.T) {
	t.Parallel()
	e := newRSEnv(t, bothMethods())
	var filters string
	base := e.sup.script
	e.sup.script = func(ctx context.Context, spec proc.Spec) (proc.Result, error) {
		if commandOf(spec) == "rclone sync" {
			data, _ := os.ReadFile(filepath.Join(e.jobScratch(), filtersFileName))
			filters = string(data)
		}
		return base(ctx, spec)
	}
	spec := remoteBackup(func(_ *syncrv1.ResticBackup, src *syncrv1.ResticBackupRemoteSource) {
		src.Transfers = ptr[int32](8)
		src.BandwidthLimitBytesPerSecond = ptr[int64](1000)
		src.FastList = true
	})
	res, snaps, err := e.run(spec)
	if err != nil {
		t.Fatal(err)
	}
	if got := e.sup.commands(); !slices.Equal(got, []string{"rclone lsf", "rclone sync", "restic backup"}) {
		t.Fatalf("commands = %q", got)
	}

	lsf := e.sup.spec("rclone lsf")
	if want := []string{"lsf", "--config=", "--use-json-log", "--log-level=NOTICE", "--max-depth=1", "--", "src:bucket/photos"}; !slices.Equal(lsf.Args, want) {
		t.Errorf("lsf args = %q", lsf.Args)
	}
	if lsf.Env["RCLONE_CONFIG_SRC_SECRET_ACCESS_KEY"] != sourceSecret || lsf.Env["RESTIC_PASSWORD"] != "" ||
		lsf.Env["RCLONE_CACHE_DIR"] != filepath.Join(e.jobScratch(), "rclone-cache") || lsf.Env["TMPDIR"] != filepath.Join(e.jobScratch(), "rclone-tmp") ||
		lsf.Env["RCLONE_ASK_PASSWORD"] != "false" || lsf.Dir != e.scratch {
		t.Errorf("lsf env = %v, dir %q", lsf.Env, lsf.Dir)
	}
	for _, a := range lsf.Args {
		if strings.Contains(a, sourceSecret) {
			t.Error("a secret is in rclone's arguments")
		}
	}

	sync := e.sup.spec("rclone sync")
	data := filepath.Join(e.mirrorDir(), "data")
	wantSync := []string{
		"sync", "--config=", "--use-json-log", "--log-level=NOTICE", "--stats=1s", "--stats-log-level=NOTICE",
		"--create-empty-src-dirs", "--delete-excluded", "--filter-from=" + filepath.Join(e.jobScratch(), filtersFileName),
		"--transfers=8", "--bwlimit=1000B", "--fast-list", "--", "src:bucket/photos", data,
	}
	if !slices.Equal(sync.Args, wantSync) {
		t.Errorf("sync args =\n%q\nwant\n%q", sync.Args, wantSync)
	}
	if filters != "- *.tmp\n" {
		t.Errorf("filters file = %q", filters)
	}

	backup := e.sup.spec("restic backup")
	if backup.Dir != data {
		t.Errorf("restic runs in %q, want the mirror's copy %q", backup.Dir, data)
	}
	args := backup.Args
	i := slices.Index(args, "backup")
	wantArgs := []string{"backup", "--tag=daily", "--tag=" + remoteTag, "--host=nas", "--ignore-inode", "--", "."}
	if i < 0 || !slices.Equal(args[i:], wantArgs) {
		t.Errorf("restic args = %q", args)
	}
	if !slices.Contains(args, "--repo=rclone:bk:bucket/repo one") || backup.Env["RESTIC_PASSWORD"] != testPassword ||
		backup.Env["TMPDIR"] != e.jobScratch() {
		t.Errorf("restic spec = %v %v", args, backup.Env)
	}

	// Phases: the mirror's counters, then restic's, starting again.
	var phases []string
	for _, p := range snaps {
		if len(phases) == 0 || phases[len(phases)-1] != p.Phase {
			phases = append(phases, p.Phase)
		}
	}
	if !slices.Equal(phases, []string{"mirror", "backup"}) {
		t.Errorf("phases = %q", phases)
	}
	if first := snaps[0]; first.Bytes != 3008 || first.Files != 3 {
		t.Errorf("first mirror sample = %+v", first)
	}
	if i := slices.IndexFunc(snaps, func(p runnerProgress) bool { return p.Phase == "backup" }); snaps[i].Bytes != 5 || snaps[i].TotalBytes != 10 {
		t.Errorf("first backup sample = %+v", snaps[i])
	}
	if res.Stats.Phase != "backup" || res.Stats.Bytes != 10 {
		t.Errorf("final stats = %+v", res.Stats)
	}

	out := res.Output.GetResticBackup()
	rs := out.GetRemoteSource()
	if out.GetSnapshotId() == "" || rs.GetMethod() != mirrorMethod || rs.GetMirroredBytes() != 3008 || rs.GetMirroredFiles() != 3 ||
		rs.GetDeletedFiles() != 0 || rs.GetMirrorElapsed() == nil || rs.GetMirrorDiscarded() {
		t.Errorf("output = %v", out)
	}
	if _, err := os.Stat(data); err != nil {
		t.Errorf("the mirror's copy is missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.mirrorDir(), sourcestate.LastRunFileName)); err != nil {
		t.Errorf("last-run: %v", err)
	}
	// The mirror's lock was released and the scratch directory removed.
	m, err := e.store.OpenMirror(sourceKey)
	if err != nil {
		t.Fatalf("the mirror stayed locked: %v", err)
	}
	_ = m.Close()
	if _, err := os.Stat(e.jobScratch()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("scratch left: %v", err)
	}
}

// TestRemoteSourceInvalidSpec: every invalid spec fails with INVALID_SPEC
// before any process runs and before the worker's folder of sources is
// created.
func TestRemoteSourceInvalidSpec(t *testing.T) {
	t.Parallel()
	type mut = func(b *syncrv1.ResticBackup, src *syncrv1.ResticBackupRemoteSource)
	for name, m := range map[string]mut{
		"paths too":          func(b *syncrv1.ResticBackup, _ *syncrv1.ResticBackupRemoteSource) { b.Paths = []string{"/srv"} },
		"excludes":           func(b *syncrv1.ResticBackup, _ *syncrv1.ResticBackupRemoteSource) { b.Excludes = []string{"*.tmp"} },
		"use_fs_snapshot":    func(b *syncrv1.ResticBackup, _ *syncrv1.ResticBackupRemoteSource) { b.UseFsSnapshot = true },
		"one_file_system":    func(b *syncrv1.ResticBackup, _ *syncrv1.ResticBackupRemoteSource) { b.OneFileSystem = true },
		"no remote":          func(_ *syncrv1.ResticBackup, s *syncrv1.ResticBackupRemoteSource) { s.Remote = "" },
		"malformed alias":    func(_ *syncrv1.ResticBackup, s *syncrv1.ResticBackupRemoteSource) { s.Remote = "Src;rm" },
		"undefined remote":   func(_ *syncrv1.ResticBackup, s *syncrv1.ResticBackupRemoteSource) { s.Remote = "other" },
		"control in path":    func(_ *syncrv1.ResticBackup, s *syncrv1.ResticBackupRemoteSource) { s.Path = "bucket/\nphotos" },
		"dot-dot in path":    func(_ *syncrv1.ResticBackup, s *syncrv1.ResticBackupRemoteSource) { s.Path = "bucket/../etc" },
		"dot-dot, backslash": func(_ *syncrv1.ResticBackup, s *syncrv1.ResticBackupRemoteSource) { s.Path = `bucket\..\etc` },
		"no method":          func(_ *syncrv1.ResticBackup, s *syncrv1.ResticBackupRemoteSource) { s.Method = 0 },
		"unknown method":     func(_ *syncrv1.ResticBackup, s *syncrv1.ResticBackupRemoteSource) { s.Method = 9 },
		"short key":          func(_ *syncrv1.ResticBackup, s *syncrv1.ResticBackupRemoteSource) { s.SourceKey = sourceKey[:31] },
		"key escapes": func(_ *syncrv1.ResticBackup, s *syncrv1.ResticBackupRemoteSource) {
			s.SourceKey = "../../../../../../etc/x/" + sourceKey[:8]
		},
		"upper-case key": func(_ *syncrv1.ResticBackup, s *syncrv1.ResticBackupRemoteSource) {
			s.SourceKey = strings.ToUpper(sourceKey)
		},
		"bad filter": func(_ *syncrv1.ResticBackup, s *syncrv1.ResticBackupRemoteSource) { s.Filters[0].Pattern = "a\nb" },
		"discard with mount": func(_ *syncrv1.ResticBackup, s *syncrv1.ResticBackupRemoteSource) {
			s.Method, s.DiscardMirror = mountMethod, true
		},
		"transfers with mount": func(_ *syncrv1.ResticBackup, s *syncrv1.ResticBackupRemoteSource) {
			s.Method, s.Transfers = mountMethod, ptr[int32](4)
		},
		"fast_list with mount": func(_ *syncrv1.ResticBackup, s *syncrv1.ResticBackupRemoteSource) {
			s.Method, s.FastList = mountMethod, true
		},
		"too many transfers": func(_ *syncrv1.ResticBackup, s *syncrv1.ResticBackupRemoteSource) { s.Transfers = ptr[int32](65) },
		"negative bwlimit": func(_ *syncrv1.ResticBackup, s *syncrv1.ResticBackupRemoteSource) {
			s.BandwidthLimitBytesPerSecond = ptr[int64](-1)
		},
		"source holds the repository": func(_ *syncrv1.ResticBackup, s *syncrv1.ResticBackupRemoteSource) {
			s.Remote, s.Path = "bk", "bucket"
		},
		"source inside the repository": func(_ *syncrv1.ResticBackup, s *syncrv1.ResticBackupRemoteSource) {
			s.Remote, s.Path = "bk", "bucket/repo one/data"
		},
		"remote's root holds it": func(_ *syncrv1.ResticBackup, s *syncrv1.ResticBackupRemoteSource) { s.Remote, s.Path = "bk", "" },
		"tag with a comma":       func(b *syncrv1.ResticBackup, _ *syncrv1.ResticBackupRemoteSource) { b.Tags = []string{"a,b"} },
		"host with a newline":    func(b *syncrv1.ResticBackup, _ *syncrv1.ResticBackupRemoteSource) { b.Host = "a\nb" },
		"exclude_larger_than 0": func(b *syncrv1.ResticBackup, _ *syncrv1.ResticBackupRemoteSource) {
			b.ExcludeLargerThanBytes = ptr[int64](0)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newRSEnv(t, bothMethods())
			_, _, err := e.run(remoteBackup(m))
			mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC)
			if got := e.sup.commands(); len(got) != 0 {
				t.Errorf("ran %q", got)
			}
			if _, err := os.Stat(e.store.Root()); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the folder of sources was created: %v", err)
			}
			if _, err := os.Stat(e.jobScratch()); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a scratch directory was created: %v", err)
			}
		})
	}
	// The same source on another remote than the repository's is fine.
	e := newRSEnv(t, bothMethods())
	if _, _, err := e.run(remoteBackup(func(_ *syncrv1.ResticBackup, s *syncrv1.ResticBackupRemoteSource) { s.Path = "bucket" })); err != nil {
		t.Errorf("a source on another remote: %v", err)
	}
}

// TestRemoteSourceUnavailableMethod: a method the worker cannot use fails
// with TOOL_UNAVAILABLE and its reason, before anything runs.
func TestRemoteSourceUnavailableMethod(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		support *syncrv1.RemoteSourceSupport
		method  syncrv1.ResticRemoteSourceMethod
		want    string
	}{
		"no report":  {nil, mirrorMethod, "this worker does not keep mirrors of remotes: this worker does not report it"},
		"mirror off": {&syncrv1.RemoteSourceSupport{MirrorReason: "remote_sources.mirror is off in worker.yaml", Mount: true}, mirrorMethod, "remote_sources.mirror is off in worker.yaml"},
		"no FUSE":    {&syncrv1.RemoteSourceSupport{Mirror: true, MountReason: "/dev/fuse is missing"}, mountMethod, "this worker cannot mount remotes: /dev/fuse is missing"},
	} {
		e := newRSEnv(t, tt.support)
		_, _, err := e.run(remoteBackup(func(_ *syncrv1.ResticBackup, s *syncrv1.ResticBackupRemoteSource) { s.Method = tt.method }))
		mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_TOOL_UNAVAILABLE)
		if !strings.Contains(err.Error(), tt.want) || len(e.sup.commands()) != 0 {
			t.Errorf("%s: %v (ran %q)", name, err, e.sup.commands())
		}
	}
}

// TestRemoteSourcePrecheck: the listing decides before anything changes on
// the worker: NOT_FOUND for a missing folder, SOURCE_EMPTY for an empty
// one (unless allowed), the transfer classification otherwise.
func TestRemoteSourcePrecheck(t *testing.T) {
	t.Parallel()
	notFound := `{"time":"2026-11-14T01:54:01.216227661Z","level":"error","msg":"error listing: directory not found","object":"","objectType":"string","source":"walk/walk.go:175"}`
	for name, tt := range map[string]struct {
		exit       int
		stdout     []string
		stderr     []string
		allowEmpty bool
		code       syncrv1.JobErrorCode
		want       string
	}{
		"missing": {exit: 3, stderr: []string{notFound}, code: syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND,
			want: `the folder "bucket/photos" on the remote does not exist: rclone lsf failed: directory not found (exit code 3): error listing: directory not found`},
		"empty": {code: syncrv1.JobErrorCode_JOB_ERROR_CODE_SOURCE_EMPTY, want: `the folder "bucket/photos" on the remote is empty`},
		"temporary": {exit: 5, stderr: []string{`{"level":"error","msg":"error listing: exceeded maximum number of attempts"}`},
			code: syncrv1.JobErrorCode_JOB_ERROR_CODE_TEMPORARY, want: "cannot list the folder"},
		"denied": {exit: 1, stderr: []string{`{"level":"error","msg":"error listing: SignatureDoesNotMatch for key ` + sourceSecret + `"}`},
			code: syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, want: "SignatureDoesNotMatch for key [redacted]"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newRSEnv(t, bothMethods())
			e.sup.script = func(_ context.Context, spec proc.Spec) (proc.Result, error) {
				replayLines(spec.Stdout, tt.stdout...)
				replayLines(spec.Stderr, tt.stderr...)
				if tt.exit != 0 {
					return proc.Result{ExitCode: tt.exit}, &proc.ExitError{Path: spec.Path, ExitCode: tt.exit}
				}
				return proc.Result{}, nil
			}
			_, _, err := e.run(remoteBackup())
			mustFail(t, err, tt.code)
			if !strings.Contains(err.Error(), tt.want) || strings.Contains(err.Error(), sourceSecret) {
				t.Errorf("error = %v", err)
			}
			if got := e.sup.commands(); !slices.Equal(got, []string{"rclone lsf"}) {
				t.Errorf("commands = %q", got)
			}
			if _, err := os.Stat(e.store.Root()); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the mirror was touched: %v", err)
			}
		})
	}

	// allow_empty backs an empty folder up; the mirror is created.
	e := newRSEnv(t, bothMethods())
	base := e.sup.script
	e.sup.script = func(ctx context.Context, spec proc.Spec) (proc.Result, error) {
		if commandOf(spec) == "rclone lsf" {
			return proc.Result{}, nil
		}
		return base(ctx, spec)
	}
	if _, _, err := e.run(remoteBackup(func(_ *syncrv1.ResticBackup, s *syncrv1.ResticBackupRemoteSource) { s.AllowEmpty = true })); err != nil {
		t.Fatal(err)
	}
	if got := e.sup.commands(); !slices.Equal(got, []string{"rclone lsf", "rclone sync", "restic backup"}) {
		t.Errorf("commands = %q", got)
	}
	if info, err := os.Stat(filepath.Join(e.mirrorDir(), "data")); err != nil || !info.IsDir() {
		t.Errorf("the empty mirror's folder: %v", err)
	}
}

// TestRemoteSourceSyncFails: a failed sync fails the job without a backup
// and keeps what the mirror holds; a full disk says so.
func TestRemoteSourceSyncFails(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		line string
		want string
	}{
		"remote error": {`{"level":"error","msg":"Failed to copy: read: connection reset by peer","object":"a.bin"}`,
			`the mirror of the folder "bucket/photos" on the remote could not be brought up to date, so nothing was backed up: rclone sync failed: error (exit code 1): a.bin: Failed to copy`},
		"disk full": {`{"level":"error","msg":"Failed to copy: write /data/sources/mirror/k/data/a.bin: no space left on device","object":"a.bin"}`,
			"the worker's disk is full: the mirror of the folder does not fit in "},
		"disk full on Windows": {`{"level":"error","msg":"Failed to copy: There is not enough space on the disk.","object":"a.bin"}`,
			"the worker's disk is full"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newRSEnv(t, bothMethods())
			kept := filepath.Join(e.mirrorDir(), "data", "old.bin")
			putFile(t, kept, []byte("from the last run"))
			base := e.sup.script
			e.sup.script = func(ctx context.Context, spec proc.Spec) (proc.Result, error) {
				if commandOf(spec) == "rclone sync" {
					replayLines(spec.Stderr, tt.line)
					return proc.Result{ExitCode: 1}, &proc.ExitError{Path: spec.Path, ExitCode: 1}
				}
				return base(ctx, spec)
			}
			res, _, err := e.run(remoteBackup())
			mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED)
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v", err)
			}
			if got := e.sup.commands(); !slices.Equal(got, []string{"rclone lsf", "rclone sync"}) {
				t.Errorf("commands = %q", got)
			}
			if _, err := os.Stat(kept); err != nil {
				t.Errorf("the partial mirror was not kept: %v", err)
			}
			if res.Output != nil || res.Stats.Phase != "mirror" || res.ExitCode != 1 {
				t.Errorf("result = %+v", res)
			}
		})
	}
}

func TestRemoteSourceMirrorBusy(t *testing.T) {
	t.Parallel()
	e := newRSEnv(t, bothMethods())
	m, err := e.store.OpenMirror(sourceKey)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()
	_, _, err = e.run(remoteBackup())
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_TEMPORARY)
	if got := e.sup.commands(); !slices.Equal(got, []string{"rclone lsf"}) {
		t.Errorf("commands = %q", got)
	}
}

// TestRemoteSourceDiscard: with discard_mirror the copy goes after the run,
// after a failed backup too.
func TestRemoteSourceDiscard(t *testing.T) {
	t.Parallel()
	discard := func(_ *syncrv1.ResticBackup, s *syncrv1.ResticBackupRemoteSource) { s.DiscardMirror = true }

	e := newRSEnv(t, bothMethods())
	res, _, err := e.run(remoteBackup(discard))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Output.GetResticBackup().GetRemoteSource().GetMirrorDiscarded() {
		t.Errorf("output = %v", res.Output)
	}
	if _, err := os.Stat(filepath.Join(e.mirrorDir(), "data")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the copy was kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.mirrorDir(), sourcestate.LastRunFileName)); err != nil {
		t.Errorf("last-run: %v", err)
	}

	e = newRSEnv(t, bothMethods())
	base := e.sup.script
	e.sup.script = func(ctx context.Context, spec proc.Spec) (proc.Result, error) {
		if commandOf(spec) == "restic backup" {
			putFile(t, filepath.Join(e.mirrorDir(), "data", "a.bin"), []byte("x"))
			return proc.Result{ExitCode: 1}, &proc.ExitError{Path: spec.Path, ExitCode: 1}
		}
		return base(ctx, spec)
	}
	_, _, err = e.run(remoteBackup(discard))
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED)
	if _, err := os.Stat(filepath.Join(e.mirrorDir(), "data")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the copy was kept after a failed backup: %v", err)
	}
}

// TestRemoteSourceDryRun: the mirror is brought up to date for real; restic
// runs with --dry-run.
func TestRemoteSourceDryRun(t *testing.T) {
	t.Parallel()
	e := newRSEnv(t, bothMethods())
	res, _, err := e.run(remoteBackup(func(b *syncrv1.ResticBackup, _ *syncrv1.ResticBackupRemoteSource) { b.DryRun = true }))
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(e.sup.spec("rclone sync").Args, "--dry-run") || !slices.Contains(e.sup.spec("restic backup").Args, "--dry-run") {
		t.Errorf("sync %q, backup %q", e.sup.spec("rclone sync").Args, e.sup.spec("restic backup").Args)
	}
	if out := res.Output.GetResticBackup(); out.GetSnapshotId() != "" || out.GetRemoteSource().GetMirroredBytes() != 3008 {
		t.Errorf("output = %v", out)
	}
}

// TestRemoteSourceMount: mount, back up "." in the mount, unmount; the
// mirror's stats stay zero and only the backup phase is reported.
func TestRemoteSourceMount(t *testing.T) {
	t.Parallel()
	e := newRSEnv(t, bothMethods())
	spec := remoteBackup(func(_ *syncrv1.ResticBackup, s *syncrv1.ResticBackupRemoteSource) {
		s.Method = mountMethod
		s.BandwidthLimitBytesPerSecond = ptr[int64](2048)
	})
	res, snaps, err := e.run(spec)
	if err != nil {
		t.Fatal(err)
	}
	if got := e.sup.commands(); !slices.Equal(got, []string{"rclone lsf", "restic backup"}) {
		t.Fatalf("commands = %q", got)
	}
	point := filepath.Join(e.store.MountsDir(), sourceKey)
	if len(e.mounter.reqs) != 1 || e.mounter.unmounts != 1 {
		t.Fatalf("mounts %d, unmounts %d", len(e.mounter.reqs), e.mounter.unmounts)
	}
	req := e.mounter.reqs[0]
	if req.Remote != "src:bucket/photos" || req.Point != point || req.DevName != "syncr-"+sourceKey || req.Timeout != 30*time.Second ||
		req.Dir != e.scratch || req.Env["RCLONE_CONFIG_SRC_SECRET_ACCESS_KEY"] != sourceSecret || req.Scrub == nil ||
		!slices.Equal(req.Flags, []string{"--filter-from=" + filepath.Join(e.jobScratch(), filtersFileName), "--bwlimit=2048B"}) {
		t.Errorf("mount request = %+v", req)
	}
	if dir := e.sup.spec("restic backup").Dir; dir != point {
		t.Errorf("restic runs in %q, want the mount point", dir)
	}
	for _, p := range snaps {
		if p.Phase != "backup" {
			t.Errorf("a %q sample with the mount method", p.Phase)
		}
	}
	rs := res.Output.GetResticBackup().GetRemoteSource()
	if rs.GetMethod() != mountMethod || rs.GetMirroredBytes() != 0 || rs.GetMirrorElapsed() != nil || rs.GetMirrorDiscarded() {
		t.Errorf("remote source stats = %v", rs)
	}
	// The mount point's lock was released.
	l, err := e.store.LockMount(sourceKey)
	if err != nil {
		t.Fatalf("the mount point stayed locked: %v", err)
	}
	_ = l.Close()
}

// TestRemoteSourceMountFails: a machine that cannot mount is
// TOOL_UNAVAILABLE, any other mount failure EXECUTION_FAILED; restic does
// not run.
func TestRemoteSourceMountFails(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		err  error
		code syncrv1.JobErrorCode
	}{
		"no FUSE":    {&mount.Error{Unavailable: true, Msg: "rclone mount failed (exit code 1): mount helper error: fusermount3: fuse device not found"}, syncrv1.JobErrorCode_JOB_ERROR_CODE_TOOL_UNAVAILABLE},
		"not ready":  {&mount.Error{Msg: "the remote was not mounted within 30s (remote_sources.mount_timeout)"}, syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED},
		"other":      {errors.New("mount: the mount point is not absolute"), syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL},
		"secret out": {&mount.Error{Msg: "rclone mount failed: key " + sourceSecret}, syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED},
	} {
		e := newRSEnv(t, bothMethods())
		e.mounter.err = tt.err
		_, _, err := e.run(remoteBackup(func(_ *syncrv1.ResticBackup, s *syncrv1.ResticBackupRemoteSource) { s.Method = mountMethod }))
		mustFail(t, err, tt.code)
		if got := e.sup.commands(); !slices.Equal(got, []string{"rclone lsf"}) || strings.Contains(err.Error(), sourceSecret) {
			t.Errorf("%s: commands %q, error %v", name, got, err)
		}
	}
}

// TestRemoteSourceUnmountsOnEveryPath: after a failed backup, a cancelled
// one, and a successful one whose unmount fails (a warning).
func TestRemoteSourceUnmountsOnEveryPath(t *testing.T) {
	t.Parallel()
	mountSpec := func() *syncrv1.JobSpec {
		return remoteBackup(func(_ *syncrv1.ResticBackup, s *syncrv1.ResticBackupRemoteSource) { s.Method = mountMethod })
	}

	e := newRSEnv(t, bothMethods())
	base := e.sup.script
	e.sup.script = func(ctx context.Context, spec proc.Spec) (proc.Result, error) {
		if commandOf(spec) == "restic backup" {
			return proc.Result{ExitCode: 1}, &proc.ExitError{Path: spec.Path, ExitCode: 1}
		}
		return base(ctx, spec)
	}
	_, _, err := e.run(mountSpec())
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED)
	if e.mounter.unmounts != 1 {
		t.Errorf("unmounts after a failed backup = %d", e.mounter.unmounts)
	}

	e = newRSEnv(t, bothMethods())
	ctx, cancel := context.WithCancel(t.Context())
	base = e.sup.script
	e.sup.script = func(c context.Context, spec proc.Spec) (proc.Result, error) {
		if commandOf(spec) == "restic backup" {
			cancel()
			return proc.Result{ExitCode: -1}, &proc.CanceledError{Path: spec.Path, ExitCode: -1, Cause: context.Canceled}
		}
		return base(c, spec)
	}
	r := e.runners[kindBackup]
	if _, err := r.Run(ctx, runnerJob(mountSpec()), nil); !runner.Canceled(err) {
		t.Errorf("cancelled run = %v", err)
	}
	if e.mounter.unmounts != 1 {
		t.Errorf("unmounts after a cancel = %d", e.mounter.unmounts)
	}

	e = newRSEnv(t, bothMethods())
	e.mounter.unmountErr = errors.New("/data/sources/mount/k is still mounted")
	res, _, err := e.run(mountSpec())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "could not be unmounted") || e.mounter.unmounts != 1 {
		t.Errorf("warnings = %q, unmounts %d", res.Warnings, e.mounter.unmounts)
	}

	e = newRSEnv(t, bothMethods())
	l, err := e.store.LockMount(sourceKey)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	_, _, err = e.run(mountSpec())
	mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_TEMPORARY)
	if len(e.mounter.reqs) != 0 {
		t.Error("mounted while another run held the mount point")
	}
}

// TestPathsBackupLeavesSourcesOut: a backup of paths that hold the
// folder of sources passes --exclude-if-present with its marker; others
// are unchanged.
func TestPathsBackupLeavesSourcesOut(t *testing.T) {
	t.Parallel()
	var store *sourcestate.Store
	te := newTestEnv(t, func(c *Config) {
		var err error
		policy, ok := c.Paths.(rootPolicy)
		if !ok {
			t.Fatalf("paths = %T", c.Paths)
		}
		store, err = sourcestate.New(sourcestate.Config{Root: filepath.Join(policy.root, "var", "lib", "syncr", "sources")})
		if err != nil {
			t.Fatal(err)
		}
		c.RemoteSources.Store = store
	})
	for _, tt := range []struct {
		path string
		want bool
	}{
		{te.local("srv"), false},
		{te.local(), true},
		{filepath.Dir(store.Root()), true},
		{filepath.Dir(filepath.Dir(store.Root())), true},
		{filepath.Join(filepath.Dir(store.Root()), "scratch"), false},
	} {
		te.sup.result = proc.Result{}
		te.sup.stdout = fixture(t, "backup_summary.out")
		if _, _, err := te.run(backupSpec(remoteRepo(), &syncrv1.ResticBackup{Paths: []string{tt.path}})); err != nil {
			t.Fatal(err)
		}
		args := te.sup.spec.Args
		i := slices.Index(args, "--")
		has := slices.Contains(args, "--exclude-if-present=.syncr-sources")
		if has != tt.want || (has && slices.Index(args, "--exclude-if-present=.syncr-sources") > i) || args[i+1] != tt.path {
			t.Errorf("%s: args %q", tt.path, args)
		}
	}
}

func ptr[T any](v T) *T { return &v }

func putFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRemoteSourceEmptySnapshot: restic 0.19.1 makes no snapshot of a
// folder without any entry. With allow_empty and an empty folder the run
// succeeds without one, and says so; when only the filters leave nothing,
// it fails.
func TestRemoteSourceEmptySnapshot(t *testing.T) {
	t.Parallel()
	exitEmpty := `{"message_type":"exit_error","code":1,"message":"Fatal: unable to save snapshot: snapshot is empty"}`
	for name, tt := range map[string]struct {
		entries []string
		ok      bool
	}{
		"empty folder allowed":         {nil, true},
		"filters leave nothing behind": {[]string{"only.tmp"}, false},
	} {
		e := newRSEnv(t, bothMethods())
		base := e.sup.script
		e.sup.script = func(ctx context.Context, spec proc.Spec) (proc.Result, error) {
			switch commandOf(spec) {
			case "rclone lsf":
				replayLines(spec.Stdout, tt.entries...)
				return proc.Result{}, nil
			case "restic backup":
				replayLines(spec.Stdout, exitEmpty)
				return proc.Result{ExitCode: 1}, &proc.ExitError{Path: spec.Path, ExitCode: 1}
			}
			return base(ctx, spec)
		}
		res, _, err := e.run(remoteBackup(func(_ *syncrv1.ResticBackup, s *syncrv1.ResticBackupRemoteSource) { s.AllowEmpty = true }))
		if !tt.ok {
			mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED)
			if !strings.Contains(err.Error(), "the filters leave out everything") {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out := res.Output.GetResticBackup()
		if out.GetSnapshotId() != "" || out.GetRemoteSource().GetMirroredBytes() != 3008 || len(res.Warnings) != 1 ||
			res.Warnings[0] != `the folder "bucket/photos" on the remote is empty and restic makes no empty snapshot, so no snapshot was made` {
			t.Errorf("%s: output %v, warnings %q", name, out, res.Warnings)
		}
	}
}

// TestRemoteSourceMirrorDeletes reads what a sync deleted from rclone
// v1.75.2's stats (a second sync after a deletion and a new filter).
func TestRemoteSourceMirrorDeletes(t *testing.T) {
	t.Parallel()
	e := newRSEnv(t, bothMethods())
	again := fixture(t, "rclone-1.75.2_sync_mirror_again.jsonl")
	base := e.sup.script
	e.sup.script = func(ctx context.Context, spec proc.Spec) (proc.Result, error) {
		if commandOf(spec) == "rclone sync" {
			replayLines(spec.Stderr, again...)
			return proc.Result{}, nil
		}
		return base(ctx, spec)
	}
	res, snaps, err := e.run(remoteBackup())
	if err != nil {
		t.Fatal(err)
	}
	rs := res.Output.GetResticBackup().GetRemoteSource()
	if rs.GetDeletedFiles() != 2 || rs.GetMirroredFiles() != 0 || rs.GetMirroredBytes() != 0 {
		t.Errorf("remote source stats = %v", rs)
	}
	if snaps[0].Phase != "mirror" || snaps[0].Deletes != 2 || snaps[0].Checks != 3 {
		t.Errorf("mirror sample = %+v", snaps[0])
	}
}
