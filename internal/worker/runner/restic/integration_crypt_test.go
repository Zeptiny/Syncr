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
// bucket folder syncr-rs-crypt/<folder> of the harness's MinIO, with
// password and file names encrypted as names says.
func cryptSource(password, names, folder string) *syncrv1.RemoteDefinition {
	return &syncrv1.RemoteDefinition{
		Alias:       "vault",
		BackendType: "crypt",
		Wrapped:     &syncrv1.Location{Remote: "minio", Path: "syncr-rs-crypt/" + folder},
		Options:     map[string]string{"filename_encryption": names},
		Secrets:     map[string]string{"password": password},
		ObscureKeys: []string{"password"},
	}
}

// TestIntegrationRemoteSourceCrypt backs up a crypt remote over MinIO,
// in each file-name mode, through a mirror: the mirror and the snapshot
// hold the decrypted files, while MinIO holds only encrypted names (unless
// names are not encrypted). The data has folders only at the remote's
// root. With the wrong password, every backup fails with EXECUTION_FAILED
// ("password is wrong") before anything reads the folder: from the root or
// a folder, with or without allow_empty, by mirror or by mount (the mount
// is never made; the worker here reports that it can mount, and a fake
// mounter records any attempt). The mirror keeps what the right password
// mirrored, and no snapshot is made.
func TestIntegrationRemoteSourceCrypt(t *testing.T) {
	h := newMinIOHarness(t)
	h.loc = &syncrv1.Location{Remote: "minio", Path: "syncr-rs-crypt-repo/repo"}
	mounter := &fakeMounter{}
	store := withSources(h, &syncrv1.RemoteSourceSupport{Mirror: true, Mount: true}, mounter)
	h.init(itPassword)
	const password, wrong = "crypt source pa:ss w'rd", "not the password"
	src := h.local("upload")
	writeFile(t, filepath.Join(src, "photos", "a.jpg"), randomBytes(t, 4000))
	writeFile(t, filepath.Join(src, "photos", "2024", "b.jpg"), randomBytes(t, 3000))
	writeFile(t, filepath.Join(src, "photos", "2024", "raw", "c.raw"), randomBytes(t, 1000))
	writeFile(t, filepath.Join(src, "docs", "d.txt"), randomBytes(t, 70_000))
	h.rclone("mkdir", "minio:syncr-rs-crypt")
	h.defs = append(h.defs, nil)
	snapshots := 0

	for _, names := range []string{"standard", "obfuscate", "off"} {
		h.defs[len(h.defs)-1] = cryptSource(password, names, names)
		h.rclone("copy", src, "vault:")
		if names != "off" {
			for _, o := range h.objects("syncr-rs-crypt/" + names) {
				if strings.Contains(o, "photos") || strings.Contains(o, "jpg") || strings.Contains(o, "2024") {
					t.Errorf("%s: MinIO holds the plain name %q", names, o)
				}
			}
		}

		out := h.mustRemoteRun("vault", "")
		snapshots++
		t.Logf("%s: backup of the root = %v", names, out)
		// The modes share the mirror (one source key): the first fills it,
		// the others find the same files unchanged.
		if out.GetSnapshotId() == "" || (names == "standard" && (out.GetFilesNew() != 4 || out.GetRemoteSource().GetMirroredFiles() != 4)) {
			t.Errorf("%s: first run = %v", names, out)
		}
		if got := h.tree(out.GetSnapshotId(), "/"); !slices.Equal(got, []string{"docs/", "photos/"}) {
			t.Errorf("%s: snapshot tree = %q", names, got)
		}
		want := []string{"docs/d.txt", "photos/2024/b.jpg", "photos/2024/raw/c.raw", "photos/a.jpg"}
		if got := mirrored(t, store); !slices.Equal(got, want) {
			t.Errorf("%s: mirror = %q", names, got)
		}

		h.defs[len(h.defs)-1] = cryptSource(wrong, names, names)
		for _, tt := range []struct {
			path       string
			method     syncrv1.ResticRemoteSourceMethod
			allowEmpty bool
		}{
			{"", mirrorMethod, false},
			{"", mirrorMethod, true},
			{"photos", mirrorMethod, true},
			{"photos/2024", mirrorMethod, false},
			{"", mountMethod, true},
			{"photos/2024", mountMethod, false},
		} {
			_, err := h.remoteRun(t.Context(), "vault", tt.path, nil, func(_ *syncrv1.ResticBackup, s *syncrv1.ResticBackupRemoteSource) {
				s.Method, s.AllowEmpty = tt.method, tt.allowEmpty
			})
			t.Logf("%s: backup of vault:%q (%v, allow_empty %v) with the wrong password: %v", names, tt.path, tt.method, tt.allowEmpty, err)
			mustFail(t, err, syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED)
			if !strings.Contains(err.Error(), "password is wrong") || strings.Contains(err.Error(), password) || strings.Contains(err.Error(), wrong) {
				t.Errorf("%s: backup of vault:%q with the wrong password = %v", names, tt.path, err)
			}
			if got := mirrored(t, store); !slices.Equal(got, want) {
				t.Errorf("%s: mirror after a backup with the wrong password = %q, want %q", names, got, want)
			}
		}
	}
	if snaps := h.snapshots(&syncrv1.ResticSnapshots{}); len(snaps) != snapshots {
		t.Errorf("snapshots = %d, want only the %d made with the right password", len(snaps), snapshots)
	}
	if len(mounter.reqs) != 0 {
		t.Errorf("mounted with the wrong password: %v", mounter.reqs)
	}
}
