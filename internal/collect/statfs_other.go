//go:build !linux

package collect

// FilesystemUsage is a stub outside Linux.
func FilesystemUsage(path string) (used, total uint64, err error) { return 0, 0, nil }
