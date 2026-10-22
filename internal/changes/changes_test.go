package changes_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jiaweisky/harbor/internal/changes"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestCaptureGit(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "init")

	base := changes.Snapshot(dir)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("old\nnew\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := changes.Capture(dir, base, 0)
	if !res.Unified {
		t.Fatal("git capture should be a unified diff")
	}
	if !strings.Contains(res.Diff, "+new") {
		t.Fatalf("diff missing modification:\n%s", res.Diff)
	}
	if !strings.Contains(res.Diff, "b.txt") {
		t.Fatalf("diff missing untracked file:\n%s", res.Diff)
	}
	if !contains(res.Files, "a.txt") || !contains(res.Files, "b.txt") {
		t.Fatalf("files = %v", res.Files)
	}
}

func TestCaptureNonGit(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := changes.Snapshot(dir, "*.txt")

	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := changes.Capture(dir, base, 0)
	if res.Unified {
		t.Fatal("non-git capture should be a summary")
	}
	if !strings.Contains(res.Diff, "# modified:") || !strings.Contains(res.Diff, "a.txt") {
		t.Fatalf("summary missing modification:\n%s", res.Diff)
	}
	if !strings.Contains(res.Diff, "# added:") || !strings.Contains(res.Diff, "b.txt") {
		t.Fatalf("summary missing addition:\n%s", res.Diff)
	}
}

func TestCaptureGitRename(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init")
	if err := os.WriteFile(filepath.Join(dir, "old name.txt"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "init")

	base := changes.Snapshot(dir)
	git(t, dir, "mv", "old name.txt", "new name.txt")
	res := changes.Capture(dir, base, 0)
	if !contains(res.Files, "new name.txt") {
		t.Fatalf("files = %v, want the rename target", res.Files)
	}
}

func TestCaptureGitUntrackedWithNewline(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init")
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("tracked"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "init")

	base := changes.Snapshot(dir)
	name := "weird\nname.txt"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := changes.Capture(dir, base, 0)
	if !contains(res.Files, name) {
		t.Fatalf("files = %v, want %q", res.Files, name)
	}
	if !strings.Contains(res.Diff, strconv.Quote(name)) {
		t.Fatalf("diff missing quoted untracked path:\n%s", res.Diff)
	}
}

func TestCaptureIgnoresHarborDirs(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "init")

	base := changes.Snapshot(dir)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("old\nnew\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	attach := filepath.Join(dir, ".a2a", "attachments", "t1")
	if err := os.MkdirAll(attach, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(attach, "x.txt"), []byte("note"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := changes.Capture(dir, base, 0)
	if !strings.Contains(res.Diff, "+new") {
		t.Fatalf("diff missing the real change:\n%s", res.Diff)
	}
	if strings.Contains(res.Diff, ".a2a") {
		t.Fatalf("diff leaked Harbor's attachments:\n%s", res.Diff)
	}
	for _, f := range res.Files {
		if strings.HasPrefix(f, ".a2a") {
			t.Fatalf("files leaked Harbor's attachments: %v", res.Files)
		}
	}
}

func TestCaptureTruncatesDiff(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := changes.Snapshot(dir, "*.txt")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := changes.Capture(dir, base, 5)
	if !res.Truncated || !strings.HasSuffix(res.Diff, "... (truncated)") {
		t.Fatalf("truncated = %v, diff = %q", res.Truncated, res.Diff)
	}
}

func TestCaptureNonGitIgnoresHarborDirs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	uploads := filepath.Join(dir, ".harbor", "uploads")
	if err := os.MkdirAll(uploads, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(uploads, "1-x.txt"), []byte("note"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := changes.Snapshot(dir, "**")

	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(uploads, "2-y.txt"), []byte("note2"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := changes.Capture(dir, base, 0)
	if !strings.Contains(res.Diff, "a.txt") {
		t.Fatalf("summary missing the real change:\n%s", res.Diff)
	}
	if strings.Contains(res.Diff, ".harbor") {
		t.Fatalf("summary leaked Harbor's attachments:\n%s", res.Diff)
	}
}

func TestCaptureNonGitDisabledByDefault(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := changes.Snapshot(dir)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := changes.Capture(dir, base, 0)
	if res.Diff != "" || len(res.Files) != 0 {
		t.Fatalf("non-git capture without globs = %+v, want empty", res)
	}
}

func TestCaptureNonGitGlobsOnlyMatching(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "a.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "b.md"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := changes.Snapshot(dir, "src/**")
	if err := os.WriteFile(filepath.Join(dir, "src", "a.txt"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "b.md"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "c.txt"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := changes.Capture(dir, base, 0)
	if !contains(res.Files, "src/a.txt") || !contains(res.Files, "src/c.txt") {
		t.Fatalf("files = %v, want only src matches", res.Files)
	}
	if contains(res.Files, "docs/b.md") {
		t.Fatalf("files = %v, want docs/b.md excluded", res.Files)
	}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
