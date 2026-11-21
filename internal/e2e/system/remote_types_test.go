//go:build integration

package system

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/link"
	"github.com/Zeptiny/Syncr/internal/worker/ftptest"
)

// ftpPassword is the FTP account's password in TestSystemFTPRemote: a
// sentinel that must appear in no API response and in neither process's
// log.
const ftpPassword = "e2e-ftp-password-sentinel"

// TestSystemFTPRemote copies a worker folder to an FTP remote (rclone serve
// ftp; the remote has a base path) and back, with the real binaries
// through the API: the worker reports the ftp feature, the files arrive
// byte for byte, RunJob refuses checksum comparison with the FTP side and
// with a union that has the FTP remote as a member, and the password
// appears in no response or log.
func TestSystemFTPRemote(t *testing.T) {
	t.Parallel()
	rclone, restic := requirements(t)
	cbin, wbin := binaries(t)
	served := t.TempDir()
	srv := ftptest.Start(t, served, "e2e-ftp-user", ftpPassword)
	c := newController(t, cbin)
	w := setupWorker(t, c, wbin, rclone, restic, false)
	w.start()
	if info := c.waitOnline(w.id).GetInfo(); !slices.Contains(info.GetFeatures(), link.FeatureRcloneFTP) {
		t.Fatalf("the worker does not report %q: %v", link.FeatureRcloneFTP, info.GetFeatures())
	}
	ctx := t.Context()
	var seen []string // response text, searched for the password at the end

	created, err := c.remotes.CreateRemote(ctx, connect.NewRequest(&syncrv1.CreateRemoteRequest{Input: &syncrv1.RemoteInput{
		Name: "Old FTP", BackendType: "ftp",
		Options: map[string]string{"host": srv.Host, "port": srv.Port, "user": srv.User, "path": "/backups"},
		Secrets: map[string]string{"pass": srv.Pass},
	}}))
	if err != nil {
		t.Fatalf("CreateRemote(ftp): %v", err)
	}
	seen = append(seen, created.Msg.String())
	ftp := created.Msg.GetRemote().GetId()

	// 1. A worker folder onto the FTP server, under its base path.
	src := filepath.Join(w.root, "src")
	files := map[string]string{
		"report.txt":          "quarterly numbers",
		"plans/holiday ü.md":  "the plan",
		"plans/2026/data.csv": strings.Repeat("a,b,c\n", 5000),
	}
	for name, content := range files {
		write(t, filepath.Join(src, filepath.FromSlash(name)), content)
	}
	j, _ := c.watch(c.runJob(w.id, copyDef(local(src), onRemote(ftp, "docs"), 0)))
	wantStatus(t, j, syncrv1.JobStatus_JOB_STATUS_SUCCEEDED)
	seen = append(seen, j.String())
	for name, content := range files {
		if got := read(t, filepath.Join(served, "backups", "docs", filepath.FromSlash(name))); got != content {
			t.Errorf("the server holds %q = %q, want %q", name, got, content)
		}
	}

	// 2. And back into another worker folder.
	back := filepath.Join(w.root, "back")
	j, _ = c.watch(c.runJob(w.id, copyDef(onRemote(ftp, "docs"), local(back), 0)))
	wantStatus(t, j, syncrv1.JobStatus_JOB_STATUS_SUCCEEDED)
	seen = append(seen, j.String())
	for name, content := range files {
		if got := read(t, filepath.Join(back, filepath.FromSlash(name))); got != content {
			t.Errorf("copied back %q = %q, want %q", name, got, content)
		}
	}

	// 3. No checksums with the FTP side, directly or through a union.
	disk := c.createRemote(&syncrv1.RemoteInput{
		Name: "Disk", BackendType: "local", Options: map[string]string{"path": w.root},
	})
	union := c.createRemote(&syncrv1.RemoteInput{
		Name: "Pool", BackendType: "union",
		Members: []*syncrv1.UnionMember{
			{RemoteId: disk, Path: "pool", Mode: syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_READ_WRITE},
			{RemoteId: ftp, Path: "pool", Mode: syncrv1.UnionUpstreamMode_UNION_UPSTREAM_MODE_READ_ONLY},
		},
	})
	for _, dst := range []string{ftp, union} {
		def := copyDef(local(src), onRemote(dst, "docs"), 0)
		def.GetRcloneTransfer().Options = &syncrv1.RcloneTransferOptions{Checksum: true}
		_, err := c.jobs.RunJob(ctx, connect.NewRequest(&syncrv1.RunJobRequest{WorkerId: w.id, Definition: def}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument ||
			!slices.Contains(violations(err), "definition.rclone_transfer.options.checksum: is not available: a remote that keeps no checksums") {
			t.Errorf("RunJob with checksum into %s = %v (%q)", dst, err, violations(err))
		}
	}

	// 4. The password stays out of every response and log.
	for _, text := range seen {
		if strings.Contains(text, ftpPassword) {
			t.Errorf("a response contains the FTP password: %s", text)
		}
	}
	if strings.Contains(w.proc.out.String(), ftpPassword) || strings.Contains(c.proc.out.String(), ftpPassword) {
		t.Error("a process log contains the FTP password")
	}
}
