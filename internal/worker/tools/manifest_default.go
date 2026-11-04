package tools

// DefaultManifest returns the tools pinned by this release of Syncr.
//
// # Bumping a version
//
//  1. Pick the new release. rclone: https://github.com/rclone/rclone/releases,
//     restic: https://github.com/restic/restic/releases.
//  2. Download the release's SHA256SUMS file and check its OpenPGP
//     signature:
//     rclone:  https://downloads.rclone.org/vX.Y.Z/SHA256SUMS (clear-signed;
//     keys at https://rclone.org/KEYS, fingerprint
//     FBF7 37EC E9F8 AB18 604B D2AC 9393 5E02 FF3B 54FA);
//     restic:  https://github.com/restic/restic/releases/download/vX.Y.Z/SHA256SUMS
//     with SHA256SUMS.asc (key https://restic.net/gpg-key-alex.asc,
//     fingerprint CF8F 18F2 8445 7597 3F79 D4E1 91A6 868B D3F7 A907).
//  3. In the Spec below, update Version, and for every platform the URL, the
//     SHA256 (the digest of the archive, copied from SHA256SUMS) and, for
//     zip archives, Entry (rclone's zip nests the binary in a directory named
//     after the release).
//  4. Run `go test ./internal/worker/tools/...` (the manifest is validated
//     offline), then `go test -tags integration ./internal/worker/tools/...`,
//     which downloads the real linux/amd64 archives and runs them, and
//     `make test-integration` for the runners.
//
// Adding a tool, an OS or an architecture is adding a Spec or an Artifact
// entry here; no other code changes. The checksums are compiled in on
// purpose: the worker never trusts a digest fetched at runtime.
func DefaultManifest() Manifest {
	const (
		rcloneVersion = "1.75.2"
		resticVersion = "0.19.1"
		rcloneBase    = "https://downloads.rclone.org/v" + rcloneVersion + "/rclone-v" + rcloneVersion + "-"
		resticBase    = "https://github.com/restic/restic/releases/download/v" + resticVersion + "/restic_" + resticVersion + "_"
	)
	return Manifest{Tools: []Spec{
		{
			Name:           "rclone",
			Version:        rcloneVersion,
			VersionArgs:    []string{"version"},
			VersionPattern: `^rclone v(\S+)`,
			Artifacts: map[Platform]Artifact{
				{OS: "linux", Arch: "amd64"}: {
					URL:    rcloneBase + "linux-amd64.zip",
					SHA256: "349ac8fba6ff65d6247043f1750cdcb518ec5d500ef91463a10d37c0ccdf3702",
					Format: FormatZip,
					Entry:  "rclone-v" + rcloneVersion + "-linux-amd64/rclone",
				},
				{OS: "linux", Arch: "arm64"}: {
					URL:    rcloneBase + "linux-arm64.zip",
					SHA256: "7e1e8d69654941b7b7df84ee74c5f7fb09cce7d5496947fb7bf16b55ff427d10",
					Format: FormatZip,
					Entry:  "rclone-v" + rcloneVersion + "-linux-arm64/rclone",
				},
				{OS: "windows", Arch: "amd64"}: {
					URL:    rcloneBase + "windows-amd64.zip",
					SHA256: "8a9db2bad1dc79a65b3348fd857069598e2aab7cf4d118b77b1df23055aff818",
					Format: FormatZip,
					Entry:  "rclone-v" + rcloneVersion + "-windows-amd64/rclone.exe",
				},
			},
		},
		{
			Name:           "restic",
			Version:        resticVersion,
			VersionArgs:    []string{"version"},
			VersionPattern: `^restic (\S+)`,
			Artifacts: map[Platform]Artifact{
				{OS: "linux", Arch: "amd64"}: {
					URL:    resticBase + "linux_amd64.bz2",
					SHA256: "f415415624dcc452f2a02b8c33641791a8c6d6d3b65bbb3543fcf9a25151585c",
					Format: FormatBzip2,
				},
				{OS: "linux", Arch: "arm64"}: {
					URL:    resticBase + "linux_arm64.bz2",
					SHA256: "a5f64aaab53d51e311fa3829124c5b703f2d14cf187d8640b6be3b2b49376465",
					Format: FormatBzip2,
				},
				{OS: "windows", Arch: "amd64"}: {
					URL:    resticBase + "windows_amd64.zip",
					SHA256: "da948ad707ed690426473aaba2046cd61f8f90f6f0e7dab6be0d5796531de67d",
					Format: FormatZip,
					Entry:  "restic_" + resticVersion + "_windows_amd64.exe",
				},
			},
		},
	}}
}
