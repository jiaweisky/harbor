//go:build windows

package workspace

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
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
	var overlapped windows.Overlapped
	err = windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, &overlapped,
	)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("isolation_root %s is already in use: %w", root, err)
	}
	return &isolationFileLock{f: f}, nil
}

func (l *isolationFileLock) Close() error {
	var overlapped windows.Overlapped
	err := windows.UnlockFileEx(windows.Handle(l.f.Fd()), 0, 1, 0, &overlapped)
	if closeErr := l.f.Close(); err == nil {
		err = closeErr
	}
	return err
}
