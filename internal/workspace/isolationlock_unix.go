//go:build linux || darwin || freebsd

package workspace

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type isolationFileLock struct {
	f *os.File
}

func lockIsolationRoot(root string) (io.Closer, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}

	path := filepath.Join(root, ".harbor-isolation.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}

	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("isolation_root %s is already in use: %w", root, err)
	}

	return &isolationFileLock{f: f}, nil
}

func (l *isolationFileLock) Close() error {
	err := unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
	if closeErr := l.f.Close(); err == nil {
		err = closeErr
	}

	return err
}
