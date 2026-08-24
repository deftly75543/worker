//go:build !windows

package main

import "syscall"

func getDiskSpaceMB(path string) (freeMB, totalMB uint64, err error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0, err
	}
	freeBytes := stat.Bavail * uint64(stat.Bsize)
	totalBytes := stat.Blocks * uint64(stat.Bsize)
	return freeBytes / (1024 * 1024), totalBytes / (1024 * 1024), nil
}
