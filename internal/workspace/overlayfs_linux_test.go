//go:build linux

package workspace_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jiaweisky/harbor/internal/workspace"
)

func TestIsolationOverlayFS(t *testing.T) {
	parent := t.TempDir()
	iso := filepath.Join(parent, "iso")
	src := filepath.Join(parent, "repo")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := workspace.CheckIsolation(workspace.IsolationOverlayFS, "", iso); err != nil {
		t.Skipf("overlayfs is not available: %v", err)
	}

	r := workspace.NewResolverWithIsolation(src, workspace.IsolationOverlayFS, iso, "")
	view, err := r.Resolve("ctx-a")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(view, "a.txt"))
	if err != nil || string(got) != "base" {
		t.Fatalf("view file = %q, %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(view, "a.txt"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(filepath.Join(src, "a.txt"))
	if err != nil || string(got) != "base" {
		t.Fatalf("source file = %q, %v; want unchanged", got, err)
	}

	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(filepath.Join(view, "a.txt")); !os.IsNotExist(err) {
		t.Fatal("overlayfs mount was not released")
	}
}
