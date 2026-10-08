package workspace_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jiaweisky/harbor/internal/workspace"
)

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestIsolationWorktree(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	iso := filepath.Join(parent, "iso")
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "init")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "init")

	r := workspace.NewResolverWithIsolation(root, false, workspace.IsolationWorktree, iso, "")
	a, err := r.Resolve("ctx-a", "repo")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	b, _ := r.Resolve("ctx-b", "repo")
	if a == b || a == repo {
		t.Fatalf("expected distinct worktrees, got %q and %q (source %q)", a, b, repo)
	}
	if _, err := os.Stat(filepath.Join(a, ".git")); err != nil {
		t.Fatalf("worktree missing .git: %v", err)
	}
}

func TestIsolationCopy(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	iso := filepath.Join(parent, "iso")
	src := filepath.Join(root, "repo")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := workspace.NewResolverWithIsolation(root, false, workspace.IsolationCopy, iso, "")
	a, err := r.Resolve("ctx-a", "repo")
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
}

func TestLockSerializes(t *testing.T) {
	r := workspace.NewResolver(t.TempDir(), false)

	unlock := r.Lock("/x")
	acquired := make(chan struct{})
	go func() {
		u := r.Lock("/x")
		close(acquired)
		u()
	}()

	select {
	case <-acquired:
		t.Fatal("second lock acquired while the first is held")
	case <-time.After(100 * time.Millisecond):
	}

	unlock()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("lock was not released")
	}
}

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

func TestDefaultWorkdir(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"myproj", "other"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r := workspace.NewResolverWithIsolation(root, false, workspace.IsolationShared, "", "myproj")

	got, err := r.Resolve("ctx-default", "")
	if err != nil {
		t.Fatalf("resolve default: %v", err)
	}
	want, _ := filepath.EvalSymlinks(filepath.Join(root, "myproj"))
	if got != want {
		t.Fatalf("default resolve = %q, want %q", got, want)
	}

	gotOther, err := r.Resolve("ctx-other", "other")
	if err != nil {
		t.Fatalf("resolve explicit: %v", err)
	}
	wantOther, _ := filepath.EvalSymlinks(filepath.Join(root, "other"))
	if gotOther != wantOther {
		t.Fatalf("explicit resolve = %q, want %q", gotOther, wantOther)
	}

	outside := workspace.NewResolverWithIsolation(root, false, workspace.IsolationShared, "", filepath.Dir(root))
	if _, err := outside.Resolve("ctx-out", ""); !errors.Is(err, workspace.ErrDenied) {
		t.Fatalf("expected denial for a default outside the root, got %v", err)
	}
}
