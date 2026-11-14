package devseed

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	pathpkg "path"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/app/restic"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// Probes answers worker probes on a seeded controller with canned results:
// the demo workers do not exist, so nothing could answer them. It stands
// in for the controller's probe broker (the transport of its probe
// runner): folder listings of the demo workers and remotes, remote usage,
// restic's init, config, snapshot and tree listings for the demo
// repositories and for repositories created while the demo runs, and
// (Milestone 7, scans.go) folder sizes, searches and comparisons computed
// from the same trees and snapshots.
//
// The answers are fixed apart from times, which count back from the seed's
// start. No probe leaves the process and nothing is stored but the
// repositories created through it (in memory). It is safe for concurrent
// use.
type Probes struct {
	clock Clock

	mu sync.Mutex
	// start is the seed's start; workers the demo workers' local roots.
	start   time.Time
	workers map[domain.WorkerID][]string
	// remotes are the demo remotes' IDs by name: the tags of the
	// snapshots of backups of a remote (demoRepo.sourceTagged).
	remotes map[string]domain.RemoteID
	// created are the repositories initialized through restic_init, by
	// location, with their passwords.
	created map[string]string
}

// NewProbes returns the canned probe answers of a seeded controller. Run
// binds them to the seeded workers (Deps.Probes).
func NewProbes(clock Clock) *Probes {
	return &Probes{clock: clock, workers: map[domain.WorkerID][]string{}, created: map[string]string{}}
}

// bind tells the probes the seed's start, its workers' local roots and its
// remotes' IDs.
func (p *Probes) bind(start time.Time, roots map[domain.WorkerID][]string, remotes map[string]domain.RemoteID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.start, p.workers, p.remotes = start, roots, remotes
}

// seedStart is the time the answers count back from.
func (p *Probes) seedStart() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.start.IsZero() {
		return p.clock.Now()
	}
	return p.start
}

// probeError is a failed probe: a link error code and its message.
type probeError struct {
	code syncrv1.JobErrorCode
	msg  string
}

func (f *probeError) Error() string { return f.msg }

func failf(code syncrv1.JobErrorCode, format string, args ...any) error {
	return &probeError{code: code, msg: fmt.Sprintf(format, args...)}
}

