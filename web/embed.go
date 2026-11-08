// Package web embeds the built web UI for the controller.
//
// The UI is a React single-page app built with Vite into web/dist (npm run
// build). Builds with the webui tag embed dist; other builds embed a
// placeholder page, so `go build ./...` works without Node and the git tree
// stays free of build output. Release, Docker and CI builds run
// `npm ci && npm run build` in web/ and then build with -tags webui.
package web

import "io/fs"

// FS returns the UI's files with index.html at the root, for api.Deps.UI.
func FS() fs.FS {
	return files()
}
