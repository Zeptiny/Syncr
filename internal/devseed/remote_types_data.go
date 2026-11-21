package devseed

import (
	"strings"
	"time"

	"github.com/Zeptiny/Syncr/internal/app/remotes"
	"github.com/Zeptiny/Syncr/internal/domain"
)

// The remote types of 2.3: one demo remote each of SMB ("NAS share"), FTP
// ("Old FTP"), Azure Blob Storage ("Azure archive") and Google Cloud
// Storage ("GCS backups"), with canned listings (remoteTree) and a copy
// from NAS share into B2 offsite. NAS share and Old FTP keep no checksums,
// so the job's editor shows the checksum note; attic-nas, an old build,
// reports none of their link features, so the UI shows it outdated for
// jobs on them.

// Demo remote names of the remote types of 2.3.
const (
	rNASShare = "NAS share"
	rOldFTP   = "Old FTP"
	rAzure    = "Azure archive"
	rGCS      = "GCS backups"
)

// Roots of the remote types' demo remotes (see demoRemoteRoots): the SMB
// share, the FTP base path, the Azure container and the GCS bucket.
const (
	nasShareRoot = "family"
	oldFTPRoot   = "pub/backups"
	azureRoot    = "cold-archive"
	gcsRoot      = "syncr-demo-backups"
)

// demoGCSKey is the demo service account key: a made-up key in the form
// of a key file, which the gcs descriptor accepts. Its private key is no
// key and no such account exists.
func demoGCSKey() string {
	return strings.Join([]string{
		`{`,
		`  "type": "service_account",`,
		`  "project_id": "syncr-demo",`,
		`  "private_key_id": "0123456789abcdef0123456789abcdef01234567",`,
		`  "private_key": "-----BEGIN PRIVATE KEY-----\nDEMO-ONLY-NOT-A-REAL-KEY\n-----END PRIVATE KEY-----\n",`,
		`  "client_email": "syncr-demo@syncr-demo.iam.gserviceaccount.com",`,
		`  "client_id": "000000000000000000001",`,
		`  "auth_uri": "https://accounts.google.com/o/oauth2/auth",`,
		`  "token_uri": "https://oauth2.googleapis.com/token"`,
		`}`,
	}, "\n") + "\n"
}

// demoRemoteTypesRemotes are the remotes of the remote types of 2.3 (the
// admin's). The credentials are demo values for hosts and accounts that do
// not exist.
//
//nolint:gosec // G101: public demo credentials
func demoRemoteTypesRemotes() []demoRemote {
	return []demoRemote{
		{owner: ownerAdmin, input: remotes.Input{Name: rNASShare, Type: "smb",
			Options: map[string]string{"host": "nas.local", "user": "syncr", "share": nasShareRoot},
			Secrets: map[string]string{"pass": "syncr-demo-smb-password"}}},
		{owner: ownerAdmin, input: remotes.Input{Name: rOldFTP, Type: "ftp",
			Options: map[string]string{"host": "ftp.example.net", "user": "syncr", "explicit_tls": "true", "path": "/" + oldFTPRoot},
			Secrets: map[string]string{"pass": "syncr-demo-ftp-password"}}},
		{owner: ownerAdmin, input: remotes.Input{Name: rAzure, Type: "azureblob",
			Options: map[string]string{"account": "syncrdemo", "container": azureRoot, "access_tier": "archive"},
			Secrets: map[string]string{"key": "c3luY3ItZGVtby1henVyZS1hY2NvdW50LWtleQ=="}}},
		{owner: ownerAdmin, input: remotes.Input{Name: rGCS, Type: "gcs",
			Options: map[string]string{"bucket": gcsRoot, "location": "europe-west3", "storage_class": "NEARLINE"},
			Secrets: map[string]string{"service_account_credentials": demoGCSKey()}}},
	}
}

// demoRemoteTypesJobs copies the family documents from NAS share into B2
// offsite: without checksums, which NAS share keeps none of.
func demoRemoteTypesJobs() []demoJob {
	m, s, h := time.Minute, time.Second, time.Hour
	return []demoJob{
		{owner: ownerAdmin, worker: "nas-01", mode: copyMode, src: loc{rNASShare, "Documents"}, dst: loc{rB2, "nas-share/documents"},
			status: domain.JobSucceeded, created: 26 * h, queue: 2 * s, run: 4*m + 37*s,
			stats: done(61*mib+380*kib, 13, 13), samples: 20,
			options: ptrOptions(4, false)},
	}
}

// remoteTypesTree is the content of the demo remote of an rclone backend
// of 2.3; ok is false for other backends.
func remoteTypesTree(backend string) ([]demoFile, bool) {
	switch backend {
	case "smb":
		return documentFiles("Documents"), true
	case "ftp":
		return []demoFile{
			{"2019/website-2019-12.tar.gz", 840 * mib},
			{"2020/website-2020-12.tar.gz", 910 * mib},
			{"2021/website-2021-06.tar.gz", 960 * mib},
			{"readme.txt", 1 * kib},
		}, true
	case "azureblob":
		return []demoFile{
			{"office/2024/mailbox-2024.pst", 6*gib + 200*mib},
			{"office/2025/mailbox-2025.pst", 7*gib + 40*mib},
			{"scans/archive-2023.zip", 2*gib + 512*mib},
		}, true
	case "gcs":
		return resticLayout("restic/laptop"), true
	}
	return nil, false
}
