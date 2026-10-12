//go:build !linux

package collect

import "os"

func fileIno(info os.FileInfo) uint64 { return 0 }

func openLog(path string) (*os.File, uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	return f, 0, nil
}
