//go:build integration

package system

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	syncrv1 "github.com/Zeptiny/Syncr/gen/go/syncr/v1"
	"github.com/Zeptiny/Syncr/gen/go/syncr/v1/syncrv1connect"
	"github.com/Zeptiny/Syncr/internal/link"
)

// Passwords of the encrypted remotes in TestSystemEncryptedRemote:
// sentinels that must appear in no API response and in neither process's
// log.
const (
	cryptPassword = "e2e-crypt-password-sentinel"
	cryptSalt     = "e2e-crypt-salt-sentinel"
	cryptTypo     = "e2e-crypt-password-typo"
)

// plainMarker is in every file the test encrypts; it must never reach
// MinIO.
const plainMarker = "PLAINTEXT-MARKER-7f3a"

// encryptedName matches a file or folder name rclone's crypt writes with
// standard name encryption and the default encoding (base32hex, lower
// case, no padding).
var encryptedName = regexp.MustCompile(`^[0-9a-v]+$`)

// syncDef is an rclone sync definition.
func syncDef(src, dst *syncrv1.JobLocation) *syncrv1.JobDefinition {
	return &syncrv1.JobDefinition{Operation: &syncrv1.JobDefinition_RcloneTransfer{RcloneTransfer: &syncrv1.RcloneTransferDefinition{
		Mode: syncrv1.RcloneTransferMode_RCLONE_TRANSFER_MODE_SYNC, Source: src, Destination: dst,
	}}}
}

// cryptRemote is the input of an encrypted remote that stores its data in
// folder of the remote on.
func cryptRemote(name, on, folder, password string) *syncrv1.RemoteInput {
	return &syncrv1.RemoteInput{
		Name: name, BackendType: "crypt",
		Wrapped: &syncrv1.WrappedRemote{RemoteId: on, Path: folder},
		Secrets: map[string]string{"password": password, "password2": cryptSalt},
	}
}

