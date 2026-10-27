//go:build !linux

package workspace

import (
	"fmt"
	"path/filepath"
)

func mergedView(dst string) string { return filepath.Join(dst, "merged") }

func ensureOverlayFS(src, dst string) error {
	return fmt.Errorf("isolation %q is only supported on Linux", IsolationOverlayFS)
}

func releaseOverlayFS(dst string) error { return nil }

func checkOverlayFS(isolationRoot string) error {
	return fmt.Errorf("isolation %q is only supported on Linux", IsolationOverlayFS)
}

func cleanupOverlayMounts(isolationRoot string) error { return nil }
