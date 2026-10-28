//go:build linux || darwin

package workspace

import "syscall"

// sameFilesystem reports whether both paths are on the same device.
func sameFilesystem(left, right string) (bool, error) {
	var leftStat, rightStat syscall.Stat_t
	if err := syscall.Stat(left, &leftStat); err != nil {
		return false, err
	}

	if err := syscall.Stat(right, &rightStat); err != nil {
		return false, err
	}

	return leftStat.Dev == rightStat.Dev, nil
}
