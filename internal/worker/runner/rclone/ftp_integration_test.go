//go:build integration

package rclone

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/internal/worker/ftptest"
)

// ftpUser and ftpPass are the credentials of the test FTP servers, with
// characters that would break a connection string or an INI file.
const ftpUser, ftpPass = "syncr ftp", `p'a"s,s w:rd`

// startFTP serves a new folder over FTP (rclone serve ftp) and returns the
// server; its remote is srv.Remote(alias).
func startFTP(t *testing.T, opts ...ftptest.Option) *ftptest.Server {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "served")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return ftptest.Start(t, dir, ftpUser, ftpPass, opts...)
}

// TestIntegrationFTP copies to and from an FTP server through the runner,
// and runs the probes on it.
func TestIntegrationFTP(t *testing.T) {
	it := newITest(t)
	def := startFTP(t).Remote("ftpbox")
	roundTrip(t, it, def, "syncr-it")
	// rclone serve ftp reports no usage.
	probeRemote(t, it, def, "syncr-it", "syncr-it/nope", false)
}

// TestIntegrationBisyncFTP runs a two-way sync between a local folder and
// a folder on an FTP server, which keeps no checksums and, with rclone
// serve ftp, no modification times: rclone compares sizes only and warns
// that it cannot compare times, and the pair works.
func TestIntegrationBisyncFTP(t *testing.T) {
	it := newBisyncITest(t)
	// The test changes the server's side in its folder directly.
	srv := startFTP(t, ftptest.NoDirCache())
	def := srv.Remote("ftpbox")
	far := filepath.Join(srv.Dir, "pair")
	put(t, it.a, map[string]string{"a1.txt": "from a", "sub/s.txt": "shared"})
	put(t, far, map[string]string{"b1.txt": "from the server"})
	spec := func() *syncrv1.JobSpec {
		s := it.spec(nil, func(op *syncrv1.RcloneBisync) { op.Path2 = &syncrv1.Location{Remote: "ftpbox", Path: "pair"} })
		s.Remotes = []*syncrv1.RemoteDefinition{def}
		return s
	}
	inStep := func() []string {
		t.Helper()
		sameTree(t, tree(t, far), tree(t, it.a))
		return names(t, it.a)
	}

	// The first run of the pair resyncs: the union of both sides.
	out, res := it.sync(t, spec())
	if out.GetResyncReason() != reasonFirstRun {
		t.Errorf("first run output = %v", out)
	}
	if got, want := inStep(), []string{"a1.txt", "b1.txt", "sub/s.txt"}; !slices.Equal(got, want) {
		t.Errorf("after the first run: %v, want %v", got, want)
	}
	t.Logf("first run warnings: %q", res.Warnings)

	// New files spread both ways, and a deletion on the server spreads to
	// the local folder, without a resync.
	put(t, it.a, map[string]string{"new-a.txt": "new locally"})
	put(t, far, map[string]string{"new-b.txt": "new on the server"})
	remove(t, far, "a1.txt")
	out, res = it.sync(t, spec())
	if out.GetResyncReason() != reasonNone {
		t.Errorf("second run resynced: %v", out)
	}
	if got, want := inStep(), []string{"b1.txt", "new-a.txt", "new-b.txt", "sub/s.txt"}; !slices.Equal(got, want) {
		t.Errorf("after the second run: %v, want %v", got, want)
	}
	if res.Stats.Deletes != 1 {
		t.Errorf("deletes = %d, want 1", res.Stats.Deletes)
	}
	t.Logf("second run warnings: %q", res.Warnings)
}
