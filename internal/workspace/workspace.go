// Package workspace resolves the working directory of a task and, optionally,
// provides a per-context working view for isolation.
package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/jiaweisky/harbor/internal/core"
)

// Isolation is an alias for the runtime isolation kind.
type Isolation = core.Isolation

const (
	IsolationShared    = core.IsolationShared
	IsolationWorktree  = core.IsolationWorktree
	IsolationCopy      = core.IsolationCopy
	IsolationOverlayFS = core.IsolationOverlayFS
)

// CopyStrategy is an alias for the runtime copy strategy kind.
type CopyStrategy = core.CopyStrategy

const (
	CopyFull = core.CopyFull
	CopyCow  = core.CopyCow
)

// isolationStrategy describes how one isolation mode provisions and releases a
// context directory.
type isolationStrategy struct {
	// ensure makes dst ready for src. It must be idempotent so a resumed context
	// reuses an already provisioned directory or remounts an existing view.
	ensure func(src, dst string) error
	// view maps dst to the directory the CLI runs in. nil means dst itself.
	view func(dst string) string
	// release frees resources held by dst (currently only overlayfs unmounts).
	release func(dst string) error
	// needsSource reports whether the original workdir must stay visible when
	// the CLI runs in a container.
	needsSource bool
}

// CheckIsolation validates the isolation mode and probes runtime availability
// for strategies that depend on the platform or filesystem. It runs at startup
// so a deployment problem fails before the first task.
func CheckIsolation(isolation Isolation, strategy CopyStrategy, isolationRoot string) error {
	switch isolation {
	case "", IsolationShared, IsolationWorktree:
		if strategy != "" {
			return fmt.Errorf("sandbox.copy_strategy is only valid with isolation copy")
		}
		return nil
	case IsolationCopy:
		if strategy == "" {
			strategy = CopyFull
		}

		if !strategy.Known() {
			return fmt.Errorf("sandbox.copy_strategy must be one of full, cow; got %q", strategy)
		}

		if strategy == CopyCow {
			return probeClone(isolationRoot)
		}
		return nil
	case IsolationOverlayFS:
		return checkOverlayFS(isolationRoot)
	default:
		return fmt.Errorf(
			"sandbox.isolation must be one of shared, worktree, copy, overlayfs; got %q", isolation,
		)
	}
}

// CheckCopySource verifies that cow copy can clone from workdir into
// isolationRoot. Clone syscalls require both paths on the same filesystem, so
// this fails at startup rather than on the first task.
func CheckCopySource(workdir, isolationRoot string) error {
	if workdir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}

		workdir = wd
	}

	if isolationRoot == "" {
		return fmt.Errorf("sandbox.isolation_root is required for copy strategy %q", CopyCow)
	}

	same, err := sameFilesystem(workdir, isolationRoot)
	if err != nil {
		return err
	}

	if !same {
		return fmt.Errorf(
			"copy strategy %q requires agents[].workdir and sandbox.isolation_root on the same filesystem",
			CopyCow,
		)
	}

	return nil
}

// LockIsolationRoot takes an exclusive cross-process lock on isolationRoot for
// modes that create per-context views. The lock is released by closing the
// returned closer. Callers must hold it while cleaning and serving.
func LockIsolationRoot(isolation Isolation, isolationRoot string) (io.Closer, error) {
	switch isolation {
	case IsolationWorktree, IsolationCopy, IsolationOverlayFS:
		if isolationRoot == "" {
			return nil, fmt.Errorf("sandbox.isolation_root is required for isolation %q", isolation)
		}
		return lockIsolationRoot(isolationRoot)
	default:
		return nil, nil
	}
}

// PrepareIsolation removes artifacts left by interrupted tasks. It runs once
// during startup, after CheckIsolation and after taking the isolation_root
// lock, and before serving any task.
func PrepareIsolation(isolation Isolation, isolationRoot string) error {
	switch isolation {
	case IsolationCopy:
		return cleanupCopyTemps(isolationRoot)
	case IsolationOverlayFS:
		return cleanupOverlayMounts(isolationRoot)
	default:
		return nil
	}
}

// selectStrategy resolves a configured mode to its implementation. ok is false
// for unknown modes.
func selectStrategy(isolation Isolation, copyStrategy CopyStrategy) (isolationStrategy, bool) {
	switch isolation {
	case IsolationShared:
		return isolationStrategy{needsSource: true}, true
	case IsolationWorktree:
		return isolationStrategy{ensure: ensureWorktree, needsSource: true}, true
	case IsolationCopy:
		if copyStrategy == "" {
			copyStrategy = CopyFull
		}

		cp, ok := copyStrategies[copyStrategy]
		if !ok {
			return isolationStrategy{}, false
		}

		return isolationStrategy{
			ensure: func(src, dst string) error {
				if err := ensureCopy(src, dst, cp); err != nil {
					return fmt.Errorf("copy_strategy %q: %w", copyStrategy, err)
				}
				return nil
			},
		}, true
	case IsolationOverlayFS:
		return isolationStrategy{
			ensure:      ensureOverlayFS,
			view:        mergedView,
			release:     releaseOverlayFS,
			needsSource: false,
		}, true
	default:
		return isolationStrategy{}, false
	}
}

