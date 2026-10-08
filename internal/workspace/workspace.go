// Package workspace resolves and confines the working directory of a task.
package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// ErrDenied indicates a working directory failed validation (outside the root,
// missing, or otherwise unusable).
var ErrDenied = errors.New("workdir denied")

// Isolation strategies.
const (
	IsolationShared   = "shared"
	IsolationWorktree = "worktree"
	IsolationCopy     = "copy"
)

// Resolver maps a requested working directory to a confined absolute path. A
// resolver with an empty root performs no confinement.
type Resolver struct {
	root         string
	allowOutside bool
	isolation    string
	isoRoot      string
	// defaultDir is used when a request does not name a working directory, so an
	// agent can be pinned to a project (whose CLI config it then loads).
	defaultDir string

	mu    sync.Mutex
	bound map[string]string // contextID -> resolved directory
	locks *lockSet
}

// NewResolver creates a resolver rooted at dir using shared isolation. When
// allowOutside is true the root confinement is not enforced. An empty root
// disables resolution.
func NewResolver(root string, allowOutside bool) *Resolver {
	return NewResolverWithIsolation(root, allowOutside, IsolationShared, "", "")
}

// NewResolverWithIsolation creates a resolver with an isolation strategy.
// worktree/copy need isolationRoot (where per-context copies are created).
// defaultDir, when set, is used for requests that name no working directory.
func NewResolverWithIsolation(root string, allowOutside bool, isolation, isolationRoot, defaultDir string) *Resolver {
	if isolation == "" {
		isolation = IsolationShared
	}
	return &Resolver{
		root:         root,
		allowOutside: allowOutside,
		isolation:    isolation,
		isoRoot:      isolationRoot,
		defaultDir:   defaultDir,
		bound:        map[string]string{},
		locks:        newLockSet(),
	}
}

// Resolve returns the absolute working directory for a context, resolved once
// and reused so a resumed task keeps the same tree. Under worktree/copy
// isolation each context gets its own directory. An empty requested path yields
// a per-context scratch directory under the root.
func (r *Resolver) Resolve(contextID, requested string) (string, error) {
	if r == nil || r.root == "" {
		return requested, nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if contextID != "" {
		if dir, ok := r.bound[contextID]; ok {
			return dir, nil
		}
	}

	dir, err := r.resolveUnlocked(contextID, requested)
	if err != nil {
		return "", err
	}
	if contextID != "" {
		r.bound[contextID] = dir
	}
	return dir, nil
}

func (r *Resolver) resolveUnlocked(contextID, requested string) (string, error) {
	root, err := filepath.EvalSymlinks(r.root)
	if err != nil {
		return "", fmt.Errorf("%w: workdir_root: %v", ErrDenied, err)
	}

	if requested == "" {
		requested = r.defaultDir
	}
	if requested == "" {
		dir := filepath.Join(root, scratchName(contextID))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("%w: scratch dir: %v", ErrDenied, err)
		}
		return dir, nil
	}

	source := requested
	if !filepath.IsAbs(source) {
		source = filepath.Join(root, source)
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return "", fmt.Errorf("%w: %q: %v", ErrDenied, requested, err)
	}
	if !r.allowOutside && !within(root, source) {
		return "", fmt.Errorf("%w: %q is outside workdir_root %q", ErrDenied, requested, root)
	}

	if r.isolation == IsolationShared || r.isolation == "" {
		return source, nil
	}
	return r.isolate(contextID, source)
}

// isolate creates a per-context working copy of source.
func (r *Resolver) isolate(contextID, source string) (string, error) {
	if r.isoRoot == "" {
		return "", fmt.Errorf("%w: isolation_root is required for %q", ErrDenied, r.isolation)
	}
	dst := filepath.Join(r.isoRoot, scratchName(contextID))
	if _, err := os.Stat(dst); err == nil {
		return dst, nil // already provisioned for this context
	}
	if err := os.MkdirAll(r.isoRoot, 0o755); err != nil {
		return "", fmt.Errorf("%w: %v", ErrDenied, err)
	}

	switch r.isolation {
	case IsolationWorktree:
		if err := gitWorktree(source, dst); err != nil {
			return "", fmt.Errorf("%w: worktree: %v", ErrDenied, err)
		}
	case IsolationCopy:
		if err := copyTree(source, dst); err != nil {
			return "", fmt.Errorf("%w: copy: %v", ErrDenied, err)
		}
	default:
		return "", fmt.Errorf("%w: unknown isolation %q", ErrDenied, r.isolation)
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

// within reports whether path is root or a descendant of root.
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

// scratchName derives a stable, filesystem-safe directory name from a context.
func scratchName(contextID string) string {
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
