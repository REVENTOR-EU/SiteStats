//go:build linux

package collect

import (
	"os"
	"syscall"
)

func fileIno(info os.FileInfo) uint64 {
	if sys, ok := info.Sys().(*syscall.Stat_t); ok {
		return sys.Ino
	}
	return 0
}

func openLog(path string) (*os.File, uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	var ino uint64
	if st, err := f.Stat(); err == nil {
		ino = fileIno(st)
	}
	return f, ino, nil
}
