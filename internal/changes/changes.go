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
	"strconv"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// Baseline records the working-tree state before a task runs.
type Baseline struct {
	git      bool
	disabled bool // non-git capture is off
	head     string
	globs    []string          // non-git include patterns
	files    map[string]string // relative path -> content hash (non-git)
}

// Result is the captured change set.
type Result struct {
	Diff      string   // unified diff (git) or a summary (non-git)
	Files     []string // changed paths
	Truncated bool
	Unified   bool // Diff is a unified diff; false means a summary
}

// defaultDiffMaxBytes caps the diff size when the caller passes 0.
const defaultDiffMaxBytes = 256 << 10

// Snapshot records the baseline of dir. A non-git directory is captured only
// when nonGitGlobs is non-empty; otherwise change capture is disabled.
func Snapshot(dir string, nonGitGlobs ...string) Baseline {
	if insideGit(dir) {
		return Baseline{git: true, head: gitOutput(dir, "rev-parse", "HEAD")}
	}
	globs := normalizeGlobs(nonGitGlobs)
	if len(globs) == 0 {
		return Baseline{disabled: true}
	}
	return Baseline{globs: globs, files: hashMatchingFiles(dir, globs)}
}

// Capture returns the changes in dir relative to base, truncating the diff to
// maxBytes (0 uses the default).
func Capture(dir string, base Baseline, maxBytes int64) Result {
	if base.disabled {
		return Result{}
	}
	if maxBytes <= 0 {
		maxBytes = defaultDiffMaxBytes
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

func captureGit(dir string, base Baseline, maxBytes int64) Result {
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

	statusArgs := append([]string{"status", "--porcelain", "-z", "--", "."}, excludePathspecs()...)
	status := gitOutput(dir, statusArgs...)
	b.WriteString(untrackedSection(dir))

	diff := b.String()
	truncated := truncate(&diff, maxBytes)
	return Result{
		Diff: strings.TrimSpace(diff), Files: statusPaths(status),
		Truncated: truncated, Unified: true,
	}
}

func captureHashes(dir string, base Baseline, maxBytes int64) Result {
	now := hashMatchingFiles(dir, base.globs)

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
	args := append([]string{"ls-files", "--others", "--exclude-standard", "-z", "--", "."}, excludePathspecs()...)
	entries := strings.Split(gitOutput(dir, args...), "\x00")
	untracked := make([]string, 0, len(entries))
	for _, p := range entries {
		if p != "" {
			untracked = append(untracked, displayPath(p))
		}
	}
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
		b.WriteString(displayPath(p) + "\n")
	}
}

// statusPaths extracts the paths from `git status --porcelain -z` output.
// Rename and copy entries carry the new path first and the old path as the
// next NUL-separated field, so the old path is skipped.
func statusPaths(out string) []string {
	entries := strings.Split(out, "\x00")
	paths := make([]string, 0, len(entries))
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		if len(e) <= 3 {
			continue
		}
		paths = append(paths, e[3:])
		if strings.ContainsAny(e[:2], "RC") {
			i++
		}
	}
	return paths
}

// displayPath quotes a path when it contains line breaks, so one path does not
// span multiple lines in the textual change summary.
func displayPath(p string) string {
	if strings.ContainsAny(p, "\r\n") {
		return strconv.Quote(p)
	}
	return p
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

func truncate(s *string, max int64) bool {
	if int64(len(*s)) <= max {
		return false
	}
	*s = (*s)[:int(max)] + "\n... (truncated)"
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

// normalizeGlobs cleans non-git include patterns and drops empty ones.
func normalizeGlobs(globs []string) []string {
	out := make([]string, 0, len(globs))
	for _, g := range globs {
		g = filepath.ToSlash(strings.TrimSpace(g))
		g = strings.TrimPrefix(g, "./")
		if g == "" || g == "." || !doublestar.ValidatePattern(g) {
			continue
		}
		out = append(out, g)
	}
	return out
}

// hashMatchingFiles hashes the regular files under dir that match one of the
// non-git include patterns, keyed by slash-separated relative path.
func hashMatchingFiles(dir string, globs []string) map[string]string {
	files := map[string]string{}
	fsys := os.DirFS(dir)
	for _, pattern := range globs {
		matches, err := doublestar.Glob(fsys, pattern, doublestar.WithFilesOnly(), doublestar.WithNoFollow())
		if err != nil {
			continue
		}
		for _, rel := range matches {
			if _, seen := files[rel]; seen || hasIgnoredDir(rel) {
				continue
			}
			if sum, ok := hashFile(filepath.Join(dir, filepath.FromSlash(rel))); ok {
				files[rel] = sum
			}
		}
	}
	return files
}

// hasIgnoredDir reports whether any path segment is a Harbor-owned or VCS
// directory.
func hasIgnoredDir(rel string) bool {
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == ".git" || isIgnoredDir(part) {
			return true
		}
	}
	return false
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
