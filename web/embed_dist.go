//go:build webui

package web

import (
	"embed"
	"io/fs"
)

// dist is the Vite build output (web/dist, from npm run build).
//
//go:embed all:dist
var dist embed.FS

func files() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		// fs.Sub fails only for an invalid path; "dist" is valid.
		panic(err)
	}
	return sub
}
