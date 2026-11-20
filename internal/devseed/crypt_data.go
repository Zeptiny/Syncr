package devseed

import (
	"time"

	"github.com/Zeptiny/Syncr/internal/domain"
)

// The encrypted remote "Vault" (rclone crypt): the admin's documents,
// encrypted on the worker and stored in the folder vaultFolder of B2
// offsite, which therefore cannot be deleted while Vault exists. Its jobs
// copy nas-01's documents into it and restore its tax folder on
// backup-host. Browsing Vault lists the documents; browsing B2 offsite's
// vaultFolder lists only encrypted names (cryptStoredFiles).

// vaultFolder is the folder of B2 offsite that Vault stores its data in.
const vaultFolder = "vault"

// vaultFiles are the files Vault shows (decrypted, as the worker lists
// them through the crypt remote).
func vaultFiles() []demoFile { return documentFiles("documents") }

// cryptStoredFiles are what the demo B2 bucket holds in vaultFolder: the
// same files under encrypted names (standard name encryption, base32),
// each 32 bytes of header and 16 bytes per 64 KiB block larger.
func cryptStoredFiles() []demoFile {
	names := []string{
		"8v2nh9hl0ruqd9u1g7m5tlhd8k/0fvh6k7cf1f6jtlvq5ok4rpbs8/2s6u0kkdm4s4mrlr2u93i4p4q0",
		"8v2nh9hl0ruqd9u1g7m5tlhd8k/0fvh6k7cf1f6jtlvq5ok4rpbs8/a1kvb2m2sd6ss4lv7hrfvhm8l0",
		"8v2nh9hl0ruqd9u1g7m5tlhd8k/0fvh6k7cf1f6jtlvq5ok4rpbs8/n3gq5d1d2c6j6p4e6u9e0vfv5c/7k2l4n8lu7mlhf6q0g9m1c9q6s",
		"8v2nh9hl0ruqd9u1g7m5tlhd8k/q3r2c8a4pm5a2u5g8n0f9e7h1k/s9jv0ld0pm4c8n1s5k2qoj9ab0/r4t1h6f5n2q8i6r0c3d5e7g9b4",
		"8v2nh9hl0ruqd9u1g7m5tlhd8k/q3r2c8a4pm5a2u5g8n0f9e7h1k/s9jv0ld0pm4c8n1s5k2qoj9ab0/c1m7u5b3l9q2s4e0f8k6h1n3r0",
		"8v2nh9hl0ruqd9u1g7m5tlhd8k/q3r2c8a4pm5a2u5g8n0f9e7h1k/l7s2k4h9d1m3o5q7g2b8e0j6c4/d6p0m9v2a8f4c1q7r5k3n9h2t6",
		"8v2nh9hl0ruqd9u1g7m5tlhd8k/fq4lr6c0tn8m2k1pv7o9i3d5e8",
		"8v2nh9hl0ruqd9u1g7m5tlhd8k/k0b4n8r2l6p1t9a3d7f5h0j2m4",
	}
	files := vaultFiles()
	out := make([]demoFile, 0, len(names))
	for i, n := range names {
		size := files[i%len(files)].size
		out = append(out, demoFile{vaultFolder + "/" + n, size + 32 + 16*(size/(64*kib)+1)})
	}
	return out
}

// demoCryptJobs are the jobs of Vault: a first copy of nas-01's documents
// into it, a sync of them since, and a restore of its tax folder onto
// backup-host. None compares checksums: Vault keeps none.
func demoCryptJobs() []demoJob {
	h, m, s, d := time.Hour, time.Minute, time.Second, 24*time.Hour
	return []demoJob{
		{owner: ownerAdmin, worker: "nas-01", mode: copyMode, src: loc{"", "/srv/docs"}, dst: loc{rVault, "documents"},
			status: domain.JobSucceeded, created: 4*d + 5*h, queue: 2 * s, run: 3*m + 12*s,
			stats: done(52*mib+300*kib, 11, 11), samples: 20},
		{owner: ownerAdmin, worker: "nas-01", mode: syncMode, src: loc{"", "/srv/docs"}, dst: loc{rVault, "documents"},
			status: domain.JobSucceeded, created: 30 * h, queue: 2 * s, run: 41 * s,
			stats: done(92*kib, 1, 11), samples: 5},
		{owner: ownerAdmin, worker: "backup-host", mode: copyMode, src: loc{rVault, "documents/taxes"}, dst: loc{"", "/backup/restores/taxes"},
			status: domain.JobSucceeded, created: 8 * h, queue: 3 * s, run: 28 * s,
			stats: done(48*mib+732*kib, 3, 3), samples: 5},
	}
}
