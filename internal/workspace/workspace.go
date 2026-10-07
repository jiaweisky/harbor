// Package workspace resolves and confines the working directory of a task.
package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ErrDenied indicates a working directory failed validation (outside the root,
// missing, or otherwise unusable).
var ErrDenied = errors.New("workdir denied")

// Resolver maps a requested working directory to a confined absolute path. A
// resolver with an empty root performs no confinement.
type Resolver struct {
	root         string
	allowOutside bool

	mu    sync.Mutex
	bound map[string]string // contextID -> resolved directory
}

// NewResolver creates a resolver rooted at dir. When allowOutside is true the
// root confinement is not enforced. An empty root disables resolution.
func NewResolver(root string, allowOutside bool) *Resolver {
	return &Resolver{root: root, allowOutside: allowOutside, bound: map[string]string{}}
}

// Resolve returns the absolute working directory for a context. A directory is
// resolved once per context and reused, so a resumed task keeps the same tree.
// An empty requested path yields a per-context scratch directory under the root.
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

	dir, err := r.resolveRooted(contextID, requested)
	if err != nil {
		return "", err
	}
	if contextID != "" {
		r.bound[contextID] = dir
	}
	return dir, nil
}

func (r *Resolver) resolveRooted(contextID, requested string) (string, error) {
	root, err := filepath.EvalSymlinks(r.root)
	if err != nil {
		return "", fmt.Errorf("%w: workdir_root: %v", ErrDenied, err)
	}

	if requested == "" {
		dir := filepath.Join(root, scratchName(contextID))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("%w: scratch dir: %v", ErrDenied, err)
		}
		return dir, nil
	}

	candidate := requested
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(root, candidate)
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", fmt.Errorf("%w: %q: %v", ErrDenied, requested, err)
	}
	if !r.allowOutside && !within(root, resolved) {
		return "", fmt.Errorf("%w: %q is outside workdir_root %q", ErrDenied, requested, root)
	}
	return resolved, nil
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
