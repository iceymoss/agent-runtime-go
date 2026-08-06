//go:build docsprod

package main

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var embeddedAssets embed.FS

func productionAssets() (fs.FS, error) {
	return fs.Sub(embeddedAssets, "dist")
}
