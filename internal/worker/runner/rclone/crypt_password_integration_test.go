//go:build integration

package rclone

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/proc"
	"github.com/Zeptiny/Syncr/internal/worker/runner"
)

// bareRclone runs the pinned rclone with args and the environment the
// worker builds for defs, and returns its exit code and standard output
// lines.
func bareRclone(ctx context.Context, t *testing.T, it *itest, defs []*syncrv1.RemoteDefinition, args ...string) (int, []string) {
	t.Helper()
	env, err := it.cfg.Env.Build(t.TempDir(), defs)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	res, _ := proc.New(proc.Options{}).Run(ctx, proc.Spec{
		Path: it.cfg.Binary, Args: args, Env: env.Vars(),
		Stdout: func(l proc.Line) { out = append(out, string(l.Data)) }, Stderr: func(proc.Line) {},
	})
	return res.ExitCode, out
}

// cryptPasswordTransfers are the transfers TestIntegrationCryptPasswordCheck
// runs from a crypt remote with the wrong password.
func cryptPasswordTransfers() []struct {
	name string
	mode syncrv1.RcloneTransferMode
	opts *syncrv1.RcloneTransferOptions
} {
	include := &syncrv1.RcloneTransferOptions{Filters: []*syncrv1.FilterRule{
		{Action: syncrv1.FilterAction_FILTER_ACTION_INCLUDE, Pattern: "photos/**"},
		{Action: syncrv1.FilterAction_FILTER_ACTION_EXCLUDE, Pattern: "**"},
	}}
	return []struct {
		name string
		mode syncrv1.RcloneTransferMode
		opts *syncrv1.RcloneTransferOptions
	}{
		{"sync", modeSync, nil},
		{"sync, dry run", modeSync, &syncrv1.RcloneTransferOptions{DryRun: true}},
		{"sync, fast list", modeSync, &syncrv1.RcloneTransferOptions{FastList: true}},
		{"sync, include filter", modeSync, include},
		{"copy", modeCopy, nil},
		{"move", modeMove, nil},
	}
}

