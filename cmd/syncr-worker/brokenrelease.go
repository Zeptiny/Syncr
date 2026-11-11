//go:build syncr_broken_release

package main

// brokenRelease makes this build a release that installs but does not stay
// up: its `run` goes through the whole start (the data directory's lock,
// the start-up cleanup, the tools, the link) and then fails. Only the
// rollback case of the real-service upgrade test builds it
// (go build -tags syncr_broken_release; TestIntegrationRealServiceUpgrade
// and the CI step that prepares its workers). Because it fails after
// everything a starting worker does, that test proves that no part of the
// start removes what an upgrade in progress needs to roll back.
const brokenRelease = true
