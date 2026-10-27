//go:build linux

package workspace

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// cloneFile clones src into a new dst with FICLONE, leaving the two files
// independent after the first write to either side.
func cloneFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	if err := unix.IoctlFileClone(int(out.Fd()), int(in.Fd())); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return fmt.Errorf("FICLONE: %w", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return err
	}
	return os.Chmod(dst, info.Mode().Perm())
}
