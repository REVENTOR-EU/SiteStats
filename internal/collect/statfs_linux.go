//go:build linux

package collect

import "syscall"

// FilesystemUsage returns used and total bytes of the filesystem at path.
func FilesystemUsage(path string) (used, total uint64, err error) {
	var st syscall.Statfs_t
	if err = syscall.Statfs(path, &st); err != nil {
		return
	}
	bs := uint64(st.Bsize)
	total = st.Blocks * bs
	used = (st.Blocks - st.Bfree) * bs
	return
}
