// Package changes captures the working-tree changes a task produced, so they
// can be delivered as an A2A artifact.
package changes

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Baseline records the working-tree state before a task runs.
type Baseline struct {
	git   bool
	head  string
	files map[string]string // relative path -> content hash (non-git)
}

// Result is the captured change set.
type Result struct {
	Diff      string   // unified diff (git) or a summary (non-git)
	Files     []string // changed paths
	Truncated bool
}

// defaultMaxBytes caps the diff size.
const defaultMaxBytes = 256 << 10

// Snapshot records the baseline of dir.
func Snapshot(dir string) Baseline {
	if insideGit(dir) {
		return Baseline{git: true, head: gitOutput(dir, "rev-parse", "HEAD")}
	}
	return Baseline{files: hashFiles(dir)}
}

// Capture returns the changes in dir relative to base, truncating the diff to
// maxBytes (0 uses the default).
func Capture(dir string, base Baseline, maxBytes int) Result {
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}
	if base.git {
		return captureGit(dir, base, maxBytes)
	}
	return captureHashes(dir, base, maxBytes)
}

// ignoredDirs are directories Harbor writes into the workspace (inbound
// attachments); they are not the task's own changes and must be excluded.
var ignoredDirs = []string{".a2a", ".harbor"}

// excludePathspecs returns git exclude pathspecs for the ignored directories.
func excludePathspecs() []string {
	out := make([]string, 0, len(ignoredDirs))
	for _, d := range ignoredDirs {
		out = append(out, ":(exclude)"+d)
	}
	return out
}

// isIgnoredDir reports whether name is one of Harbor's own directories.
func isIgnoredDir(name string) bool {
	for _, d := range ignoredDirs {
		if d == name {
			return true
		}
	}
	return false
}

func captureGit(dir string, base Baseline, maxBytes int) Result {
	var b strings.Builder
	// The trailing "." pathspec scopes the diff to dir, so changes outside a
	// nested working directory are not attributed to this task; the exclude
	// pathspecs drop Harbor's own attachment directories.
	args := []string{"diff"}
	if base.head != "" {
		args = append(args, base.head)
	}
	args = append(args, "--", ".")
	args = append(args, excludePathspecs()...)
	b.WriteString(gitOutput(dir, args...))

	statusArgs := append([]string{"status", "--porcelain", "--", "."}, excludePathspecs()...)
	status := gitLines(dir, statusArgs...)
	b.WriteString(untrackedSection(dir))

	diff := b.String()
	truncated := truncate(&diff, maxBytes)
	return Result{Diff: strings.TrimSpace(diff), Files: statusPaths(status), Truncated: truncated}
}

func captureHashes(dir string, base Baseline, maxBytes int) Result {
	now := hashFiles(dir)

	var added, modified, removed []string
	for path, hash := range now {
		old, ok := base.files[path]
		switch {
		case !ok:
			added = append(added, path)
		case old != hash:
			modified = append(modified, path)
		}
	}
	for path := range base.files {
		if _, ok := now[path]; !ok {
			removed = append(removed, path)
		}
	}
	sort.Strings(added)
	sort.Strings(modified)
	sort.Strings(removed)

	var b strings.Builder
	writeSection(&b, "added", added)
	writeSection(&b, "modified", modified)
	writeSection(&b, "removed", removed)

	diff := strings.TrimSpace(b.String())
	truncated := truncate(&diff, maxBytes)
	files := union(added, modified, removed)
	return Result{Diff: diff, Files: files, Truncated: truncated}
}

func untrackedSection(dir string) string {
	args := append([]string{"ls-files", "--others", "--exclude-standard", "--", "."}, excludePathspecs()...)
	untracked := gitLines(dir, args...)
	if len(untracked) == 0 {
		return ""
	}
	return "\n# untracked files:\n" + strings.Join(untracked, "\n") + "\n"
}

func writeSection(b *strings.Builder, title string, paths []string) {
	if len(paths) == 0 {
		return
	}
	fmt.Fprintf(b, "# %s:\n", title)
	for _, p := range paths {
		b.WriteString(p + "\n")
	}
}

// statusPaths extracts the paths from `git status --porcelain` lines.
func statusPaths(lines []string) []string {
	paths := make([]string, 0, len(lines))
	for _, line := range lines {
		if len(line) > 3 {
			paths = append(paths, strings.TrimSpace(line[3:]))
		}
	}
	return paths
}

func union(sets ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range sets {
		for _, v := range s {
			if !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
	}
	return out
}

func truncate(s *string, max int) bool {
	if len(*s) <= max {
		return false
	}
	*s = (*s)[:max] + "\n... (truncated)"
	return true
}

func insideGit(dir string) bool {
	return gitOutput(dir, "rev-parse", "--is-inside-work-tree") == "true"
}

func gitOutput(dir string, args ...string) string {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(out), "\n")
}

func gitLines(dir string, args ...string) []string {
	out := gitOutput(dir, args...)
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// hashFiles hashes every regular file under dir, keyed by relative path. The
// .git directory is skipped.
func hashFiles(dir string) map[string]string {
	files := map[string]string{}
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" || isIgnoredDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return nil
		}
		if sum, ok := hashFile(path); ok {
			files[rel] = sum
		}
		return nil
	})
	return files
}

func hashFile(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", false
	}
	return hex.EncodeToString(h.Sum(nil)), true
}
