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

	"github.com/jiaweisky/harbor/internal/core"
)

// Isolation is an alias for the runtime isolation kind.
type Isolation = core.Isolation

const (
	IsolationShared   = core.IsolationShared
	IsolationWorktree = core.IsolationWorktree
	IsolationCopy     = core.IsolationCopy
)

// CopyStrategy is an alias for the runtime copy strategy kind.
type CopyStrategy = core.CopyStrategy

const (
	CopyFull     = core.CopyFull
	CopyReflink  = core.CopyReflink
	CopyOverlay  = core.CopyOverlay
	CopyHardlink = core.CopyHardlink
)

// isolationStrategy describes how one isolation mode provisions a context
// directory.
type isolationStrategy struct {
	// provision creates dst from src. nil marks a recognized but unimplemented
	// strategy; configuration rejects it.
	provision func(src, dst string) error
	// needsSource reports whether the original workdir must stay visible when
	// the CLI runs in a container.
	needsSource bool
}

// copyStrategies lists the copy implementations. Only full is implemented; the
// other entries are recognized names pending an implementation. Whether an
// implementation can run (OS, filesystem, privileges) is a runtime property and
// must be probed when it is added, not inferred here.
var copyStrategies = map[CopyStrategy]isolationStrategy{
	CopyFull:     {provision: copyTree, needsSource: false},
	CopyReflink:  {needsSource: false},
	CopyOverlay:  {needsSource: false},
	CopyHardlink: {needsSource: true},
}

// CopyStrategyKnown reports whether s is a recognized copy strategy.
func CopyStrategyKnown(s CopyStrategy) bool { return s.Known() }

// CopyStrategyImplemented reports whether this build has an implementation. It
// does not promise the strategy is usable on the current platform or filesystem;
// that needs a runtime probe.
func CopyStrategyImplemented(s CopyStrategy) bool {
	st, ok := copyStrategies[s]
	return ok && st.provision != nil
}

// CheckCopyStrategy rejects a recognized but unimplemented copy strategy.
func CheckCopyStrategy(isolation Isolation, strategy CopyStrategy) error {
	if isolation != IsolationCopy {
		return nil
	}
	if strategy == "" {
		strategy = CopyFull
	}
	if !CopyStrategyImplemented(strategy) {
		return fmt.Errorf("sandbox.copy_strategy %q is not implemented", strategy)
	}
	return nil
}

// selectStrategy resolves a configured mode to its implementation. ok is false
// for unknown or unimplemented modes.
func selectStrategy(isolation Isolation, copyStrategy CopyStrategy) (isolationStrategy, bool) {
	switch isolation {
	case IsolationShared:
		return isolationStrategy{needsSource: true}, true
	case IsolationWorktree:
		return isolationStrategy{provision: gitWorktree, needsSource: true}, true
	case IsolationCopy:
		if copyStrategy == "" {
			copyStrategy = CopyFull
		}
		st, ok := copyStrategies[copyStrategy]
		return st, ok && st.provision != nil
	default:
		return isolationStrategy{}, false
	}
}

// Resolver returns the working directory for a task from the agent's configured
// project directory. Under worktree/copy isolation each context gets its own
// copy of that directory.
type Resolver struct {
	// workdir is the configured project directory (the directory the CLI is
	// started in). Empty means the CLI inherits Harbor's working directory.
	workdir      string
	isolation    Isolation
	isoRoot      string
	copyStrategy CopyStrategy
	provision    func(src, dst string) error
	needsSource  bool

	mu    sync.Mutex
	bound map[string]string // contextID -> resolved directory
	locks *lockSet
}

// NewResolver creates a shared-isolation resolver for workdir.
func NewResolver(workdir string) *Resolver {
	return NewResolverWithIsolation(workdir, IsolationShared, "", "")
}

// NewResolverWithIsolation creates a resolver with an isolation strategy.
// worktree/copy need isolationRoot, where per-context copies are created;
// copyStrategy selects the copy implementation (empty means full).
func NewResolverWithIsolation(
	workdir string, isolation Isolation, isolationRoot string, copyStrategy CopyStrategy,
) *Resolver {
	if isolation == "" {
		isolation = IsolationShared
	}
	st, ok := selectStrategy(isolation, copyStrategy)
	if !ok {
		// Config validation rejects unknown or unimplemented modes; keep the
		// source mounted and fail on Resolve for direct callers.
		st.needsSource = true
	}
	return &Resolver{
		workdir:      workdir,
		isolation:    isolation,
		isoRoot:      isolationRoot,
		copyStrategy: copyStrategy,
		provision:    st.provision,
		needsSource:  st.needsSource,
		bound:        map[string]string{},
		locks:        newLockSet(),
	}
}

// NeedsSourceMount reports whether the original workdir must be visible inside a
// container. It is true unless the selected strategy is self-contained.
func (r *Resolver) NeedsSourceMount() bool {
	return r == nil || r.needsSource
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
	if r.isolation != IsolationShared {
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
	if r.provision == nil {
		if r.isolation == IsolationCopy {
			return "", fmt.Errorf(
				"isolation %q with copy strategy %q is not implemented", r.isolation, r.copyStrategy,
			)
		}
		return "", fmt.Errorf("isolation %q is not implemented", r.isolation)
	}
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
	if err := r.provision(r.workdir, dst); err != nil {
		return "", fmt.Errorf("%s: %w", r.isolation, err)
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
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case d.Type()&os.ModeSymlink != 0:
			return copySymlink(path, target)
		case d.Type().IsRegular():
			return copyFile(path, target)
		default:
			return nil // skip sockets, fifos, and device nodes
		}
	})
}

// copySymlink recreates a symbolic link with the same target, so copy isolation
// does not follow the link and duplicate (or read) whatever it points to.
func copySymlink(src, dst string) error {
	link, err := os.Readlink(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.Symlink(link, dst)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, info.Mode().Perm())
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
