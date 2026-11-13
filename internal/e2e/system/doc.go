// Package system holds the end-to-end tests of the real binaries: it builds
// syncr-controller and syncr-worker, runs the controller on Postgres
// (testcontainers), enrolls workers with `syncr-worker setup`, runs real
// rclone and restic jobs and worker probes through the Connect API in both
// link modes (restic against MinIO, from a new repository to the deletion
// of its data; a two-way sync of two workers' folders through MinIO; the
// file browser's folder sizes, snapshot search and comparison, restores of
// selected items of a subfolder, and copies and moves of selections with
// the web's golden filter file), and kills and restarts the processes
// mid-job. It contains no production code.
//
// The tests are behind the integration build tag and run on Linux only;
// they need SYNCR_TEST_RCLONE and SYNCR_TEST_RESTIC (the pinned tools) and
// Docker.
package system
