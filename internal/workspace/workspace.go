// Package workspace resolves the working directory of a task and, optionally,
// provides a per-context working copy for isolation.
package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Isolation strategies.
const (
	IsolationShared   = "shared"
	IsolationWorktree = "worktree"
	IsolationCopy     = "copy"
)

// Resolver returns the working directory for a task from the agent's configured
// project directory. Under worktree/copy isolation each context gets its own
// copy of that directory.
type Resolver struct {
	// workdir is the configured project directory (the directory the CLI is
	// started in). Empty means the CLI inherits Harbor's working directory.
	workdir   string
	isolation string
	isoRoot   string

	mu    sync.Mutex
	bound map[string]string // contextID -> resolved directory
	locks *lockSet
}

// NewResolver creates a shared-isolation resolver for workdir.
func NewResolver(workdir string) *Resolver {
	return NewResolverWithIsolation(workdir, IsolationShared, "")
}

// NewResolverWithIsolation creates a resolver with an isolation strategy.
// worktree/copy need isolationRoot, where per-context copies are created.
func NewResolverWithIsolation(workdir, isolation, isolationRoot string) *Resolver {
	if isolation == "" {
		isolation = IsolationShared
	}
	return &Resolver{
		workdir:   workdir,
		isolation: isolation,
		isoRoot:   isolationRoot,
		bound:     map[string]string{},
		locks:     newLockSet(),
	}
}

// Resolve returns the working directory for a context: the configured workdir, or
// (under worktree/copy isolation) a per-context copy of it. The result is
// resolved once and reused, so a resumed task keeps the same tree.
func (r *Resolver) Resolve(contextID string) (string, error) {
	if r == nil || r.workdir == "" {
		return "", nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if contextID != "" {
		if dir, ok := r.bound[contextID]; ok {
			return dir, nil
		}
	}
	dir := r.workdir
	if r.isolation == IsolationWorktree || r.isolation == IsolationCopy {
		d, err := r.isolate(contextID)
		if err != nil {
			return "", err
		}
		dir = d
	}
	if contextID != "" {
		r.bound[contextID] = dir
	}
	return dir, nil
}

// isolate creates a per-context working copy of the configured workdir.
func (r *Resolver) isolate(contextID string) (string, error) {
	if r.isoRoot == "" {
		return "", fmt.Errorf("isolation_root is required for %q", r.isolation)
	}
	dst := filepath.Join(r.isoRoot, contextName(contextID))
	if _, err := os.Stat(dst); err == nil {
		return dst, nil // already provisioned for this context
	}
	if err := os.MkdirAll(r.isoRoot, 0o755); err != nil {
		return "", err
	}
	switch r.isolation {
	case IsolationWorktree:
		if err := gitWorktree(r.workdir, dst); err != nil {
			return "", fmt.Errorf("worktree: %w", err)
		}
	case IsolationCopy:
		if err := copyTree(r.workdir, dst); err != nil {
			return "", fmt.Errorf("copy: %w", err)
		}
	default:
		return "", fmt.Errorf("unknown isolation %q", r.isolation)
	}
	return dst, nil
}

// Lock acquires the lock for dir and returns its release function. It is a
// no-op when dir is empty. Callers hold the lock for the task's lifetime.
func (r *Resolver) Lock(dir string) func() {
	if r == nil || r.locks == nil || dir == "" {
		return func() {}
	}
	return r.locks.lock(dir)
}

func gitWorktree(repo, dst string) error {
	cmd := exec.Command("git", "-C", repo, "worktree", "add", "--detach", dst, "HEAD")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree add: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

// contextName derives a stable, filesystem-safe directory name from a context.
func contextName(contextID string) string {
	if contextID == "" {
		return "ctx-default"
	}
	sum := sha256.Sum256([]byte(contextID))
	return "ctx-" + hex.EncodeToString(sum[:8])
}

// lockSet provides reference-counted per-key mutexes.
type lockSet struct {
	mu      sync.Mutex
	entries map[string]*lockEntry
}

type lockEntry struct {
	mu   sync.Mutex
	refs int
}

func newLockSet() *lockSet { return &lockSet{entries: map[string]*lockEntry{}} }

func (l *lockSet) lock(key string) func() {
	l.mu.Lock()
	e := l.entries[key]
	if e == nil {
		e = &lockEntry{}
		l.entries[key] = e
	}
	e.refs++
	l.mu.Unlock()

	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		l.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(l.entries, key)
		}
		l.mu.Unlock()
	}
}
