package workspace_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
		"":                           true,
		workspace.IsolationShared:    true,
		workspace.IsolationWorktree:  true,
		workspace.IsolationCopy:      false,
		workspace.IsolationOverlayFS: false,
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

func TestCheckIsolation(t *testing.T) {
	if err := workspace.CheckIsolation(workspace.IsolationShared, "", ""); err != nil {
		t.Errorf("shared isolation: %v", err)
	}
	if err := workspace.CheckIsolation(workspace.IsolationCopy, workspace.CopyFull, ""); err != nil {
		t.Errorf("full copy: %v", err)
	}
	if err := workspace.CheckIsolation(workspace.IsolationCopy, "", ""); err != nil {
		t.Errorf("empty strategy defaults to full: %v", err)
	}
	if err := workspace.CheckIsolation(workspace.IsolationCopy, "bogus", ""); err == nil {
		t.Error("unknown strategy must be rejected")
	}
	if err := workspace.CheckIsolation(workspace.IsolationShared, workspace.CopyCow, ""); err == nil {
		t.Error("strategy without copy isolation must be rejected")
	}
	if err := workspace.CheckIsolation("bogus", "", ""); err == nil {
		t.Error("unknown isolation must be rejected")
	}
}

func TestResolverScopeSeparatesAgents(t *testing.T) {
	parent := t.TempDir()
	work := filepath.Join(parent, "work")
	iso := filepath.Join(parent, "iso")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	a := workspace.NewResolverWithScope("agent-a", work, workspace.IsolationCopy, iso, workspace.CopyFull)
	b := workspace.NewResolverWithScope("agent-b", work, workspace.IsolationCopy, iso, workspace.CopyFull)
	dirA, err := a.Resolve("ctx")
	if err != nil {
		t.Fatal(err)
	}
	dirB, err := b.Resolve("ctx")
	if err != nil {
		t.Fatal(err)
	}
	if dirA == dirB {
		t.Fatalf("different agents shared a context directory: %q", dirA)
	}
}

func TestResolverRejectsContainedIsolationRoot(t *testing.T) {
	work := t.TempDir()
	r := workspace.NewResolverWithIsolation(
		work, workspace.IsolationCopy, filepath.Join(work, "iso"), workspace.CopyFull,
	)
	if _, err := r.Resolve("ctx"); err == nil || !strings.Contains(err.Error(), "must not contain") {
		t.Fatalf("Resolve error = %v, want a containment error", err)
	}
}
