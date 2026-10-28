//go:build darwin

package workspace

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// cloneFile clones src into a new dst with clonefile, leaving the two files
// independent after the first write to either side.
func cloneFile(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	if err := unix.Clonefile(src, dst, 0); err != nil {
		return fmt.Errorf("clonefile: %w", err)
	}

	if err := os.Chmod(dst, info.Mode().Perm()); err != nil {
		_ = os.Remove(dst)
		return err
	}

	return nil
}
