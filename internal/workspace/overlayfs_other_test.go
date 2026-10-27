//go:build !linux

package workspace_test

import (
	"strings"
	"testing"

	"github.com/jiaweisky/harbor/internal/workspace"
)

func TestIsolationOverlayFSUnsupported(t *testing.T) {
	err := workspace.CheckIsolation(workspace.IsolationOverlayFS, "", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "Linux") {
		t.Fatalf("CheckIsolation error = %v, want a Linux-only error", err)
	}
}
