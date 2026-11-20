//go:build integration

package rclone

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// Crypt remotes through the real runners and rclone 1.75.2
// (SYNCR_TEST_RCLONE), over an SFTP server: a memory remote keeps its data
// inside one rclone process, and the worker refuses a crypt remote over a
// local folder or a union, so the data lives on the server between runs.

// TestIntegrationCryptTransfers copies a tree into a crypt remote, syncs
// it into a second crypt remote (a crypt source and a crypt destination,
// with other passwords and folders on the same server) and copies it back:
// the bytes come back unchanged, and the server holds only encrypted
// names. With the wrong password a sync from the crypt remote fails and
// deletes nothing, and a listing fails instead of looking empty.
func TestIntegrationCryptTransfers(t *testing.T) {
	it := newITest(t)
	_, box := startSFTP(t)
	const vaultPass, otherPass, wrongPass = "vault pa:ss w'rd", "other-crypt-password", "not the vault password"
	vault := cryptRemote("vault", "box", "upload/vault", vaultPass)
	other := cryptRemote("other", "box", "upload/other", otherPass)
	defs := []*syncrv1.RemoteDefinition{box, vault, other}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()

	src := it.dir(t, "src")
	total := writeFiles(t, src, map[string]int{"a.txt": 100, "dir/b.bin": 1 << 20, quirky("dir/sp ace,'q\".txt"): 3})
	vaultPhotos := &syncrv1.Location{Remote: "vault", Path: "photos"}
	otherPhotos := &syncrv1.Location{Remote: "other", Path: "photos"}
	res, _, err := it.run(ctx, t, modeCopy, local(src), vaultPhotos, nil, defs...)
	if err != nil {
		t.Fatalf("copy into the crypt remote: %v", err)
	}
	// rclone counts what it writes to the server: the encrypted files,
	// each a little larger (a 32-byte header and 16 bytes per 64 KiB).
	if res.Stats.Bytes < total || res.Stats.Bytes > total+total/100+1024 || res.Stats.Files != 3 {
		t.Errorf("copied %d bytes in %d files, want the encrypted size of %d bytes in 3", res.Stats.Bytes, res.Stats.Files, total)
	}
	if _, _, err := it.run(ctx, t, modeSync, vaultPhotos, otherPhotos, nil, defs...); err != nil {
		t.Fatalf("sync from a crypt remote into another: %v", err)
	}
	back := it.dir(t, "back")
	if _, _, err := it.run(ctx, t, modeCopy, otherPhotos, local(back), nil, defs...); err != nil {
		t.Fatalf("copy back: %v", err)
	}
	sameTree(t, tree(t, back), tree(t, src))

	// The probes see the crypt remote's names; the server has only
	// encrypted ones.
	p := it.prober(t)
	l, err := p.List(ctx, "0195f0a4-3b7c-7d1e-9f00-0000000000e1", defs, &syncrv1.ProbeListRemote{Location: vaultPhotos})
	if err != nil {
		t.Fatalf("list the crypt remote: %v", err)
	}
	var got []string
	for _, e := range l.GetEntries() {
		got = append(got, e.GetName())
	}
	if !slices.Equal(got, []string{"dir", "a.txt"}) {
		t.Errorf("crypt listing = %q", got)
	}
	for _, folder := range []string{"upload/vault", "upload/other"} {
		l, err := p.List(ctx, "0195f0a4-3b7c-7d1e-9f00-0000000000e2", defs, &syncrv1.ProbeListRemote{Location: &syncrv1.Location{Remote: "box", Path: folder}})
		if err != nil {
			t.Fatalf("list %s on the server: %v", folder, err)
		}
		if es := l.GetEntries(); len(es) != 1 || es[0].GetName() == "photos" || es[0].GetType() != syncrv1.ProbeEntryType_PROBE_ENTRY_TYPE_DIR {
			t.Errorf("%s on the server holds %v, want one folder with an encrypted name", folder, es)
		}
	}

	// The wrong password: the worker sets strict_names, so a listing of
	// the crypt remote's root fails on the names it cannot decrypt, and a
	// folder below it is not found (its name encrypts to another one).
	// The runner lists a source's parent first (checkCryptSource). Nothing
	// looks empty, and a sync deletes nothing.
	wrong := []*syncrv1.RemoteDefinition{box, cryptRemote("vault", "box", "upload/vault", wrongPass)}
	retry := &syncrv1.RcloneTransferOptions{Retries: ptr[int32](1), LowLevelRetries: ptr[int32](1)}
	for _, tt := range []struct {
		from          string
		code          syncrv1.JobErrorCode
		msg           string
		listCode      syncrv1.JobErrorCode // of a probe's listing
		listMsg       string
		listingFolder string
	}{
		{"", syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "undecryptable",
			syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "undecryptable", ""},
		{"photos", syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "undecryptable",
			syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "directory not found", "photos"},
		{"photos/dir", syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "directory not found",
			syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "directory not found", "photos/dir"},
	} {
		dst := it.dir(t, "dst-"+strings.ReplaceAll(tt.from, "/", "-"))
		writeFiles(t, dst, map[string]int{"keep.txt": 10, "photos/a.txt": 7, "a.txt": 3})
		before := tree(t, dst)
		_, _, err := it.run(ctx, t, modeSync, &syncrv1.Location{Remote: "vault", Path: tt.from}, local(dst), retry, wrong...)
		t.Logf("sync from vault:%s with the wrong password: %v (code %v)", tt.from, err, runner.CodeOf(err))
		if err == nil || runner.CodeOf(err) != tt.code || !strings.Contains(err.Error(), tt.msg) {
			t.Errorf("sync from vault:%s with the wrong password = %v (code %v), want code %v and %q", tt.from, err, runner.CodeOf(err), tt.code, tt.msg)
		}
		if err != nil && (strings.Contains(err.Error(), wrongPass) || strings.Contains(err.Error(), vaultPass)) {
			t.Errorf("error leaks a password: %v", err)
		}
		sameTree(t, tree(t, dst), before)

		l, err := p.List(ctx, "0195f0a4-3b7c-7d1e-9f00-0000000000e3", wrong, &syncrv1.ProbeListRemote{Location: &syncrv1.Location{Remote: "vault", Path: tt.listingFolder}})
		if err == nil || runner.CodeOf(err) != tt.listCode || !strings.Contains(err.Error(), tt.listMsg) {
			t.Errorf("listing vault:%s with the wrong password = %v, %v (code %v), want code %v and %q", tt.listingFolder, l, err, runner.CodeOf(err), tt.listCode, tt.listMsg)
		}
	}
}

