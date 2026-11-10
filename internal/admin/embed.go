package admin

import (
	"embed"
	"io/fs"
)

// webAssets is the embedded management UI. It ships inside the binary so the
// container image needs no extra files.
//
//go:embed web
var webAssets embed.FS

// WebFS returns the admin web asset filesystem rooted at web/.
func WebFS() fs.FS {
	sub, err := fs.Sub(webAssets, "web")
	if err != nil {
		panic(err) // embed layout is fixed at build time
	}
	return sub
}
