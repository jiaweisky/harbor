//go:build !linux && !darwin && !freebsd && !windows

package workspace

import "io"

// lockIsolationRoot is a no-op on platforms without an implemented file lock.
func lockIsolationRoot(root string) (io.Closer, error) { return nil, nil }