// TestIntegrationCryptBisync keeps a local folder and a folder of a crypt
// remote in step: a first run (a resync) and changes on either side that
// the next runs carry over.
func TestIntegrationCryptBisync(t *testing.T) {
	it := newBisyncITest(t)
	_, box := startSFTP(t)
	vault := cryptRemote("vault", "box", "upload/vault", "two-way pa:ss")
	remotes := []*syncrv1.RemoteDefinition{box, vault}
	pair := &syncrv1.Location{Remote: "vault", Path: "pair"}
	transfers, err := New(it.cfg)
	if err != nil {
		t.Fatal(err)
	}
	transfer := func(src, dst *syncrv1.Location) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		job := transferJob(src, dst, remotes...)
		if _, err := transfers.Run(ctx, job, func(runner.Progress) {}); err != nil {
			t.Fatalf("copy %v to %v: %v", src, dst, err)
		}
	}
	// cryptSide copies the crypt side into a new local folder and returns
	// its files.
	n := 0
	cryptSide := func() []string {
		t.Helper()
		n++
		dir := filepath.Join(it.root, "check"+strconv.Itoa(n))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		transfer(pair, local(dir))
		return names(t, dir)
	}

	// rclone bisync needs both sides to exist, also in a resync: the
	// crypt side starts with a copy of the local one.
	put(t, it.a, map[string]string{"a1.txt": "from a", "sub/s.txt": "shared"})
	transfer(local(it.a), pair)
	spec := it.spec(nil, func(op *syncrv1.RcloneBisync) { op.Path2 = pair })
	spec.Remotes = remotes
	if out, _ := it.sync(t, spec); out.GetResyncReason() != reasonFirstRun {
		t.Errorf("first run: %v", out)
	}

	// A change on the local side reaches the crypt side.
	put(t, it.a, map[string]string{"new.txt": "new"})
	remove(t, it.a, "a1.txt")
	if out, _ := it.sync(t, spec); out.GetResyncReason() != reasonNone {
		t.Errorf("second run: %v", out)
	}
	if got := cryptSide(); !slices.Equal(got, []string{"new.txt", "sub/s.txt"}) {
		t.Errorf("crypt side after the second run = %q", got)
	}

	// A change on the crypt side reaches the local one.
	extra := filepath.Join(it.root, "extra")
	put(t, extra, map[string]string{"from-crypt.txt": "written through the crypt remote"})
	transfer(local(extra), pair)
	if out, _ := it.sync(t, spec); out.GetResyncReason() != reasonNone {
		t.Errorf("third run: %v", out)
	}
	if got := names(t, it.a); !slices.Equal(got, []string{"from-crypt.txt", "new.txt", "sub/s.txt"}) {
		t.Errorf("local side after the third run = %q", got)
	}
	if got := cryptSide(); !slices.Equal(got, []string{"from-crypt.txt", "new.txt", "sub/s.txt"}) {
		t.Errorf("crypt side after the third run = %q", got)
	}
}

