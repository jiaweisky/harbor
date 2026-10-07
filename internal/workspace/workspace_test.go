package workspace_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jiaweisky/harbor/internal/workspace"
)

func TestNoRootPassesThrough(t *testing.T) {
	r := workspace.NewResolver("", false)
	if got, _ := r.Resolve("ctx", "/tmp/x"); got != "/tmp/x" {
		t.Fatalf("got %q", got)
	}
	if got, _ := r.Resolve("ctx", ""); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestResolveRelativeInsideRoot(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	r := workspace.NewResolver(root, false)

	got, err := r.Resolve("ctx", "repo")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want, _ := filepath.EvalSymlinks(repo)
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestResolveRejectsTraversal(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	outside := filepath.Join(parent, "outside")
	for _, d := range []string{root, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r := workspace.NewResolver(root, false)

	if _, err := r.Resolve("ctx", "../outside"); !errors.Is(err, workspace.ErrDenied) {
		t.Fatalf("expected ErrDenied, got %v", err)
	}
}

func TestResolveRejectsSymlinkEscape(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	outside := filepath.Join(parent, "outside")
	for _, d := range []string{root, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	r := workspace.NewResolver(root, false)

	if _, err := r.Resolve("ctx", "escape"); !errors.Is(err, workspace.ErrDenied) {
		t.Fatalf("expected ErrDenied, got %v", err)
	}
}

func TestResolveMissingDir(t *testing.T) {
	root := t.TempDir()
	r := workspace.NewResolver(root, false)
	if _, err := r.Resolve("ctx", "nope"); !errors.Is(err, workspace.ErrDenied) {
		t.Fatalf("expected ErrDenied, got %v", err)
	}
}

func TestScratchDirPerContext(t *testing.T) {
	root := t.TempDir()
	r := workspace.NewResolver(root, false)

	a, err := r.Resolve("ctx-a", "")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(a)
	if err != nil || !info.IsDir() {
		t.Fatalf("scratch dir not created: %v", err)
	}
	if b, _ := r.Resolve("ctx-b", ""); b == a {
		t.Fatal("different contexts should get different scratch dirs")
	}
	if again, _ := r.Resolve("ctx-a", ""); again != a {
		t.Fatal("scratch dir should be stable per context")
	}
}

func TestResolveBindsToContext(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"repo", "other"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r := workspace.NewResolver(root, false)

	first, err := r.Resolve("ctx", "repo")
	if err != nil {
		t.Fatal(err)
	}
	// A later request for the same context reuses the first directory.
	again, err := r.Resolve("ctx", "other")
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Fatalf("context binding not reused: %q != %q", again, first)
	}
}