// Probe answers one probe as the worker workerID would. It never fails: a
// refused probe is a result with status FAILED, as a worker reports it.
func (p *Probes) Probe(ctx context.Context, workerID domain.WorkerID, run *syncrv1.RunProbe) (*syncrv1.ProbeResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	res := &syncrv1.ProbeResult{ProbeId: run.GetProbeId(), Status: syncrv1.ProbeStatus_PROBE_STATUS_OK, Elapsed: durationpb.New(180 * time.Millisecond)}
	spec := run.GetSpec()
	var err error
	switch op := spec.GetOp().(type) {
	case *syncrv1.ProbeSpec_ListLocal:
		var l *syncrv1.ProbeListing
		if l, err = p.listLocal(workerID, op.ListLocal); err == nil {
			res.Output = &syncrv1.ProbeResult_Listing{Listing: l}
		}
	case *syncrv1.ProbeSpec_ListRemote:
		var l *syncrv1.ProbeListing
		if l, err = p.listRemote(spec.GetRemotes(), op.ListRemote); err == nil {
			res.Output = &syncrv1.ProbeResult_Listing{Listing: l}
		}
	case *syncrv1.ProbeSpec_About:
		var u *syncrv1.ProbeUsage
		if u, err = about(spec.GetRemotes()); err == nil {
			res.Output = &syncrv1.ProbeResult_Usage{Usage: u}
		}
	case *syncrv1.ProbeSpec_ResticInit:
		var c *syncrv1.ResticRepositoryConfig
		if c, err = p.resticInit(spec.GetRemotes(), op.ResticInit.GetRepository()); err == nil {
			res.Output = &syncrv1.ProbeResult_RepositoryConfig{RepositoryConfig: c}
		}
	case *syncrv1.ProbeSpec_ResticConfig:
		var r probeRepo
		if r, err = p.open(spec.GetRemotes(), op.ResticConfig.GetRepository()); err == nil {
			res.Output = &syncrv1.ProbeResult_RepositoryConfig{RepositoryConfig: &syncrv1.ResticRepositoryConfig{Id: r.resticID, Version: 2}}
		}
	case *syncrv1.ProbeSpec_ResticSnapshots:
		var r probeRepo
		if r, err = p.open(spec.GetRemotes(), op.ResticSnapshots.GetRepository()); err == nil {
			res.Output = &syncrv1.ProbeResult_Snapshots{Snapshots: snapshotsOutput(r, p.seedStart(), int(op.ResticSnapshots.GetMaxSnapshots()))}
		}
	case *syncrv1.ProbeSpec_ResticLs:
		var l *syncrv1.ProbeListing
		if l, err = p.resticLs(spec.GetRemotes(), op.ResticLs); err == nil {
			res.Output = &syncrv1.ProbeResult_Listing{Listing: l}
		}
	case *syncrv1.ProbeSpec_SftpHostKeys:
		var k *syncrv1.SftpHostKeys
		if k, err = hostKeys(op.SftpHostKeys); err == nil {
			res.Output = &syncrv1.ProbeResult_SftpHostKeys{SftpHostKeys: k}
		}
	case *syncrv1.ProbeSpec_MeasureLocal:
		var m *syncrv1.ProbeMeasurement
		if m, err = p.measureLocal(workerID, op.MeasureLocal); err == nil {
			res.Output, res.Elapsed = &syncrv1.ProbeResult_Measurement{Measurement: m}, scanElapsed(run, m.GetComplete())
		}
	case *syncrv1.ProbeSpec_MeasureRemote:
		var m *syncrv1.ProbeMeasurement
		if m, err = p.measureRemote(spec.GetRemotes(), op.MeasureRemote); err == nil {
			res.Output, res.Elapsed = &syncrv1.ProbeResult_Measurement{Measurement: m}, scanElapsed(run, m.GetComplete())
		}
	case *syncrv1.ProbeSpec_ResticMeasure:
		var m *syncrv1.ProbeMeasurement
		if m, err = p.resticMeasure(spec.GetRemotes(), op.ResticMeasure); err == nil {
			res.Output, res.Elapsed = &syncrv1.ProbeResult_Measurement{Measurement: m}, scanElapsed(run, true)
		}
	case *syncrv1.ProbeSpec_ResticFind:
		var f *syncrv1.ResticFindOutput
		if f, err = p.resticFind(spec.GetRemotes(), op.ResticFind); err == nil {
			res.Output, res.Elapsed = &syncrv1.ProbeResult_Find{Find: f}, scanElapsed(run, true)
		}
	case *syncrv1.ProbeSpec_ResticDiff:
		var d *syncrv1.ResticDiffOutput
		if d, err = p.resticDiff(spec.GetRemotes(), op.ResticDiff); err == nil {
			res.Output, res.Elapsed = &syncrv1.ProbeResult_Diff{Diff: d}, scanElapsed(run, true)
		}
	default:
		err = failf(syncrv1.JobErrorCode_JOB_ERROR_CODE_UNSUPPORTED_KIND, "the demo does not answer this probe")
	}
	if err != nil {
		code := syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL
		if f, ok := err.(*probeError); ok { //nolint:errorlint // made here, never wrapped
			code = f.code
		}
		res.Status, res.Error = syncrv1.ProbeStatus_PROBE_STATUS_FAILED, &syncrv1.JobError{Code: code, Message: err.Error()}
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// Demo file trees
// ---------------------------------------------------------------------------

// demoFile is a file of a demo tree: its slash-separated path and size.
type demoFile struct {
	path string
	size int64
}

// treeFile is a file of a demo tree with what a snapshot may change: mod,
// if set, is its modification time (otherwise a fixed age before the
// seed's start); link makes it a symbolic link (size 0).
type treeFile struct {
	demoFile
	mod  time.Time
	link bool
}

// tree is a file tree: the direct children of every folder, by folder
// path. Paths are absolute and slash-separated ("/" is the top).
type tree map[string][]*syncrv1.ProbeEntry

// newTree builds the tree of files, with every folder above them, plus the
// empty folders dirs. Modification times count back from start.
func newTree(start time.Time, files []demoFile, dirs ...string) tree {
	fs := make([]treeFile, len(files))
	for i, f := range files {
		fs[i] = treeFile{demoFile: f}
	}
	return buildTree(start, fs, dirs...)
}

// buildTree is newTree for files with their own times and types.
func buildTree(start time.Time, files []treeFile, dirs ...string) tree {
	t := tree{"/": nil}
	var addDir func(dir string)
	addDir = func(dir string) {
		if _, ok := t[dir]; ok {
			return
		}
		t[dir] = nil
		parent := pathpkg.Dir(dir)
		addDir(parent)
		t[parent] = append(t[parent], &syncrv1.ProbeEntry{
			Name: pathpkg.Base(dir), Path: dir, Type: syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_DIR, Mode: "drwxr-xr-x",
			ModTime: timestamppb.New(start.Add(-age(dir))),
		})
	}
	for _, d := range dirs {
		addDir(pathpkg.Clean("/" + d))
	}
	for _, f := range files {
		full := pathpkg.Clean("/" + f.path)
		dir := pathpkg.Dir(full)
		addDir(dir)
		e := &syncrv1.ProbeEntry{
			Name: pathpkg.Base(full), Path: full, Type: syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_FILE, Size: f.size, Mode: "-rw-r--r--",
			ModTime: timestamppb.New(start.Add(-age(full))),
		}
		if !f.mod.IsZero() {
			e.ModTime = timestamppb.New(f.mod)
		}
		if f.link {
			e.Type, e.Size, e.Mode = syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_SYMLINK, 0, "Lrwxrwxrwx"
		}
		t[dir] = append(t[dir], e)
	}
	for dir := range t {
		slices.SortFunc(t[dir], func(a, b *syncrv1.ProbeEntry) int {
			if ad, bd := a.GetType() == syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_DIR, b.GetType() == syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_DIR; ad != bd {
				if ad {
					return -1
				}
				return 1
			}
			return strings.Compare(a.GetName(), b.GetName())
		})
	}
	return t
}

// age is a fixed age for a path: between an hour and about 300 days.
func age(path string) time.Duration {
	sum := sha256.Sum256([]byte(path))
	return time.Hour + time.Duration(int(sum[0])<<8|int(sum[1]))%(300*24)*time.Hour
}

// list lists dir: at most limit entries (0: all), only folders with
// dirsOnly. ok is false if dir is not a folder of the tree.
func (t tree) list(dir string, limit int, dirsOnly bool) (l *syncrv1.ProbeListing, ok bool) {
	dir = pathpkg.Clean("/" + dir)
	entries, ok := t[dir]
	if !ok {
		return nil, false
	}
	l = &syncrv1.ProbeListing{Path: dir}
	for _, e := range entries {
		if dirsOnly && e.GetType() != syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_DIR {
			continue
		}
		if limit > 0 && len(l.Entries) == limit {
			l.Truncated = true
			break
		}
		l.Entries = append(l.Entries, e)
	}
	return l, true
}

// photoFiles are a photo library below root: a folder a month.
func photoFiles(root string) []demoFile {
	var out []demoFile
	for m := 1; m <= 11; m++ {
		for i := range 9 {
			n := m*100 + i*7
			out = append(out, demoFile{fmt.Sprintf("%s/2026/%02d/IMG_%04d.jpg", root, m, n), 3*mib + int64(n%17)*180*kib})
		}
	}
	for i := range 6 {
		out = append(out, demoFile{fmt.Sprintf("%s/2025/12/IMG_%04d.jpg", root, 9000+i*3), 4*mib + int64(i)*210*kib})
	}
	return append(out,
		demoFile{root + "/2026/albums.json", 18 * kib},
		demoFile{root + "/README.txt", 420},
	)
}

// documentFiles are a documents folder below root.
func documentFiles(root string) []demoFile {
	return []demoFile{
		{root + "/contracts/lease-2024.pdf", 812 * kib},
		{root + "/contracts/insurance.pdf", 1*mib + 204*kib},
		{root + "/contracts/signed/lease-2024-signed.pdf", 2*mib + 90*kib},
		{root + "/taxes/2024/return.pdf", 640 * kib},
		{root + "/taxes/2024/receipts.zip", 48 * mib},
		{root + "/taxes/2025/return-draft.ods", 92 * kib},
		{root + "/notes/a b.txt", 2 * kib},
		{root + "/notes/[x] todo.md", 4 * kib},
		{root + "/notes/über.txt", 1 * kib},
		{root + "/budget.xlsx", 310 * kib},
		{root + "/inventory.csv", 58 * kib},
	}
}

// workerTree is the filesystem the demo workers share (each sees its own
// local roots of it).
func workerTree(start time.Time) tree {
	files := slices.Concat(photoFiles("/srv/photos"), documentFiles("/srv/docs"), []demoFile{
		{"/srv/media/films/holiday-2025.mkv", 4*gib + 300*mib},
		{"/srv/media/films/wedding.mp4", 2*gib + 120*mib},
		{"/srv/media/music/playlist.m3u", 6 * kib},
		{"/srv/home/alice/thesis.pdf", 12 * mib},
		{"/srv/home/alice/notes.md", 9 * kib},
		{"/srv/incoming/scan-0001.pdf", 1 * mib},
		{"/mnt/disk1/nextcloud/data.tar", 9 * gib},
		{"/mnt/disk1/old-nas/shares.tar", 3 * gib},
		{"/mnt/disk2/nextcloud/data.tar.1", 5 * gib},
		{"/backup/nextcloud-photos/2026-10.tar", 6 * gib},
		{"/backup/restore-test/photos/2024/IMG_0001.jpg", 4 * mib},
	}, sharedFiles(sharedLocal))
	return newTree(start, files, "/srv/restore", "/mnt/disk2/media", "/backup/restores")
}

// listLocal lists a folder of a demo worker, inside its local roots.
func (p *Probes) listLocal(workerID domain.WorkerID, op *syncrv1.ProbeListLocal) (*syncrv1.ProbeListing, error) {
	p.mu.Lock()
	roots := slices.Clone(p.workers[workerID])
	p.mu.Unlock()
	start := p.seedStart()
	limit := int(op.GetMaxEntries())
	if op.GetPath() == "" {
		l := &syncrv1.ProbeListing{}
		for _, r := range roots {
			if limit > 0 && len(l.Entries) == limit {
				l.Truncated = true
				break
			}
			l.Entries = append(l.Entries, &syncrv1.ProbeEntry{
				Name: r, Path: r, Type: syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_DIR, Mode: "drwxr-xr-x", ModTime: timestamppb.New(start.Add(-age(r))),
			})
		}
		return l, nil
	}
	dir := pathpkg.Clean(op.GetPath())
	if !slices.ContainsFunc(roots, func(r string) bool { return dir == r || strings.HasPrefix(dir, r+"/") }) {
		return nil, failf(syncrv1.JobErrorCode_JOB_ERROR_CODE_PATH_NOT_ALLOWED, "%s: path is outside the worker's local_roots", op.GetPath())
	}
	l, ok := workerTree(start).list(dir, limit, op.GetDirsOnly())
	if !ok {
		return nil, failf(syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "%s: no such folder", op.GetPath())
	}
	return l, nil
}

// ---------------------------------------------------------------------------
// Demo remotes
// ---------------------------------------------------------------------------

// resticLayout are the files of a restic repository at root.
func resticLayout(root string) []demoFile {
	return []demoFile{
		{root + "/config", 155},
		{root + "/keys/3f1c9a2b7e5d4c60", 460},
		{root + "/index/8a2e4c6f0b1d3597", 2 * mib},
		{root + "/snapshots/0c5e7a9b1d3f2460", 310},
		{root + "/data/00/00a1b2c3d4e5f607", 16 * mib},
		{root + "/data/a7/a7c9e1f3b5d70829", 17 * mib},
	}
}

// demoRemoteRoots are the roots rclone joins to a path on the demo remotes
// (the bucket, the SFTP folder), longest first.
func demoRemoteRoots() []string {
	return []string{"home/backups", "syncr-offsite", "alice-backup", sharedBucket, "archive"}
}

// remoteRel returns a location's path relative to its demo remote's root.
func remoteRel(loc *syncrv1.Location) string {
	p := strings.Trim(pathpkg.Clean("/"+loc.GetPath()), "/")
	for _, root := range demoRemoteRoots() {
		if p == root {
			return ""
		}
		if rest, ok := strings.CutPrefix(p, root+"/"); ok {
			return rest
		}
	}
	return p
}

// backendOf is the rclone backend of the remote a probe addresses ("" if
// it names none), or "r2" for an S3 remote on Cloudflare R2 (the shared
// folder's bucket), which has content of its own.
func backendOf(remotes []*syncrv1.RemoteDefinition) string {
	if len(remotes) == 0 {
		return ""
	}
	if r := remotes[0]; r.GetBackendType() == "s3" && r.GetOptions()["provider"] == "Cloudflare" {
		return "r2"
	}
	return remotes[0].GetBackendType()
}

// remoteTree is the content of the demo remote on backend; ok is false for
// a backend whose demo remote cannot be reached.
func remoteTree(backend string, start time.Time) (tree, error) {
	switch backend {
	case "s3":
		return newTree(start, slices.Concat(resticLayout("restic/nas-docs"), photoFiles("photos"), []demoFile{
			{"archive/2025/nextcloud-2025-12.tar", 14 * gib},
			{"archive/2025/nextcloud-2025-11.tar", 13 * gib},
			{"nextcloud/documents/budget.xlsx", 310 * kib},
			{"nextcloud/shared/readme.md", 2 * kib},
			{"office-pc/documents/report.docx", 240 * kib},
		})), nil
	case "b2":
		return newTree(start, slices.Concat(resticLayout("restic/photos"), resticLayout("restic"), []demoFile{
			{"photos/2024/IMG_0001.jpg", 4 * mib},
			{"photos/2026/11/IMG_1100.jpg", 3 * mib},
			{"pool/nextcloud/data.tar", 9 * gib},
		})), nil
	case "sftp":
		return newTree(start, slices.Concat(resticLayout("backups/office"), resticLayout("backups/old-laptop"), []demoFile{
			{"nextcloud/2026-11-01.tar", 14 * gib},
			{"archive/2026-10-25.tar", 15 * gib},
			{"media/films/holiday-2025.mkv", 4*gib + 300*mib},
			{"calendar/ada.ics", 96 * kib},
			{"laptop/projects/syncr.tar.zst", 220 * mib},
			{"Scans/2026-10-invoice.pdf", 640 * kib},
			{"Scans/2026-11-contract.pdf", 2*mib + 300*kib},
		})), nil
	case "r2":
		return newTree(start, sharedFiles(sharedRemotePath)), nil
	case "webdav":
		return nextcloudTree(start), nil
	}
	return nil, failf(syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "the demo has no content for this kind of remote")
}

// nextcloudTree is the demo Nextcloud: the phone's photos that the
// nightly remote backup reads (with thumbnails and an upload in progress,
// which its filters leave out), the documents and shared folders the copy
// jobs read, and an empty Inbox (the allowed empty run).
func nextcloudTree(start time.Time) tree {
	return newTree(start, slices.Concat(photoFiles("Photos"), documentFiles("Documents"), []demoFile{
		{"Photos/.thumbnails/IMG_1100.jpg", 24 * kib},
		{"Photos/2026/11/IMG_1163.jpg.tmp", 1*mib + 512*kib},
		{"Shared/readme.md", 2 * kib},
		{"Calendar/ada.ics", 96 * kib},
	}), "Inbox", "Projects")
}

func (p *Probes) listRemote(remotes []*syncrv1.RemoteDefinition, op *syncrv1.ProbeListRemote) (*syncrv1.ProbeListing, error) {
	t, err := remoteTree(backendOf(remotes), p.seedStart())
	if err != nil {
		return nil, err
	}
	rel := remoteRel(op.GetLocation())
	l, ok := t.list(rel, int(op.GetMaxEntries()), op.GetDirsOnly())
	if !ok {
		return nil, failf(syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "directory not found: %s", rel)
	}
	return l, nil
}

// about is the usage of the demo remote on the probe's backend.
func about(remotes []*syncrv1.RemoteDefinition) (*syncrv1.ProbeUsage, error) {
	switch backendOf(remotes) {
	case "b2":
		return &syncrv1.ProbeUsage{Used: ptr(412*gib + 300*mib), Objects: ptr[int64](58214)}, nil
	case "sftp":
		return &syncrv1.ProbeUsage{Total: ptr(1024 * gib), Used: ptr(318 * gib), Free: ptr(706 * gib)}, nil
	case "webdav":
		// Nextcloud's quota.
		return &syncrv1.ProbeUsage{Total: ptr(512 * gib), Used: ptr(74*gib + 600*mib), Free: ptr(437*gib + 424*mib)}, nil
	}
	return &syncrv1.ProbeUsage{Unsupported: true}, nil
}

// hostKeys answers an sftp_host_keys probe as a worker would: every demo
// SSH server offers the demo host keys (demoHostKeys), with their SHA-256
// fingerprints. The controller checks them as it checks a worker's answer.
func hostKeys(op *syncrv1.ProbeSftpHostKeys) (*syncrv1.SftpHostKeys, error) {
	if op.GetHost() == "" || op.GetPort() < 0 || op.GetPort() > 65535 {
		return nil, failf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INVALID_SPEC, "invalid host or port")
	}
	out := &syncrv1.SftpHostKeys{}
	for _, line := range demoHostKeys() {
		algorithm, key, _ := strings.Cut(line, " ")
		blob, err := base64.StdEncoding.DecodeString(key)
		if err != nil {
			return nil, failf(syncrv1.JobErrorCode_JOB_ERROR_CODE_INTERNAL, "demo host key %s: %v", algorithm, err)
		}
		sum := sha256.Sum256(blob)
		out.Keys = append(out.Keys, &syncrv1.SftpHostKey{
			Algorithm: algorithm, Key: key, FingerprintSha256: "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:]),
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Demo repositories
// ---------------------------------------------------------------------------

// probeRepo is a repository as the probes know it.
type probeRepo struct {
	resticID string
	demo     *demoRepo // nil for a repository created through restic_init
}

// repoKey identifies a repository's location: its backend and path.
func repoKey(remotes []*syncrv1.RemoteDefinition, repo *syncrv1.ResticRepository) string {
	if backendOf(remotes) == "" {
		return "local:" + pathpkg.Clean(repo.GetLocation().GetPath())
	}
	return backendOf(remotes) + ":" + remoteRel(repo.GetLocation())
}

// resticIDOf is the fixed restic config ID of the repository at key.
func resticIDOf(key string) string {
	sum := sha256.Sum256([]byte("syncr devseed repository " + key))
	return hex.EncodeToString(sum[:])
}

// open finds the repository a probe addresses and checks its password.
func (p *Probes) open(remotes []*syncrv1.RemoteDefinition, repo *syncrv1.ResticRepository) (probeRepo, error) {
	key := repoKey(remotes, repo)
	for _, d := range demoRepos() {
		if d.key() != key || d.gone {
			continue
		}
		if repo.GetPassword() != demoRepoPassword {
			return probeRepo{}, failf(syncrv1.JobErrorCode_JOB_ERROR_CODE_WRONG_PASSWORD, "wrong password or no key found")
		}
		p.mu.Lock()
		d = d.sourceTagged(p.remotes)
		p.mu.Unlock()
		return probeRepo{resticID: resticIDOf(key), demo: &d}, nil
	}
	p.mu.Lock()
	password, ok := p.created[key]
	p.mu.Unlock()
	switch {
	case !ok:
		return probeRepo{}, failf(syncrv1.JobErrorCode_JOB_ERROR_CODE_REPOSITORY_NOT_FOUND, "no restic repository at this location")
	case password != repo.GetPassword():
		return probeRepo{}, failf(syncrv1.JobErrorCode_JOB_ERROR_CODE_WRONG_PASSWORD, "wrong password or no key found")
	}
	return probeRepo{resticID: resticIDOf(key)}, nil
}

// resticInit creates a repository where none is yet.
func (p *Probes) resticInit(remotes []*syncrv1.RemoteDefinition, repo *syncrv1.ResticRepository) (*syncrv1.ResticRepositoryConfig, error) {
	if backendOf(remotes) == "webdav" {
		return nil, failf(syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "connect: connection refused")
	}
	key := repoKey(remotes, repo)
	exists := slices.ContainsFunc(demoRepos(), func(d demoRepo) bool { return d.key() == key && !d.gone })
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.created[key]; ok || exists {
		return nil, failf(syncrv1.JobErrorCode_JOB_ERROR_CODE_ALREADY_EXISTS, "a restic repository already exists at this location")
	}
	p.created[key] = repo.GetPassword()
	return &syncrv1.ResticRepositoryConfig{Id: resticIDOf(key), Version: 2}, nil
}

// snapshotsOutput lists r's snapshots, the newest limit of them.
func snapshotsOutput(r probeRepo, start time.Time, limit int) *syncrv1.ResticSnapshotsOutput {
	out := &syncrv1.ResticSnapshotsOutput{}
	if r.demo == nil {
		return out
	}
	snaps := r.demo.snapshots(start) // newest first
	if limit > 0 && len(snaps) > limit {
		snaps, out.Truncated = snaps[:limit], true
	}
	for _, s := range snaps {
		out.Snapshots = append(out.Snapshots, snapshotToWire(s))
	}
	return out
}

func snapshotToWire(s restic.Snapshot) *syncrv1.ResticSnapshot {
	out := &syncrv1.ResticSnapshot{
		Id: s.ID, Time: timestamppb.New(s.Time), Host: s.Hostname, Username: s.Username, Parent: s.Parent,
		ProgramVersion: s.ProgramVersion, Paths: s.Paths, Tags: s.Tags,
	}
	if sm := s.Summary; sm != nil {
		out.Summary = &syncrv1.ResticSnapshotSummary{
			BackupStart: timestamppb.New(sm.BackupStart), BackupEnd: timestamppb.New(sm.BackupEnd),
			FilesNew: sm.FilesNew, FilesChanged: sm.FilesChanged, FilesUnmodified: sm.FilesUnmodified,
			DirsNew: sm.DirsNew, DirsChanged: sm.DirsChanged, DirsUnmodified: sm.DirsUnmodified,
			DataBlobs: sm.DataBlobs, TreeBlobs: sm.TreeBlobs, DataAddedBytes: sm.DataAdded, DataAddedPackedBytes: sm.DataAddedPacked,
			TotalFilesProcessed: sm.TotalFilesProcessed, TotalBytesProcessed: sm.TotalBytesProcessed,
		}
	}
	return out
}

// resticLs lists a folder of a snapshot's tree.
func (p *Probes) resticLs(remotes []*syncrv1.RemoteDefinition, op *syncrv1.ProbeResticLs) (*syncrv1.ProbeListing, error) {
	r, err := p.open(remotes, op.GetRepository())
	if err != nil {
		return nil, err
	}
	start := p.seedStart()
	snap, ok := r.snapshot(start, op.GetSnapshotId())
	if !ok {
		return nil, failf(syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "no snapshot %s in the repository", op.GetSnapshotId())
	}
	l, ok := r.demo.treeAt(start, snap).list(op.GetPath(), int(op.GetMaxEntries()), false)
	if !ok {
		return nil, failf(syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "%s: no such folder in the snapshot", op.GetPath())
	}
	return l, nil
}