// TestIntegrationCryptOverS3WrongPassword: on a bucket (MinIO), a folder
// of a crypt remote read with the wrong password is not an error for
// rclone: its name encrypts to another prefix, which has no objects and
// so lists as an empty folder, and a sync from it would empty its
// destination (checked here with the bare rclone). The runner lists the
// folder's parent first and fails with NOT_FOUND; the destination keeps
// everything. With the right password the same sync works.
func TestIntegrationCryptOverS3WrongPassword(t *testing.T) {
	it := newITest(t)
	minio := startMinIO(t)
	const pass = "s3 crypt pa:ss"
	vault := func(password string) *syncrv1.RemoteDefinition {
		return cryptRemote("vault", "minio", "syncr-crypt/vault", password)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	src := it.dir(t, "src")
	writeFiles(t, src, map[string]int{"a.txt": 10, "dir/b.bin": 2000})
	photos := &syncrv1.Location{Remote: "vault", Path: "photos"}
	if _, _, err := it.run(ctx, t, modeCopy, local(src), photos, nil, minio, vault(pass)); err != nil {
		t.Fatalf("copy into the crypt remote: %v", err)
	}

	// The bare rclone, with the environment the worker builds: the folder
	// lists as empty, without an error, and a sync from it deletes.
	wrong := []*syncrv1.RemoteDefinition{minio, vault("not the password")}
	env, err := it.cfg.Env.Build(t.TempDir(), wrong)
	if err != nil {
		t.Fatal(err)
	}
	bare := it.dir(t, "bare")
	writeFiles(t, bare, map[string]int{"keep.txt": 10})
	if _, err := proc.New(proc.Options{}).Run(ctx, proc.Spec{
		Path: it.cfg.Binary, Args: []string{"sync", "--", "vault:photos", bare}, Env: env.Vars(),
		Stdout: func(proc.Line) {}, Stderr: func(proc.Line) {},
	}); err != nil {
		t.Logf("bare rclone sync with the wrong password: %v", err)
	}
	if got := tree(t, bare); len(got) != 0 {
		t.Logf("the bare rclone kept %v: rclone no longer lists a missing prefix as empty; the runner's check may be unneeded", slicesOfKeys(got))
	} else {
		t.Log("the bare rclone emptied the destination: the runner's check is needed")
	}

	// The runner refuses before it syncs: the parent of a folder below
	// the root lists as empty (NOT_FOUND), and the root's listing fails on
	// the names it cannot decrypt.
	dst := it.dir(t, "dst")
	writeFiles(t, dst, map[string]int{"keep.txt": 10, "a.txt": 3})
	before := tree(t, dst)
	for _, tt := range []struct {
		from string
		code syncrv1.JobErrorCode
		msg  string
	}{
		{"photos/dir", syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "password is wrong"},
		{"photos", syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "undecryptable"},
		{"", syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "undecryptable"},
	} {
		_, _, err = it.run(ctx, t, modeSync, &syncrv1.Location{Remote: "vault", Path: tt.from}, local(dst), nil, wrong...)
		if runner.CodeOf(err) != tt.code || !strings.Contains(err.Error(), tt.msg) {
			t.Errorf("sync from vault:%s with the wrong password = %v (code %v), want code %v and %q", tt.from, err, runner.CodeOf(err), tt.code, tt.msg)
		}
		sameTree(t, tree(t, dst), before)
	}

	// With the right password the folder is found and synced.
	if _, _, err := it.run(ctx, t, modeSync, photos, local(dst), nil, minio, vault(pass)); err != nil {
		t.Fatalf("sync with the right password: %v", err)
	}
	sameTree(t, tree(t, dst), tree(t, src))
}
