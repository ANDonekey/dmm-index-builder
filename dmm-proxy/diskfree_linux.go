//go:build linux

package main

import "syscall"

// freeBytes 返回目录所在分区的可用字节数（非 root 也准确，用的是 Bavail）。
func freeBytes(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
