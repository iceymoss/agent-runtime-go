//go:build !docsprod

package main

import (
	"errors"
	"io/fs"
)

func productionAssets() (fs.FS, error) {
	return nil, errors.New("production documentation assets are unavailable; pass --dev-url or build with -tags docsprod")
}
