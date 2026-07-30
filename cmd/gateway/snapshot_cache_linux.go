//go:build linux

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// These hints are deliberately Linux-only: the accepted deployment and its
// cgroup memory accounting run on Linux. Other targets retain identical
// verification semantics and use the no-op implementation in the companion
// file instead of making the whole Gateway package platform-specific.
func adviseSequentialFile(file *os.File) {
	if file == nil {
		return
	}
	_ = unix.Fadvise(int(file.Fd()), 0, 0, unix.FADV_SEQUENTIAL)
	_ = unix.Fadvise(int(file.Fd()), 0, 0, unix.FADV_NOREUSE)
}

func dropFileCache(file *os.File, offset, length int64) {
	if file == nil || length <= 0 {
		return
	}
	_ = unix.Fadvise(int(file.Fd()), offset, length, unix.FADV_DONTNEED)
}
