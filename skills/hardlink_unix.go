//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package skills

import (
	"io/fs"
	"syscall"
)

func hardLinked(info fs.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return !ok || stat.Nlink != 1
}
