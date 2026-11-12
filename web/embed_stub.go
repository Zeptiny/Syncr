//go:build !webui

package web

import (
	"embed"
	"io/fs"
)

// placeholder is served by builds without the webui tag.
//
//go:embed placeholder
var placeholder embed.FS

func files() fs.FS {
	sub, err := fs.Sub(placeholder, "placeholder")
	if err != nil {
		// fs.Sub fails only for an invalid path; "placeholder" is valid.
		panic(err)
	}
	return sub
}
