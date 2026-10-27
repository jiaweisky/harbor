package workspace

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// fileCopier materializes one regular file. The directory walker creates the
// parent directory, symlinks, and special-file policy shared by every strategy.
type fileCopier func(src, dst string) error

// copyStrategies maps each copy strategy to its file copier. Availability on a
// specific filesystem is a runtime property checked by CheckIsolation.
var copyStrategies = map[CopyStrategy]fileCopier{
	CopyFull: copyFile,
	CopyCow:  cloneFile,
}

// ensureCopy materializes dst atomically: a failed or interrupted copy never
// leaves a directory that a later Resolve could mistake for a complete tree.
func ensureCopy(src, dst string, copyRegular fileCopier) error {
	if _, err := os.Stat(dst); err == nil {
		return nil // already provisioned for this context
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(dst)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, ".tmp-"+filepath.Base(dst)+"-")
	if err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	if err := copyTree(src, tmp, copyRegular); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	return nil
}

func copyTree(src, dst string, copyRegular fileCopier) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case d.Type()&os.ModeSymlink != 0:
			return copySymlink(path, target)
		case d.Type().IsRegular():
			return copyRegular(path, target)
		default:
			return nil // skip sockets, fifos, and device nodes
		}
	})
}

// copySymlink recreates a symbolic link with the same target, so copy isolation
// does not follow the link and duplicate (or read) whatever it points to.
func copySymlink(src, dst string) error {
	link, err := os.Readlink(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.Symlink(link, dst)
}

func copyFile(src, dst string) error {
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
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, info.Mode().Perm())
}

// cleanupCopyTemps removes staging directories left by a copy that was
// interrupted before its atomic rename. It runs before serving tasks.
func cleanupCopyTemps(isolationRoot string) error {
	if isolationRoot == "" {
		return nil
	}
	entries, err := os.ReadDir(isolationRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), ".tmp-ctx-") {
			continue
		}
		if err := os.RemoveAll(filepath.Join(isolationRoot, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

// probeClone verifies the copy strategy works on isolationRoot before any task
// starts, so an unsupported filesystem is a startup error rather than a
// per-request failure.
func probeClone(isolationRoot string) error {
	if isolationRoot == "" {
		return fmt.Errorf("sandbox.isolation_root is required for copy strategy %q", CopyCow)
	}
	if err := os.MkdirAll(isolationRoot, 0o755); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(isolationRoot, ".harbor-cow-probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("harbor"), 0o600); err != nil {
		return err
	}
	if err := cloneFile(src, filepath.Join(dir, "dst")); err != nil {
		return fmt.Errorf("copy_strategy %q is not available at %s: %w", CopyCow, isolationRoot, err)
	}
	return nil
}
