//go:build integration

package restic

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
)

// cryptSource is the crypt remote "vault" that stores its data in the
// bucket folder syncr-rs-crypt/vault of the harness's MinIO, with password.
func cryptSource(password string) *syncrv1.RemoteDefinition {
	return &syncrv1.RemoteDefinition{
		Alias:       "vault",
		BackendType: "crypt",
		Wrapped:     &syncrv1.Location{Remote: "minio", Path: "syncr-rs-crypt/vault"},
		Secrets:     map[string]string{"password": password},
		ObscureKeys: []string{"password"},
	}
}

// TestIntegrationRemoteSourceCrypt backs up a folder of a crypt remote
// over MinIO through a mirror: the mirror and the snapshot hold the
// decrypted files, while MinIO holds only encrypted names. With the wrong
// password, even with allow_empty, the backup fails, leaves the mirror as
// it was and makes no snapshot. On a bucket a folder's name that encrypts
// to another prefix lists as empty, without an error; the check lists its
// parent, which fails on the names it cannot decrypt (the root) or lacks
// the folder (NOT_FOUND).
func TestIntegrationRemoteSourceCrypt(t *testing.T) {
	h := newMinIOHarness(t)
	h.loc = &syncrv1.Location{Remote: "minio", Path: "syncr-rs-crypt-repo/repo"}
	store := withSources(h, &syncrv1.RemoteSourceSupport{Mirror: true}, nil)
	h.init(itPassword)
	const password = "crypt source pa:ss w'rd"
	h.defs = append(h.defs, cryptSource(password))
	src := h.local("upload")
	writeFile(t, filepath.Join(src, "a.jpg"), randomBytes(t, 4000))
	writeFile(t, filepath.Join(src, "2024", "b.jpg"), randomBytes(t, 3000))
	writeFile(t, filepath.Join(src, "2024", "raw", "c.raw"), randomBytes(t, 1000))
	h.rclone("mkdir", "minio:syncr-rs-crypt")
	h.rclone("copy", src, "vault:photos")
	for _, o := range h.objects("syncr-rs-crypt/vault") {
		if strings.Contains(o, "photos") || strings.Contains(o, "jpg") || strings.Contains(o, "2024") {
			t.Errorf("MinIO holds the plain name %q", o)
		}
	}

	out := h.mustRemoteRun("vault", "photos")
	t.Logf("backup of vault:photos = %v", out)
	if out.GetFilesNew() != 3 || out.GetRemoteSource().GetMirroredFiles() != 3 {
		t.Errorf("first run = %v", out)
	}
	if got := h.tree(out.GetSnapshotId(), "/"); !slices.Equal(got, []string{"2024/", "a.jpg"}) {
		t.Errorf("snapshot tree = %q", got)
	}
	want := []string{"2024/b.jpg", "2024/raw/c.raw", "a.jpg"}
	if got := mirrored(t, store); !slices.Equal(got, want) {
		t.Errorf("mirror = %q", got)
	}

	h.defs[len(h.defs)-1] = cryptSource("not the password")
	for _, tt := range []struct {
		path string
		code syncrv1.JobErrorCode
		msg  string
	}{
		{"photos/2024", syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "password is wrong"},
		{"photos", syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "undecryptable"},
		{"", syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "undecryptable"},
	} {
		_, err := h.remoteRun(t.Context(), "vault", tt.path, nil, func(_ *syncrv1.ResticBackup, s *syncrv1.ResticBackupRemoteSource) {
			s.AllowEmpty = true
		})
		t.Logf("backup of vault:%s with the wrong password: %v", tt.path, err)
		mustFail(t, err, tt.code)
		if !strings.Contains(err.Error(), tt.msg) || strings.Contains(err.Error(), password) {
			t.Errorf("backup of vault:%s with the wrong password = %v, want %q", tt.path, err, tt.msg)
		}
		if got := mirrored(t, store); !slices.Equal(got, want) {
			t.Errorf("mirror after a backup with the wrong password = %q, want %q", got, want)
		}
	}
	if snaps := h.snapshots(&syncrv1.ResticSnapshots{}); len(snaps) != 1 {
		t.Errorf("snapshots = %v, want only the first one", snaps)
	}
}