// cryptPasswordCase runs TestIntegrationCryptPasswordCheck's checks for
// one file-name mode (names) over the remote store, in its folder base:
// data with folders only at the root written with the right password,
// then transfers from and into it, and listings, with the wrong one.
func cryptPasswordCase(ctx context.Context, t *testing.T, it *itest, store *syncrv1.RemoteDefinition, base, names, src string, newDir func(string) string) {
	t.Helper()
	const right, wrong = "the right pa:ss", "a mistyped pass"
	vault := func(pass, folder string) []*syncrv1.RemoteDefinition {
		d := cryptRemote("vault", store.GetAlias(), base+"/"+names+"-"+folder, pass)
		d.Options = map[string]string{"filename_encryption": names}
		return []*syncrv1.RemoteDefinition{store, d}
	}
	root := &syncrv1.Location{Remote: "vault"}
	if _, _, err := it.run(ctx, t, modeCopy, local(src), root, nil, vault(right, "src")...); err != nil {
		t.Fatalf("copy into the crypt remote: %v", err)
	}

	if names == "obfuscate" && store.GetBackendType() == "s3" {
		// What the check prevents, with the bare rclone: a sync from the
		// root, folder by folder, sees every garbled folder as empty;
		// with --fast-list it reads the files and fails, but an include
		// filter matches no garbled name.
		for _, args := range [][]string{
			{"sync"},
			{"sync", "--fast-list", "--filter=+ photos/**", "--filter=- **"},
		} {
			dst := newDir("bare")
			writeFiles(t, dst, map[string]int{"photos/2024/a.jpg": 7, "docs/c.txt": 3})
			code, _ := bareRclone(ctx, t, it, vault(wrong, "src"), append(args, "--", "vault:", dst)...)
			t.Logf("bare rclone %q from the obfuscated root with the wrong password: exit %d, the destination keeps %q",
				args, code, slices.Sorted(slices.Values(slicesOfKeys(tree(t, dst)))))
		}
	}

	// Transfers from the remote, with the wrong password.
	for _, from := range []string{"", "photos", "photos/2024"} {
		for _, tr := range cryptPasswordTransfers() {
			dst := newDir("dst")
			writeFiles(t, dst, map[string]int{"keep.txt": 10, "photos/2024/a.jpg": 7, "docs/c.txt": 3})
			before := tree(t, dst)
			_, _, err := it.run(ctx, t, tr.mode, &syncrv1.Location{Remote: "vault", Path: from}, local(dst), tr.opts, vault(wrong, "src")...)
			if runner.CodeOf(err) != syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED || !strings.Contains(err.Error(), "source: the encrypted remote cannot decrypt") ||
				!strings.Contains(err.Error(), "password is wrong") {
				t.Errorf("%s from vault:%q with the wrong password = %v (code %v)", tr.name, from, err, runner.CodeOf(err))
			}
			if err != nil && (strings.Contains(err.Error(), right) || strings.Contains(err.Error(), wrong)) {
				t.Errorf("error leaks a password: %v", err)
			}
			sameTree(t, tree(t, dst), before)
		}
	}
	// The remote still holds everything (move moved nothing).
	back := newDir("back")
	if _, _, err := it.run(ctx, t, modeCopy, root, local(back), nil, vault(right, "src")...); err != nil {
		t.Fatalf("copy back with the right password: %v", err)
	}
	sameTree(t, tree(t, back), tree(t, src))

	// Transfers into the remote, with the wrong password: a store with a
	// file at its root (obfuscated names list it, garbled, and a sync into
	// the root would delete it).
	seed := newDir("seed")
	writeFiles(t, seed, map[string]int{"top.txt": 20, "docs/c.txt": 6})
	if _, _, err := it.run(ctx, t, modeCopy, local(seed), root, nil, vault(right, "dst")...); err != nil {
		t.Fatalf("seed the destination store: %v", err)
	}
	_, stored := bareRclone(ctx, t, it, vault(right, "dst"), "lsf", "-R", "--", "vault:")
	news := newDir("new")
	writeFiles(t, news, map[string]int{"new.txt": 5})
	for _, to := range []string{"", "docs"} {
		for _, tr := range []syncrv1.RcloneTransferMode{modeSync, modeCopy} {
			_, _, err := it.run(ctx, t, tr, local(news), &syncrv1.Location{Remote: "vault", Path: to}, nil, vault(wrong, "dst")...)
			if runner.CodeOf(err) != syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED || !strings.Contains(err.Error(), "destination: the encrypted remote cannot decrypt") {
				t.Errorf("%v into vault:%q with the wrong password = %v (code %v)", tr, to, err, runner.CodeOf(err))
			}
		}
	}
	if _, now := bareRclone(ctx, t, it, vault(right, "dst"), "lsf", "-R", "--", "vault:"); !slices.Equal(now, stored) {
		t.Errorf("the destination store lists %q after the wrong password, want %q", now, stored)
	}

	// Probes: the browser and "Test remote" list through the check.
	p := it.prober(t)
	for _, path := range []string{"", "photos"} {
		l, err := p.List(ctx, "0195f0a4-3b7c-7d1e-9f00-0000000000c1", vault(wrong, "src"), &syncrv1.ProbeListRemote{Location: &syncrv1.Location{Remote: "vault", Path: path}})
		if runner.CodeOf(err) != syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED || !strings.Contains(err.Error(), "password is wrong") {
			t.Errorf("listing vault:%q with the wrong password = %v, %v (code %v)", path, l, err, runner.CodeOf(err))
		}
	}
	l, err := p.List(ctx, "0195f0a4-3b7c-7d1e-9f00-0000000000c2", vault(right, "src"), &syncrv1.ProbeListRemote{Location: root})
	if err != nil || len(l.GetEntries()) != 2 {
		t.Errorf("listing the root with the right password = %v, %v", l, err)
	}
}