// Resolver resolves the working directory for a task from the agent's configured
// project directory. Under worktree/copy/overlayfs isolation each context gets
// its own view of that directory.
type Resolver struct {
	// scope distinguishes one agent's resolver from another. It is part of the
	// context directory key so two agents cannot share a view.
	scope string
	// workdir is the configured project directory (the directory the CLI is
	// started in). Empty means the CLI inherits Harbor's working directory.
	workdir      string
	isolation    Isolation
	isoRoot      string
	copyStrategy CopyStrategy
	ensure       func(src, dst string) error
	view         func(dst string) string
	release      func(dst string) error
	needsSource  bool

	mu     sync.Mutex
	bound  map[string]string // contextID -> resolved directory
	active map[string]bool   // directories with release work pending
}

// NewResolver creates a shared-isolation resolver for workdir.
func NewResolver(workdir string) *Resolver {
	return NewResolverWithIsolation(workdir, IsolationShared, "", "")
}

// NewResolverWithIsolation creates a resolver with an isolation strategy.
// worktree/copy/overlayfs need isolationRoot, where per-context views are
// created; copyStrategy selects the copy implementation (empty means full).
func NewResolverWithIsolation(
	workdir string, isolation Isolation, isolationRoot string, copyStrategy CopyStrategy,
) *Resolver {
	return NewResolverWithScope("", workdir, isolation, isolationRoot, copyStrategy)
}

// NewResolverWithScope creates a resolver whose context directories are scoped
// to scope (normally the agent ID). Two resolvers with different scopes never
// share a per-context view.
func NewResolverWithScope(
	scope, workdir string, isolation Isolation, isolationRoot string, copyStrategy CopyStrategy,
) *Resolver {
	if isolation == "" {
		isolation = IsolationShared
	}

	if isolation == IsolationCopy && copyStrategy == "" {
		copyStrategy = CopyFull
	}

	if workdir != "" && isolation != IsolationShared {
		if resolved, err := filepath.EvalSymlinks(workdir); err == nil {
			workdir = resolved
		}
	}

	st, ok := selectStrategy(isolation, copyStrategy)
	if !ok {
		// Config validation rejects unknown modes; keep the source mounted and
		// fail on Resolve for direct callers.
		st.needsSource = true
	}

	return &Resolver{
		scope:        scope,
		workdir:      workdir,
		isolation:    isolation,
		isoRoot:      isolationRoot,
		copyStrategy: copyStrategy,
		ensure:       st.ensure,
		view:         st.view,
		release:      st.release,
		needsSource:  st.needsSource,
		bound:        map[string]string{},
		active:       map[string]bool{},
	}
}

// NeedsSourceMount reports whether the original workdir must be visible inside a
// container. It is true unless the selected strategy is self-contained.
func (r *Resolver) NeedsSourceMount() bool {
	return r == nil || r.needsSource
}

// Close releases every provisioned view that needs teardown, such as an
// overlayfs mount. Persistent per-context data (copy trees and overlay uppers)
// is kept so a resumed context can reuse it after a restart.
func (r *Resolver) Close() error {
	if r == nil || r.release == nil {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	var errs []error
	for dir := range r.active {
		if err := r.release(dir); err != nil {
			errs = append(errs, fmt.Errorf("release %s: %w", dir, err))
		}

		delete(r.active, dir)
	}

	return errors.Join(errs...)
}

// Resolve returns the working directory for a context: the configured workdir, or
// (under worktree/copy/overlayfs isolation) a per-context view of it. The result
// is resolved once and reused, so a resumed task keeps the same tree.
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

// isolate provisions the per-context directory of the configured workdir.
func (r *Resolver) isolate(contextID string) (string, error) {
	if r.ensure == nil {
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

	if r.isolation == IsolationCopy || r.isolation == IsolationOverlayFS {
		if containsPath(r.isoRoot, r.workdir) || containsPath(r.workdir, r.isoRoot) {
			return "", fmt.Errorf(
				"isolation_root %s and workdir %s must not contain each other", r.isoRoot, r.workdir,
			)
		}
	}

	if err := os.MkdirAll(r.isoRoot, 0o755); err != nil {
		return "", err
	}

	root := filepath.Join(r.isoRoot, r.contextName(contextID))
	if err := r.ensure(r.workdir, root); err != nil {
		return "", fmt.Errorf("%s: %w", r.isolation, err)
	}

	if r.release != nil {
		r.active[root] = true
	}

	if r.view != nil {
		return r.view(root), nil
	}

	return root, nil
}

// contextName derives a stable, filesystem-safe directory name. The agent scope,
// workdir, and isolation mode are part of the key so another agent or a
// configuration change cannot reuse a tree with an incompatible layout.
func (r *Resolver) contextName(contextID string) string {
	key := r.scope + "\x00" + string(r.isolation) + "\x00" + string(r.copyStrategy) + "\x00" +
		r.workdir + "\x00" + contextID
	sum := sha256.Sum256([]byte(key))

	return "ctx-" + hex.EncodeToString(sum[:])
}

// Lock acquires the lock for dir and returns its release function. It is a
// no-op when dir is empty. Callers hold the lock for the task's lifetime.
func (r *Resolver) Lock(dir string) func() {
	if r == nil || dir == "" {
		return func() {}
	}
	return directoryLocks.lock(dir)
}

func containsPath(root, target string) bool {
	root = canonicalPath(root)
	target = canonicalPath(target)

	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}

	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func canonicalPath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}

	rest := []string(nil)
	for {
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			return filepath.Join(append([]string{resolved}, rest...)...)
		}
		parent := filepath.Dir(path)
		if parent == path {
			return path
		}
		rest = append([]string{filepath.Base(path)}, rest...)
		path = parent
	}
}