// TestSystemEncryptedRemote copies a worker folder into an encrypted remote
// stored in a folder of MinIO and back, with the real binaries through the
// API. MinIO holds only encrypted names and no plaintext (read back raw
// with a copy from the S3 remote), the files come back byte for byte, and
// RunJob refuses checksum comparison with the encrypted side. A second
// encrypted remote over the same folder with a mistyped password, with
// standard or obfuscated names, fails its jobs (the worker reads one of its
// files first, which does not decrypt; on S3 a folder would otherwise list
// as empty) and its "Test remote", and a sync from it deletes nothing from
// its destination, whatever the folder; a folder that does not exist is
// NOT_FOUND. No password appears in a response or a log.
func TestSystemEncryptedRemote(t *testing.T) {
	t.Parallel()
	rclone, restic := requirements(t)
	cbin, wbin := binaries(t)
	endpoint := startMinIO(t)
	c := newController(t, cbin)
	w := setupWorker(t, c, wbin, rclone, restic, false)
	w.start()
	if info := c.waitOnline(w.id).GetInfo(); !slices.Contains(info.GetFeatures(), link.FeatureRcloneCrypt) {
		t.Fatalf("the worker does not report %q: %v", link.FeatureRcloneCrypt, info.GetFeatures())
	}
	ctx := t.Context()
	var seen []string // response text, searched for the passwords at the end

	minio := c.createRemote(&syncrv1.RemoteInput{
		Name: "MinIO", BackendType: "s3",
		Options: map[string]string{
			"provider": "Minio", "endpoint": endpoint, "region": "us-east-1",
			"access_key_id": minioUser, "bucket": "syncr-crypt", "force_path_style": "true",
		},
		Secrets: map[string]string{"secret_access_key": minioPassword},
	})
	created, err := c.remotes.CreateRemote(ctx, connect.NewRequest(&syncrv1.CreateRemoteRequest{Input: cryptRemote("Vault", minio, "vault", cryptPassword)}))
	if err != nil {
		t.Fatalf("CreateRemote(Vault): %v", err)
	}
	seen = append(seen, created.Msg.String())
	vault := created.Msg.GetRemote()
	if vault.GetWrapped().GetRemoteId() != minio || vault.GetWrapped().GetPath() != "vault" ||
		!slices.Equal(vault.GetSecretsSet(), []string{"password", "password2"}) {
		t.Errorf("the encrypted remote = %v", vault)
	}
	got, err := c.remotes.GetRemote(ctx, connect.NewRequest(&syncrv1.GetRemoteRequest{RemoteId: minio}))
	if err != nil || !slices.Equal(got.Msg.GetRemote().GetWrappedBy(), []string{vault.GetId()}) {
		t.Errorf("GetRemote(MinIO) = %v (%v), want it wrapped by the encrypted remote", got, err)
	}

	// 1. A worker folder into the encrypted remote.
	src := filepath.Join(w.root, "src")
	plain := map[string]string{
		"secret-report.txt":        plainMarker + " quarterly numbers",
		"plans/holiday plan.md":    plainMarker + " the plan",
		"plans/2026/budget ü.xlsx": plainMarker + strings.Repeat(" budget line", 20000), // several 64 KiB blocks
	}
	for name, content := range plain {
		write(t, filepath.Join(src, filepath.FromSlash(name)), content)
	}
	j, _ := c.watch(c.runJob(w.id, copyDef(local(src), onRemote(vault.GetId(), "docs"), 0)))
	wantStatus(t, j, syncrv1.JobStatus_JOB_STATUS_SUCCEEDED)
	seen = append(seen, j.String())
	if j.GetStats().GetFiles() != int64(len(plain)) {
		t.Errorf("upload stats = %v, want %d files", j.GetStats(), len(plain))
	}

	// 2. MinIO holds only encrypted names and data: its folder, copied raw
	// from the S3 remote, has as many files, none named or containing
	// anything of the originals.
	raw := filepath.Join(w.root, "raw")
	j, _ = c.watch(c.runJob(w.id, copyDef(onRemote(minio, "vault"), local(raw), 0)))
	wantStatus(t, j, syncrv1.JobStatus_JOB_STATUS_SUCCEEDED)
	n := 0
	err = filepath.WalkDir(raw, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == raw {
			return err
		}
		if !encryptedName.MatchString(d.Name()) {
			t.Errorf("MinIO holds a name that is not encrypted: %q", d.Name())
		}
		if d.IsDir() {
			return nil
		}
		n++
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(b, []byte(plainMarker)) || bytes.Contains(b, []byte("budget line")) {
			t.Errorf("MinIO holds plaintext in %s", d.Name())
		}
		if !bytes.HasPrefix(b, []byte("RCLONE\x00\x00")) {
			t.Errorf("%s does not start with crypt's file header", d.Name())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != len(plain) {
		t.Errorf("MinIO holds %d files, want %d", n, len(plain))
	}

	// 3. Back into another worker folder, byte for byte.
	back := filepath.Join(w.root, "back")
	j, _ = c.watch(c.runJob(w.id, copyDef(onRemote(vault.GetId(), "docs"), local(back), 0)))
	wantStatus(t, j, syncrv1.JobStatus_JOB_STATUS_SUCCEEDED)
	sameFiles(t, src, back)
	sameFiles(t, back, src)

	// 4. Checksums cannot be compared with an encrypted side.
	checksum := copyDef(local(src), onRemote(vault.GetId(), "docs"), 0)
	checksum.GetRcloneTransfer().Options = &syncrv1.RcloneTransferOptions{Checksum: true}
	_, err = c.jobs.RunJob(ctx, connect.NewRequest(&syncrv1.RunJobRequest{WorkerId: w.id, Definition: checksum}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument ||
		!slices.Contains(violations(err), "definition.rclone_transfer.options.checksum: is not available: a remote that keeps no checksums") {
		t.Errorf("RunJob with checksum = %v (%q)", err, violations(err))
	}

	// 5. A sync from a folder that cannot be read deletes nothing: a
	// mistyped password on a second remote over the same folder, with
	// standard names or obfuscated ones (whose root lists garbled names,
	// and on S3 every garbled folder as empty), or a folder that does not
	// exist.
	typo := c.createRemote(cryptRemote("Vault (typo)", minio, "vault", cryptTypo))
	obfuscated := cryptRemote("Obfuscated", minio, "obfuscated", cryptPassword)
	obfuscated.Options = map[string]string{"filename_encryption": "obfuscate"}
	obf := c.createRemote(obfuscated)
	j, _ = c.watch(c.runJob(w.id, copyDef(local(src), onRemote(obf, "docs"), 0)))
	wantStatus(t, j, syncrv1.JobStatus_JOB_STATUS_SUCCEEDED)
	obfuscated.Name, obfuscated.Secrets = "Obfuscated (typo)", map[string]string{"password": cryptTypo, "password2": cryptSalt}
	obfTypo := c.createRemote(obfuscated)
	kept := filepath.Join(w.root, "kept")
	write(t, filepath.Join(kept, "keep.txt"), "must survive")
	write(t, filepath.Join(kept, "docs", "plans", "holiday plan.md"), "must survive too")
	for _, tt := range []struct {
		name   string
		src    *syncrv1.JobLocation
		code   syncrv1.JobErrorCode
		marker string
	}{
		// The worker reads the first block of one of the remote's files
		// before the sync, from its root: it does not decrypt.
		{"the root with the wrong password", onRemote(typo, ""), syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "password is wrong"},
		{"a sub-folder with the wrong password", onRemote(typo, "docs/plans"), syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "password is wrong"},
		{"a top-level folder with the wrong password", onRemote(typo, "docs"), syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "undecryptable"},
		{"the obfuscated root with the wrong password", onRemote(obfTypo, ""), syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "bad password?"},
		{"an obfuscated folder with the wrong password", onRemote(obfTypo, "docs"), syncrv1.JobErrorCode_JOB_ERROR_CODE_EXECUTION_FAILED, "password is wrong"},
		{"a folder that does not exist", onRemote(vault.GetId(), "docs/missing"), syncrv1.JobErrorCode_JOB_ERROR_CODE_NOT_FOUND, "is not in its parent folder"},
	} {
		j, _ := c.watch(c.runJob(w.id, syncDef(tt.src, local(kept))))
		seen = append(seen, j.String())
		if j.GetStatus() != syncrv1.JobStatus_JOB_STATUS_FAILED || j.GetError().GetCode() != tt.code ||
			!strings.Contains(j.GetError().GetMessage(), tt.marker) {
			t.Errorf("%s: job %v error %v, want failed %v with %q", tt.name, j.GetStatus(), j.GetError(), tt.code, tt.marker)
		}
		if got := read(t, filepath.Join(kept, "keep.txt")); got != "must survive" {
			t.Errorf("%s: the destination's file = %q", tt.name, got)
		}
		if got := read(t, filepath.Join(kept, "docs", "plans", "holiday plan.md")); got != "must survive too" {
			t.Errorf("%s: the destination's file in a folder = %q", tt.name, got)
		}
	}

	// "Test remote" reads one file too: it fails with the wrong password,
	// in either name mode, and passes with the right one.
	probes := syncrv1connect.NewProbeServiceClient(c.client(), c.base)
	for _, tt := range []struct {
		id string
		ok bool
	}{{vault.GetId(), true}, {obf, true}, {typo, false}, {obfTypo, false}} {
		res, err := probes.TestRemote(ctx, connect.NewRequest(&syncrv1.TestRemoteRequest{RemoteId: tt.id, WorkerId: w.id}))
		if err != nil {
			t.Fatalf("TestRemote(%s): %v", tt.id, err)
		}
		seen = append(seen, res.Msg.String())
		if res.Msg.GetOk() != tt.ok || (!tt.ok && !strings.Contains(res.Msg.GetError(), "password is wrong")) {
			t.Errorf("TestRemote(%s) = %v, want ok %v", tt.id, res.Msg, tt.ok)
		}
	}

	// 6. The wrapped remote cannot be deleted while the encrypted remotes
	// store their data in it.
	_, err = c.remotes.DeleteRemote(ctx, connect.NewRequest(&syncrv1.DeleteRemoteRequest{RemoteId: minio}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "stores the data of encrypted remote") {
		t.Errorf("DeleteRemote(MinIO) = %v", err)
	}

	for _, secret := range []string{cryptPassword, cryptSalt, cryptTypo, minioPassword} {
		for _, text := range seen {
			if strings.Contains(text, secret) {
				t.Errorf("a response contains a secret: %s", text)
			}
		}
		if strings.Contains(w.proc.out.String(), secret) || strings.Contains(c.proc.out.String(), secret) {
			t.Errorf("a secret appears in a log (%d bytes long)", len(secret))
		}
	}
}
