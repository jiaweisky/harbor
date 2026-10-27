package workspace_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jiaweisky/harbor/internal/workspace"
)

func TestIsolationCopy(t *testing.T) {
	parent := t.TempDir()
	iso := filepath.Join(parent, "iso")
	src := filepath.Join(parent, "repo")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "run.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A symlink to a file outside the source tree must be recreated as a link,
	// not followed and copied.
	outside := filepath.Join(parent, "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}

	r := workspace.NewResolverWithIsolation(src, workspace.IsolationCopy, iso, "")
	a, err := r.Resolve("ctx-a")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if a == src {
		t.Fatal("copy isolation should not use the source directory")
	}
	copied, err := os.ReadFile(filepath.Join(a, "a.txt"))
	if err != nil || string(copied) != "base" {
		t.Fatalf("copied file: %q, %v", copied, err)
	}
	want, err := os.Stat(filepath.Join(src, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.Stat(filepath.Join(a, "run.sh"))
	if err != nil || got.Mode().Perm() != want.Mode().Perm() {
		t.Fatalf("copied mode = %v, %v; want %v", got, err, want.Mode().Perm())
	}
	li, err := os.Lstat(filepath.Join(a, "link"))
	if err != nil || li.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("copied symlink = %v, %v; want a symlink", li, err)
	}
}

func TestIsolationCopyCow(t *testing.T) {
	parent := t.TempDir()
	iso := filepath.Join(parent, "iso")
	src := filepath.Join(parent, "repo")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := workspace.NewResolverWithIsolation(src, workspace.IsolationCopy, iso, workspace.CopyCow)
	a, err := r.Resolve("ctx-a")
	if err != nil {
		t.Skipf("cow copy is not available: %v", err)
	}
	if a == src {
		t.Fatal("cow isolation should not use the source directory")
	}
	if err := os.WriteFile(filepath.Join(a, "a.txt"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(src, "a.txt"))
	if err != nil || string(got) != "base" {
		t.Fatalf("source file = %q, %v; want unchanged", got, err)
	}
}

func TestIsolationCopySymlinkedWorkdir(t *testing.T) {
	parent := t.TempDir()
	work := filepath.Join(parent, "repo")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(work, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	iso := filepath.Join(parent, "iso")
	r := workspace.NewResolverWithIsolation(link, workspace.IsolationCopy, iso, workspace.CopyFull)
	view, err := r.Resolve("ctx")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(view, "a.txt"))
	if err != nil || string(got) != "base" {
		t.Fatalf("copied file = %q, %v", got, err)
	}
}

func TestCopyStrategyState(t *testing.T) {
	for _, s := range []workspace.CopyStrategy{workspace.CopyFull, workspace.CopyCow} {
		if !s.Known() {
			t.Errorf("%q must be a known copy strategy", s)
		}
	}
	if workspace.CopyStrategy("bogus").Known() {
		t.Error("bogus must not be a known copy strategy")
	}
}

func TestUnknownCopyStrategyFailsResolve(t *testing.T) {
	r := workspace.NewResolverWithIsolation(t.TempDir(), workspace.IsolationCopy, t.TempDir(), "bogus")
	if !r.NeedsSourceMount() {
		t.Error("unknown copy strategy must keep the source mounted")
	}
	if _, err := r.Resolve("ctx"); err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("Resolve error = %v, want a not-implemented error", err)
	}
}

func TestCheckIsolationCopyCow(t *testing.T) {
	if err := workspace.CheckIsolation(
		workspace.IsolationCopy, workspace.CopyCow, t.TempDir(),
	); err != nil {
		t.Skipf("cow copy is not available: %v", err)
	}
}

func TestCheckCopySource(t *testing.T) {
	parent := t.TempDir()
	work := filepath.Join(parent, "work")
	iso := filepath.Join(parent, "iso")
	for _, dir := range []string{work, iso} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := workspace.CheckCopySource(work, iso); err != nil {
		t.Fatalf("CheckCopySource: %v", err)
	}
}

func TestPrepareIsolationCleansCopyTemps(t *testing.T) {
	root := t.TempDir()
	tmp := filepath.Join(root, ".tmp-ctx-old-123")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := workspace.PrepareIsolation(workspace.IsolationCopy, root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatal("stale copy staging directory was not removed")
	}
}
