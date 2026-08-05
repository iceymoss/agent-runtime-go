//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package skills

import "io/fs"

// Platforms without a portable link-count contract fail closed.
func hardLinked(fs.FileInfo) bool { return true }
