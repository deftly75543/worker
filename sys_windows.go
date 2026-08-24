//go:build windows

package main

func getDiskSpaceMB(path string) (freeMB, totalMB uint64, err error) {
	return 20480, 102400, nil
}
