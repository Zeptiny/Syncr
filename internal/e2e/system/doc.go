// Package system holds the end-to-end tests of the real binaries: it builds
// syncr-controller and syncr-worker, runs the controller on Postgres
// (testcontainers), enrolls workers with `syncr-worker setup`, runs real
// rclone and restic jobs through the Connect API in both link modes, and
// kills and restarts the processes mid-job. It contains no production code.
//
// The tests are behind the integration build tag and run on Linux only;
// they need SYNCR_TEST_RCLONE and SYNCR_TEST_RESTIC (the pinned tools) and
// Docker.
package system