// TestIntegrationCryptPasswordCheck: a crypt remote over MinIO read or
// written with the wrong password, in each file-name mode, through the
// runner and the pinned rclone (1.75.2). The data has folders only at its
// root: the layout in which, with obfuscated names, a sync from the root
// emptied its destination (review finding B1). Every transfer (sync,
// sync as a dry run, with fast_list or an include filter, copy, move),
// from the root or a folder, fails with EXECUTION_FAILED ("password is
// wrong") and changes nothing; so does every transfer into the remote,
// whose files stay as the right password wrote them; and so does a probe
// listing ("Test remote", the browser). With the right password everything
// works.
func TestIntegrationCryptPasswordCheck(t *testing.T) {
	it := newITest(t)
	minio := startMinIO(t)
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	src := it.dir(t, "src")
	writeFiles(t, src, map[string]int{"photos/2024/a.jpg": 5000, "photos/b.raw": 70_000, "docs/c.txt": 6})
	n := 0
	newDir := func(prefix string) string {
		n++
		return it.dir(t, fmt.Sprintf("%s-%d", prefix, n))
	}
	for _, names := range []string{"standard", "obfuscate", "off"} {
		t.Run(names, func(t *testing.T) { cryptPasswordCase(ctx, t, it, minio, "syncr-cc", names, src, newDir) })
	}

	// A remote that holds no file with data has nothing to decrypt: the
	// check passes, and a folder that its parent does not list is still
	// NOT_FOUND (checkCryptSource), here with obfuscated names.
	const right, wrong = "the right pa:ss", "a mistyped pass"
	empty := func(pass string) []*syncrv1.RemoteDefinition {
		d := cryptRemote("vault", "minio", "syncr-cc/empty-files", pass)
		d.Options = map[string]string{"filename_encryption": "obfuscate"}
		return []*syncrv1.RemoteDefinition{minio, d}
	}
	zero := newDir("zero")
	writeFiles(t, zero, map[string]int{"photos/zero.txt": 0})
	if _, _, err := it.run(ctx, t, modeCopy, local(zero), &syncrv1.Location{Remote: "vault"}, nil, empty(right)...); err != nil {
		t.Fatalf("copy an empty file: %v", err)
	}
	dst := newDir("dst-empty")
	writeFiles(t, dst, map[string]int{"keep.txt": 10})
	_, _, err := it.run(ctx, t, modeSync, &syncrv1.Location{Remote: "vault", Path: "photos"}, local(dst), nil, empty(wrong)...)
	if runner.CodeOf(err) != syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND || !strings.Contains(err.Error(), "password is wrong") {
		t.Errorf("sync from a folder of a remote without data, wrong password = %v (code %v)", err, runner.CodeOf(err))
	}
	if got := tree(t, dst); len(got) != 1 {
		t.Errorf("destination = %q", slicesOfKeys(got))
	}
}

// TestIntegrationCryptPasswordCheckSFTP: the same checks over an SFTP
// server, where a garbled folder is missing instead of empty: the check
// fails on it (or on the files it reads) all the same.
func TestIntegrationCryptPasswordCheckSFTP(t *testing.T) {
	it := newITest(t)
	_, box := startSFTP(t)
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	src := it.dir(t, "src")
	writeFiles(t, src, map[string]int{"photos/2024/a.jpg": 5000, "photos/b.raw": 70_000, "docs/c.txt": 6})
	n := 0
	newDir := func(prefix string) string {
		n++
		return it.dir(t, fmt.Sprintf("%s-%d", prefix, n))
	}
	for _, names := range []string{"standard", "obfuscate", "off"} {
		t.Run(names, func(t *testing.T) { cryptPasswordCase(ctx, t, it, box, "upload/cc", names, src, newDir) })
	}
}
