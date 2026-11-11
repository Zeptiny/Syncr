//go:build integration

package rclone

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"golang.org/x/crypto/ssh"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/localfs"
	"github.com/Zeptiny/Syncr/internal/worker/probe"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/rcloneenv"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// Integration tests run real transfers with the rclone binary in
// SYNCR_TEST_RCLONE through the real supervisor, env builder and path
// policy. MinIO and SFTP tests also need Docker; they use the images of
// deploy/docker-compose.dev.yml.

const (
	minioImage = "pgsty/minio:RELEASE.2026-08-04T00-00-00Z" // see deploy/docker-compose.dev.yml
	sftpImage  = "atmoz/sftp:alpine"
)

type itest struct {
	root    string // the local root; test data lives below it
	scratch string
	runner  *Runner
	// cfg is the runner's Config, for a Prober on the same tools.
	cfg Config
}

// prober returns a Prober with the test's configuration.
func (it *itest) prober(t *testing.T) *Prober {
	t.Helper()
	p, err := NewProber(it.cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// probeRemote checks the probe operations on a remote that holds the tree
// of roundTrip at path: a listing of path (directories first), a bounded
// listing, a missing folder, and the usage.
func probeRemote(t *testing.T, it *itest, def *syncrv1.RemoteDefinition, path, missing string, wantUsage bool) {
	t.Helper()
	const id = "0195f0a4-3b7c-7d1e-9f00-0000000000dd"
	p := it.prober(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	remotes := []*syncrv1.RemoteDefinition{def}
	loc := &syncrv1.Location{Remote: def.GetAlias(), Path: path}

	l, err := p.List(ctx, id, remotes, &syncrv1.ProbeListRemote{Location: loc})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var got []string
	for _, e := range l.GetEntries() {
		got = append(got, e.GetType().String()[len("PROBE_ENTRY_TYPE_"):]+" "+e.GetPath())
	}
	if want := []string{"DIR " + path + "/dir", "FILE " + path + "/a.txt"}; !slices.Equal(got, want) || l.GetTruncated() {
		t.Errorf("listing = %v, want %v", got, want)
	}
	if f := l.GetEntries()[1]; f.GetSize() != 100 || f.GetModTime() == nil {
		t.Errorf("file entry = %v", f)
	}
	sub, err := p.List(ctx, id, remotes, &syncrv1.ProbeListRemote{Location: &syncrv1.Location{Remote: def.GetAlias(), Path: path + "/dir"}, MaxEntries: 1})
	if err != nil || len(sub.GetEntries()) != 1 || !sub.GetTruncated() {
		t.Errorf("bounded listing = %v, %v; want 1 of 2 entries, truncated", sub, err)
	}
	dirs, err := p.List(ctx, id, remotes, &syncrv1.ProbeListRemote{Location: loc, DirsOnly: true})
	if err != nil || len(dirs.GetEntries()) != 1 || dirs.GetEntries()[0].GetName() != "dir" {
		t.Errorf("dirs only = %v, %v", dirs, err)
	}
	_, err = p.List(ctx, id, remotes, &syncrv1.ProbeListRemote{Location: &syncrv1.Location{Remote: def.GetAlias(), Path: missing}})
	if err == nil || runner.CodeOf(err) != syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND {
		t.Errorf("missing folder %s: %v (code %v), want NOT_FOUND", missing, err, runner.CodeOf(err))
	}

	u, err := p.About(ctx, id, remotes, &syncrv1.ProbeAbout{Location: loc})
	if err != nil {
		t.Fatalf("about: %v", err)
	}
	if wantUsage == u.GetUnsupported() || (wantUsage && u.Total == nil && u.Used == nil && u.Free == nil) {
		t.Errorf("usage = %v, want usage reported: %v", u, wantUsage)
	}

	// A wrong secret fails without leaking it.
	bad := cloneDef(def)
	for k := range bad.Secrets {
		bad.Secrets[k] = "wrong-" + k + "-Secret-Value"
	}
	_, err = p.List(ctx, id, []*syncrv1.RemoteDefinition{bad}, &syncrv1.ProbeListRemote{Location: loc})
	if err == nil {
		t.Fatal("listing with wrong credentials succeeded")
	}
	for _, v := range bad.Secrets {
		if strings.Contains(err.Error(), v) {
			t.Errorf("error leaks a secret: %v", err)
		}
	}
	if left, _ := os.ReadDir(it.scratch); len(left) != 0 {
		t.Errorf("scratch entries left by the probes: %v", left)
	}
}

func newITest(t *testing.T) *itest {
	t.Helper()
	bin := os.Getenv("SYNCR_TEST_RCLONE")
	if bin == "" {
		t.Skip("SYNCR_TEST_RCLONE not set")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	it := &itest{root: filepath.Join(base, "data"), scratch: filepath.Join(base, "scratch")}
	if err := os.MkdirAll(it.root, 0o700); err != nil {
		t.Fatal(err)
	}
	policy, err := localfs.NewPolicy([]string{it.root})
	if err != nil {
		t.Fatal(err)
	}
	it.cfg = Config{
		Binary:     bin,
		Supervisor: proc.New(proc.Options{GracePeriod: 10 * time.Second}),
		Env:        rcloneenv.NewBuilder(policy),
		Paths:      policy,
		ScratchDir: it.scratch,
	}
	it.runner, err = New(it.cfg)
	if err != nil {
		t.Fatal(err)
	}
	return it
}

// dir returns a new directory below the local root.
func (it *itest) dir(t *testing.T, name string) string {
	t.Helper()
	d := filepath.Join(it.root, name)
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}

// run executes a transfer and returns the result, the emitted progress and
// the error. It checks that the scratch dir is cleaned up.
func (it *itest) run(ctx context.Context, t *testing.T, mode syncrv1.RcloneTransferMode, src, dst *syncrv1.Location, opts *syncrv1.RcloneTransferOptions, remotes ...*syncrv1.RemoteDefinition) (runner.Result, []runner.Progress, error) {
	t.Helper()
	job := runner.Job{ID: "it-" + t.Name(), Spec: &syncrv1.JobSpec{
		Remotes: remotes,
		Operation: &syncrv1.JobSpec_RcloneTransfer{RcloneTransfer: &syncrv1.RcloneTransfer{
			Mode: mode, Source: src, Destination: dst, Options: opts,
		}},
	}}
	var progress []runner.Progress
	res, err := it.runner.Run(ctx, job, func(p runner.Progress) { progress = append(progress, p) })
	if entries, _ := os.ReadDir(it.scratch); len(entries) != 0 {
		t.Errorf("scratch dir not cleaned up: %v", entries)
	}
	return res, progress, err
}

func local(path string) *syncrv1.Location { return &syncrv1.Location{Path: path} }

// quirky returns a file or directory name with characters that would break a
// shell, a connection string or an INI file. Windows forbids the double quote
// in names, so there it is left out; the other characters stay.
func quirky(name string) string {
	if runtime.GOOS == "windows" {
		return strings.ReplaceAll(name, `"`, "")
	}
	return name
}

// requireContainers skips the test where Docker cannot run Linux containers:
// on Windows, GitHub's runners only offer Windows containers, and a
// developer's Docker may be unavailable or in Windows-container mode.
func requireContainers(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("skipping: testcontainers needs Linux containers, and Windows CI runners only run Windows containers")
	}
	testcontainers.SkipIfProviderIsNotHealthy(t)
}

// writeFiles creates files with random content and returns their total size.
func writeFiles(t *testing.T, root string, files map[string]int) int64 {
	t.Helper()
	var total int64
	for name, size := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		data := make([]byte, size)
		_, _ = rand.Read(data)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		total += int64(size)
	}
	return total
}

// tree returns the files below root (slash-separated relative paths) and
// their contents.
func tree(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		files[filepath.ToSlash(rel)] = data
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func sameTree(t *testing.T, got, want map[string][]byte) {
	t.Helper()
	if !maps.EqualFunc(got, want, bytes.Equal) {
		t.Errorf("tree has %v, want %v", slicesOfKeys(got), slicesOfKeys(want))
	}
}

func slicesOfKeys(m map[string][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func TestIntegrationLocalCopySyncMove(t *testing.T) {
	it := newITest(t)
	// The worker's own RCLONE_* variables must not reach rclone: if
	// RCLONE_DRY_RUN leaked, nothing would be copied.
	t.Setenv("RCLONE_DRY_RUN", "true")
	t.Setenv("RCLONE_CONFIG_SRC_TYPE", "memory")

	src, dst := it.dir(t, "src"), it.dir(t, "dst")
	total := writeFiles(t, src, map[string]int{"a.txt": 10, "sub/b.bin": 300_000, "sub/deeper/c.bin": 70_000, quirky("with space 'q\".txt"): 5})
	want := tree(t, src)

	// copy
	res, progress, err := it.run(t.Context(), t, modeCopy, local(src), local(dst), nil)
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	sameTree(t, tree(t, dst), want)
	if res.ExitCode != 0 || res.Stats.Bytes != total || res.Stats.Files != 4 || res.Stats.TotalFiles != 4 {
		t.Errorf("copy result = %+v, want %d bytes in 4 files", res, total)
	}
	if len(progress) == 0 || progress[len(progress)-1].Bytes != total {
		t.Errorf("copy progress = %+v", progress)
	}

	// sync deletes extra destination files and propagates source deletes.
	writeFiles(t, dst, map[string]int{"extra.txt": 3, "sub/extra.bin": 4})
	if err := os.Remove(filepath.Join(src, "a.txt")); err != nil {
		t.Fatal(err)
	}
	delete(want, "a.txt")
	res, _, err = it.run(t.Context(), t, modeSync, local(src), local(dst), nil)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	sameTree(t, tree(t, dst), want)
	if res.Stats.Deletes != 3 {
		t.Errorf("sync deletes = %d, want 3", res.Stats.Deletes)
	}

	// move empties the source, including its directories.
	moved := it.dir(t, "moved")
	_, _, err = it.run(t.Context(), t, modeMove, local(src), local(moved), &syncrv1.RcloneTransferOptions{DeleteEmptySrcDirs: true})
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	sameTree(t, tree(t, moved), want)
	if left := tree(t, src); len(left) != 0 {
		t.Errorf("move left %v in the source", slicesOfKeys(left))
	}
	if _, err := os.Stat(filepath.Join(src, "sub")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("empty source dirs not deleted: %v", err)
	}
}

func TestIntegrationFiltersAndDryRun(t *testing.T) {
	it := newITest(t)
	src, dst := it.dir(t, "src"), it.dir(t, "dst")
	writeFiles(t, src, map[string]int{"keep.txt": 1, "skip.tmp": 1, "photos/a.jpg": 1, "photos/raw/b.cr2": 1})

	_, _, err := it.run(t.Context(), t, modeCopy, local(src), local(dst), &syncrv1.RcloneTransferOptions{DryRun: true})
	if err != nil || len(tree(t, dst)) != 0 {
		t.Fatalf("dry run: err %v, copied %v", err, slicesOfKeys(tree(t, dst)))
	}

	_, _, err = it.run(t.Context(), t, modeCopy, local(src), local(dst), &syncrv1.RcloneTransferOptions{
		Filters: []*syncrv1.FilterRule{exclude("*.tmp"), exclude("/photos/raw/**"), include("**")},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := tree(t, dst)
	if len(got) != 2 || got["keep.txt"] == nil || got["photos/a.jpg"] == nil {
		t.Errorf("filtered copy = %v", slicesOfKeys(got))
	}
}

func TestIntegrationProgressEvents(t *testing.T) {
	it := newITest(t)
	src, dst := it.dir(t, "src"), it.dir(t, "dst")
	total := writeFiles(t, src, map[string]int{"big.bin": 3 << 20, "small.bin": 1000})

	start := time.Now()
	res, progress, err := it.run(t.Context(), t, modeCopy, local(src), local(dst), &syncrv1.RcloneTransferOptions{
		BandwidthLimitBytesPerSecond: ptr[int64](1 << 20),
	})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 2*time.Second {
		t.Errorf("bandwidth limit not applied: took %v", time.Since(start))
	}
	if len(progress) < 3 {
		t.Fatalf("got %d progress events, want one per second", len(progress))
	}
	for i := 1; i < len(progress); i++ {
		prev, cur := progress[i-1], progress[i]
		if cur.Bytes < prev.Bytes || cur.Files < prev.Files || cur.Elapsed < prev.Elapsed {
			t.Errorf("progress went backwards: %+v -> %+v", prev, cur)
		}
	}
	if first := progress[0]; first.Bytes == 0 || first.Bytes >= total || first.TotalBytes != total {
		t.Errorf("first sample = %+v, want partial progress of %d bytes", first, total)
	}
	if res.Stats.Bytes != total || res.Stats.Elapsed < 2*time.Second {
		t.Errorf("final stats = %+v", res.Stats)
	}
}

func TestIntegrationCancelMidTransfer(t *testing.T) {
	tests := []struct {
		name string
		size int64
		dst  func(it *itest) *syncrv1.Location
		// graceful: rclone exits on SIGINT with code 130. It installs its
		// SIGINT handler only when a backend registers cleanup work (the
		// local backend removes partial files); otherwise the default
		// action kills it, which is fine as there is nothing to clean up.
		// Windows has no SIGINT for console programs: the supervisor kills
		// the process, so there only "terminated, not successful" is checked.
		graceful bool
	}{
		{"local destination", 32 << 20, func(it *itest) *syncrv1.Location { return local(filepath.Join(it.root, "dst")) }, true},
		// A 4 GiB sparse source streamed into memory: no disk space needed
		// (rclone preallocates local destination files).
		{"large sparse source", 4 << 30, func(*itest) *syncrv1.Location { return &syncrv1.Location{Remote: "mem", Path: "bucket"} }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			it := newITest(t)
			src := it.dir(t, "src")
			f, err := os.Create(filepath.Join(src, "huge.bin"))
			if err != nil {
				t.Fatal(err)
			}
			// Unix filesystems make the extended file sparse by themselves;
			// NTFS needs it asked for, or Truncate would allocate the size.
			if err := markSparse(f); err != nil {
				t.Logf("cannot mark the file sparse (it will occupy %d bytes): %v", tt.size, err)
			}
			if err := f.Truncate(tt.size); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			job := runner.Job{ID: "cancel", Spec: &syncrv1.JobSpec{
				Remotes: []*syncrv1.RemoteDefinition{{Alias: "mem", BackendType: "memory"}},
				Operation: &syncrv1.JobSpec_RcloneTransfer{RcloneTransfer: &syncrv1.RcloneTransfer{
					Mode: modeCopy, Source: local(src), Destination: tt.dst(it),
					Options: &syncrv1.RcloneTransferOptions{BandwidthLimitBytesPerSecond: ptr[int64](2 << 20)},
				}},
			}}
			var samples int
			start := time.Now()
			res, err := it.runner.Run(ctx, job, func(p runner.Progress) {
				samples++
				if p.Bytes > 0 {
					cancel() // the transfer is under way
				}
			})
			if !runner.Canceled(err) {
				t.Fatalf("Run() = %v, want a cancellation", err)
			}
			if _, ok := errors.AsType[*proc.CanceledError](err); !ok {
				t.Errorf("err = %T, want *proc.CanceledError", err)
			}
			if elapsed := time.Since(start); elapsed > 15*time.Second {
				t.Errorf("cancel took %v", elapsed)
			}
			switch {
			case runtime.GOOS == "windows":
				if res.ExitCode == 0 {
					t.Errorf("exit code 0 after a cancelled transfer; the process should have been killed")
				}
			case tt.graceful && res.ExitCode != 130:
				t.Errorf("exit code %d, want 130 (rclone stopped on SIGINT)", res.ExitCode)
			}
			if samples == 0 || res.Stats.Bytes == 0 || res.Stats.Bytes >= tt.size {
				t.Errorf("stats = %+v after %d samples", res.Stats, samples)
			}
			if entries, _ := os.ReadDir(it.scratch); len(entries) != 0 {
				t.Errorf("scratch dir not cleaned up: %v", entries)
			}
		})
	}
}

func TestIntegrationLimitReached(t *testing.T) {
	it := newITest(t)
	src, dst := it.dir(t, "src"), it.dir(t, "dst")
	writeFiles(t, src, map[string]int{"a.bin": 2 << 20, "b.bin": 2 << 20})
	_, _, err := it.run(t.Context(), t, modeCopy, local(src), local(dst), &syncrv1.RcloneTransferOptions{
		MaxTransferBytes: ptr[int64](1 << 20), Transfers: ptr[int32](1),
	})
	if runner.CodeOf(err) != syncrv1.JobErrorCode_JOB_ERROR_CODE_LIMIT_REACHED {
		t.Errorf("err = %v (code %v), want LIMIT_REACHED", err, runner.CodeOf(err))
	}
}

func TestIntegrationMissingSource(t *testing.T) {
	it := newITest(t)
	_, _, err := it.run(t.Context(), t, modeCopy, local(filepath.Join(it.root, "missing")), local(it.dir(t, "dst")), &syncrv1.RcloneTransferOptions{Retries: ptr[int32](1)})
	if runner.CodeOf(err) != syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND || !strings.Contains(err.Error(), "directory not found") {
		t.Errorf("err = %v (code %v), want NOT_FOUND", err, runner.CodeOf(err))
	}
}

func TestIntegrationUnionOfLocalDirs(t *testing.T) {
	it := newITest(t)
	a, b, dst := it.dir(t, "a dir"), it.dir(t, quirky("b \"dir\"")), it.dir(t, "dst")
	writeFiles(t, a, map[string]int{"from-a.txt": 10, "shared/x.txt": 5})
	writeFiles(t, b, map[string]int{"from-b.txt": 20, "shared/y.txt": 6})

	union := &syncrv1.RemoteDefinition{
		Alias: "both", BackendType: "union",
		UnionUpstreams: []*syncrv1.UnionUpstream{
			{Location: local(a)},
			{Location: local(b), Mode: syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_READ_ONLY},
		},
	}
	_, _, err := it.run(t.Context(), t, modeCopy, &syncrv1.Location{Remote: "both"}, local(dst), nil, union)
	if err != nil {
		t.Fatal(err)
	}
	got := tree(t, dst)
	for _, name := range []string{"from-a.txt", "from-b.txt", "shared/x.txt", "shared/y.txt"} {
		if got[name] == nil {
			t.Errorf("union copy lacks %s (got %v)", name, slicesOfKeys(got))
		}
	}

	// Writing into the union goes to the writable upstream only.
	src := it.dir(t, "new")
	writeFiles(t, src, map[string]int{"written.txt": 7})
	if _, _, err := it.run(t.Context(), t, modeCopy, local(src), &syncrv1.Location{Remote: "both", Path: "in"}, nil, union); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(a, "in", "written.txt")); err != nil {
		t.Errorf("file not written to the read-write upstream: %v", err)
	}
	if _, err := os.Stat(filepath.Join(b, "in")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("read-only upstream was written: %v", err)
	}
}

// roundTrip copies a local tree to remote:path and back, and compares.
func roundTrip(t *testing.T, it *itest, def *syncrv1.RemoteDefinition, path string) {
	t.Helper()
	src, back := it.dir(t, "src"), it.dir(t, "back")
	total := writeFiles(t, src, map[string]int{"a.txt": 100, "dir/b.bin": 1 << 20, "dir/sp ace,'q\":x.txt": 3})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	remote := &syncrv1.Location{Remote: def.GetAlias(), Path: path}
	res, _, err := it.run(ctx, t, modeCopy, local(src), remote, nil, def)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if res.Stats.Bytes != total {
		t.Errorf("uploaded %d bytes, want %d", res.Stats.Bytes, total)
	}
	if _, _, err := it.run(ctx, t, modeCopy, remote, local(back), nil, def); err != nil {
		t.Fatalf("download: %v", err)
	}
	sameTree(t, tree(t, back), tree(t, src))

	// A wrong secret fails without leaking it.
	bad := cloneDef(def)
	for k := range bad.Secrets {
		bad.Secrets[k] = "wrong-" + k + "-Secret-Value"
	}
	_, _, err = it.run(ctx, t, modeCopy, local(src), remote, &syncrv1.RcloneTransferOptions{Retries: ptr[int32](1), LowLevelRetries: ptr[int32](1)}, bad)
	if err == nil {
		t.Fatal("transfer with wrong credentials succeeded")
	}
	t.Logf("wrong credentials: %v (code %v)", err, runner.CodeOf(err))
	for _, v := range bad.Secrets {
		if strings.Contains(err.Error(), v) {
			t.Errorf("error leaks a secret: %v", err)
		}
	}
}

func cloneDef(d *syncrv1.RemoteDefinition) *syncrv1.RemoteDefinition {
	return &syncrv1.RemoteDefinition{
		Alias: d.Alias, BackendType: d.BackendType, Options: maps.Clone(d.Options),
		Secrets: maps.Clone(d.Secrets), ObscureKeys: d.ObscureKeys, UnionUpstreams: d.UnionUpstreams,
	}
}

func TestIntegrationMinIO(t *testing.T) {
	it := newITest(t)
	requireContainers(t)
	// The secret contains characters that would break a connection string
	// or an INI file; MinIO only accepts the request signatures if it
	// arrives intact.
	const user, secret = "syncr-it", `s3'cr"et, with: spaces`
	ctr, err := testcontainers.Run(t.Context(), minioImage,
		testcontainers.WithCmd("server", "/data"),
		testcontainers.WithEnv(map[string]string{"MINIO_ROOT_USER": user, "MINIO_ROOT_PASSWORD": secret}),
		// tmpfs: fast, and independent of the host's free disk space (MinIO
		// refuses writes when its drive is nearly full).
		testcontainers.WithTmpfs(map[string]string{"/data": "rw,size=256m"}),
		testcontainers.WithExposedPorts("9000/tcp"),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/minio/health/ready").WithPort("9000/tcp").WithStartupTimeout(time.Minute)),
	)
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := ctr.PortEndpoint(t.Context(), "9000/tcp", "http")
	if err != nil {
		t.Fatal(err)
	}
	def := &syncrv1.RemoteDefinition{
		Alias:       "minio",
		BackendType: "s3",
		Options:     map[string]string{"provider": "Minio", "endpoint": endpoint, "region": "us-east-1", "force_path_style": "true"},
		Secrets:     map[string]string{"access_key_id": user, "secret_access_key": secret},
	}
	roundTrip(t, it, def, "syncr-it/prefix")
	// S3 cannot report usage, and only a bucket can be missing: a prefix
	// without objects is an empty folder.
	probeRemote(t, it, def, "syncr-it/prefix", "syncr-it-no-such-bucket/x", false)
	empty, err := it.prober(t).List(t.Context(), "0195f0a4-3b7c-7d1e-9f00-0000000000de", []*syncrv1.RemoteDefinition{def},
		&syncrv1.ProbeListRemote{Location: &syncrv1.Location{Remote: "minio", Path: "syncr-it/no-such-prefix"}})
	if err != nil || len(empty.GetEntries()) != 0 {
		t.Errorf("a prefix without objects = %v, %v; want an empty listing", empty, err)
	}
}

func TestIntegrationSFTP(t *testing.T) {
	it := newITest(t)
	requireContainers(t)
	// atmoz/sftp splits its argument on ":", so the password avoids it.
	const user, pass = "syncr", `p'a"s,s w$rd`
	ctr, err := testcontainers.Run(t.Context(), sftpImage,
		testcontainers.WithCmd(fmt.Sprintf("%s:%s:1001:1001:upload", user, pass)),
		testcontainers.WithExposedPorts("22/tcp"),
		testcontainers.WithWaitStrategy(wait.ForListeningPort("22/tcp").WithStartupTimeout(time.Minute)),
	)
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatal(err)
	}
	host, err := ctr.Host(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	port, err := ctr.MappedPort(t.Context(), "22/tcp")
	if err != nil {
		t.Fatal(err)
	}
	def := &syncrv1.RemoteDefinition{
		Alias:       "box",
		BackendType: "sftp",
		// shell_type none: the chrooted server has no shell for hash
		// commands, and rclone would otherwise probe for one.
		Options:     map[string]string{"host": host, "port": port.Port(), "user": user, "shell_type": "none"},
		Secrets:     map[string]string{"pass": pass},
		ObscureKeys: []string{"pass"},
	}
	roundTrip(t, it, def, "upload/syncr-it")
	probeRemote(t, it, def, "upload/syncr-it", "upload/syncr-it/nope", true)
	sftpHostKeys(t, it, ctr, def)
}

// sftpHostKeys checks the sftp_host_keys probe against the server: the keys
// it fetches are the server's (every type it has), a listing that pins them
// in host_keys works, and one that pins another key is refused by rclone.
func sftpHostKeys(t *testing.T, it *itest, ctr testcontainers.Container, def *syncrv1.RemoteDefinition) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	port, err := strconv.Atoi(def.GetOptions()["port"])
	if err != nil {
		t.Fatal(err)
	}
	fetched, err := (&probe.SSHScanner{}).HostKeys(ctx, def.GetOptions()["host"], port)
	if err != nil {
		t.Fatalf("fetch host keys: %v", err)
	}
	var want, got []string
	for _, name := range []string{"ssh_host_ed25519_key.pub", "ssh_host_rsa_key.pub"} {
		r, err := ctr.CopyFileFromContainer(ctx, "/etc/ssh/"+name)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil {
			t.Fatal(err)
		}
		k, _, _, _, err := ssh.ParseAuthorizedKey(raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want = append(want, k.Type()+" "+base64.StdEncoding.EncodeToString(k.Marshal())+" "+ssh.FingerprintSHA256(k))
	}
	var pins []string
	for _, k := range fetched {
		got = append(got, k.GetAlgorithm()+" "+k.GetKey()+" "+k.GetFingerprintSha256())
		pins = append(pins, k.GetAlgorithm()+" "+k.GetKey())
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("fetched host keys\n%q\nwant the server's\n%q", got, want)
	}

	p := it.prober(t)
	list := func(hostKeys string) error {
		d := cloneDef(def)
		d.Options["host_keys"] = hostKeys
		_, err := p.List(ctx, "0195f0a4-3b7c-7d1e-9f00-0000000000df", []*syncrv1.RemoteDefinition{d},
			&syncrv1.ProbeListRemote{Location: &syncrv1.Location{Remote: d.GetAlias(), Path: "upload/syncr-it"}})
		return err
	}
	if err := list(strings.Join(pins, ",")); err != nil {
		t.Errorf("listing with the fetched host keys pinned: %v", err)
	}
	_, other, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := ssh.NewPublicKey(other.Public())
	if err != nil {
		t.Fatal(err)
	}
	wrong := otherKey.Type() + " " + base64.StdEncoding.EncodeToString(otherKey.Marshal())
	if err := list(wrong); err == nil || !strings.Contains(err.Error(), "host key mismatch") {
		t.Errorf("listing with another host key pinned = %v; want rclone to refuse the server's key", err)
	}
}
