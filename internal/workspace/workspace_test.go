package workspace_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	iso := filepath.Join(parent, "iso")
	repo := filepath.Join(parent, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "init")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "init")

	r := workspace.NewResolverWithIsolation(repo, workspace.IsolationWorktree, iso, "")
	a, err := r.Resolve("ctx-a")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	b, _ := r.Resolve("ctx-b")
	if a == b || a == repo {
		t.Fatalf("expected distinct worktrees, got %q and %q (source %q)", a, b, repo)
	}
	if _, err := os.Stat(filepath.Join(a, ".git")); err != nil {
		t.Fatalf("worktree missing .git: %v", err)
	}
}

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

func TestLockSerializes(t *testing.T) {
	r := workspace.NewResolver(t.TempDir())

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

func TestSharedReturnsWorkdir(t *testing.T) {
	dir := t.TempDir()
	r := workspace.NewResolver(dir)
	got, err := r.Resolve("ctx")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != dir {
		t.Fatalf("got %q, want %q", got, dir)
	}
}

func TestEmptyWorkdirPassesThrough(t *testing.T) {
	r := workspace.NewResolver("")
	if got, _ := r.Resolve("ctx"); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestResolveBindsToContext(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init")
	r := workspace.NewResolverWithIsolation(repo, workspace.IsolationCopy, t.TempDir(), "")

	first, err := r.Resolve("ctx")
	if err != nil {
		t.Fatal(err)
	}
	again, err := r.Resolve("ctx")
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Fatalf("context binding not reused: %q != %q", again, first)
	}
}

func TestIsolationNeedsSourceMount(t *testing.T) {
	cases := map[workspace.Isolation]bool{
		"":                          true,
		workspace.IsolationShared:   true,
		workspace.IsolationWorktree: true,
		workspace.IsolationCopy:     false,
	}
	for isolation, want := range cases {
		r := workspace.NewResolverWithIsolation(t.TempDir(), isolation, t.TempDir(), "")
		if got := r.NeedsSourceMount(); got != want {
			t.Errorf("%q: NeedsSourceMount() = %v, want %v", isolation, got, want)
		}
	}
	var nilResolver *workspace.Resolver
	if !nilResolver.NeedsSourceMount() {
		t.Error("nil resolver must require the source mount")
	}
}

func TestUnknownIsolationFailsResolve(t *testing.T) {
	r := workspace.NewResolverWithIsolation(t.TempDir(), "not-implemented", t.TempDir(), "")
	if !r.NeedsSourceMount() {
		t.Error("unknown isolation must keep the source mounted")
	}
	if _, err := r.Resolve("ctx"); err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("Resolve error = %v, want a not-implemented error", err)
	}
}

func TestCopyStrategyState(t *testing.T) {
	if !workspace.CopyStrategyKnown(workspace.CopyFull) || !workspace.CopyStrategyImplemented(workspace.CopyFull) {
		t.Error("full copy must be known and implemented")
	}
	for _, s := range []workspace.CopyStrategy{
		workspace.CopyReflink, workspace.CopyOverlay, workspace.CopyHardlink,
	} {
		if !workspace.CopyStrategyKnown(s) || workspace.CopyStrategyImplemented(s) {
			t.Errorf("%q: known = %v, implemented = %v; want known and unimplemented",
				s, workspace.CopyStrategyKnown(s), workspace.CopyStrategyImplemented(s))
		}
	}
	if workspace.CopyStrategyKnown("bogus") {
		t.Error("bogus must not be a known copy strategy")
	}
}

func TestUnimplementedCopyStrategyFailsResolve(t *testing.T) {
	for _, s := range []workspace.CopyStrategy{
		workspace.CopyReflink, workspace.CopyOverlay, workspace.CopyHardlink,
	} {
		r := workspace.NewResolverWithIsolation(t.TempDir(), workspace.IsolationCopy, t.TempDir(), s)
		if !r.NeedsSourceMount() {
			t.Errorf("%q: unimplemented copy must keep the source mounted", s)
		}
		if _, err := r.Resolve("ctx"); err == nil || !strings.Contains(err.Error(), "not implemented") {
			t.Errorf("%q: Resolve error = %v, want a not-implemented error", s, err)
		}
	}
}

func TestCheckCopyStrategy(t *testing.T) {
	if err := workspace.CheckCopyStrategy(workspace.IsolationShared, workspace.CopyReflink); err != nil {
		t.Errorf("non-copy isolation: %v", err)
	}
	if err := workspace.CheckCopyStrategy(workspace.IsolationCopy, workspace.CopyFull); err != nil {
		t.Errorf("full copy: %v", err)
	}
	if err := workspace.CheckCopyStrategy(workspace.IsolationCopy, ""); err != nil {
		t.Errorf("empty strategy defaults to full: %v", err)
	}
	if err := workspace.CheckCopyStrategy(workspace.IsolationCopy, workspace.CopyReflink); err == nil {
		t.Error("unimplemented strategy must be rejected")
	}
}
