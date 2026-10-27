//go:build linux

package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// overlayLayout returns the upper, work, merged, and mount-option paths of a
// per-context overlayfs view. Commas and colons are rejected because overlayfs
// mount options use them as separators.
func overlayLayout(src, dst string) (upper, work, merged, opts string, err error) {
	upper = filepath.Join(dst, "upper")
	work = filepath.Join(dst, "work")
	merged = filepath.Join(dst, "merged")
	for _, p := range []string{src, upper, work} {
		if strings.ContainsAny(p, ",:") {
			return "", "", "", "", fmt.Errorf("overlayfs path must not contain ':' or ',': %q", p)
		}
	}
	return upper, work, merged, fmt.Sprintf(
		"lowerdir=%s,upperdir=%s,workdir=%s", src, upper, work,
	), nil
}

func mergedView(dst string) string { return filepath.Join(dst, "merged") }

// ensureOverlayFS mounts a per-context overlayfs view, reusing an existing
// mount or remounting persistent upper/work directories after a restart.
func ensureOverlayFS(src, dst string) error {
	upper, work, merged, opts, err := overlayLayout(src, dst)
	if err != nil {
		return err
	}
	for _, dir := range []string{upper, work, merged} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	mounted, err := isOverlayMount(merged)
	if err != nil {
		return err
	}
	if mounted {
		return nil
	}
	if err := unix.Mount("overlay", merged, "overlay", 0, opts); err != nil {
		return fmt.Errorf("overlayfs mount %s: %w", merged, err)
	}
	return nil
}

// releaseOverlayFS detaches the per-context mount. The upper layer is kept so
// the same context can be remounted later.
func releaseOverlayFS(dst string) error {
	merged := filepath.Join(dst, "merged")
	mounted, err := isOverlayMount(merged)
	if err != nil {
		return err
	}
	if !mounted {
		return nil
	}
	if err := unix.Unmount(merged, unix.MNT_DETACH); err != nil {
		return fmt.Errorf("overlayfs unmount %s: %w", merged, err)
	}
	return nil
}

// checkOverlayFS verifies privileges and filesystem support before serving.
func checkOverlayFS(isolationRoot string) error {
	if isolationRoot == "" {
		return fmt.Errorf("sandbox.isolation_root is required for isolation %q", IsolationOverlayFS)
	}
	if err := os.MkdirAll(isolationRoot, 0o755); err != nil {
		return err
	}
	return probeOverlayFS(isolationRoot)
}

// cleanupOverlayMounts detaches stale per-context mounts under isolationRoot.
// One Harbor instance must exclusively own an isolation_root.
func cleanupOverlayMounts(isolationRoot string) error {
	entries, err := os.ReadDir(isolationRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(isolationRoot, entry.Name())
		var merged string
		remove := false
		switch {
		case strings.HasPrefix(entry.Name(), "ctx-"):
			merged = filepath.Join(dir, "merged")
		case strings.HasPrefix(entry.Name(), ".harbor-overlayfs-probe-"):
			merged = filepath.Join(dir, "view", "merged")
			remove = true
		default:
			continue
		}
		mounted, err := isOverlayMount(merged)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if mounted {
			if err := unix.Unmount(merged, unix.MNT_DETACH); err != nil {
				return fmt.Errorf("overlayfs cleanup %s: %w", merged, err)
			}
		}
		if remove {
			if err := os.RemoveAll(dir); err != nil {
				return err
			}
		}
	}
	return nil
}

// probeOverlayFS mounts and unmounts a temporary view, confirming that Harbor
// has the required privileges and that the upper filesystem supports overlayfs.
func probeOverlayFS(isolationRoot string) error {
	base, err := os.MkdirTemp(isolationRoot, ".harbor-overlayfs-probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(base)
	src := filepath.Join(base, "lower")
	dst := filepath.Join(base, "view")
	for _, dir := range []string{src, dst} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(src, "probe"), []byte("harbor"), 0o600); err != nil {
		return err
	}
	upper, work, merged, opts, err := overlayLayout(src, dst)
	if err != nil {
		return err
	}
	for _, dir := range []string{upper, work, merged} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := unix.Mount("overlay", merged, "overlay", 0, opts); err != nil {
		return fmt.Errorf(
			"isolation %q requires CAP_SYS_ADMIN in Harbor's mount namespace: %w",
			IsolationOverlayFS, err,
		)
	}
	defer unix.Unmount(merged, unix.MNT_DETACH)
	if _, err := os.Stat(filepath.Join(merged, "probe")); err != nil {
		return fmt.Errorf("overlayfs probe read: %w", err)
	}
	return nil
}

// isOverlayMount reports whether path is itself an overlayfs mountpoint. A
// Statfs type check is not enough: a plain directory on an overlayfs root would
// be misreported as a mount.
func isOverlayMount(path string) (bool, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false, err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	mounted := false
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		sep := -1
		for i, field := range fields {
			if field == "-" {
				sep = i
				break
			}
		}
		if sep < 0 || sep+1 >= len(fields) || len(fields) < 5 {
			continue
		}
		if unescapeMountField(fields[4]) != abs {
			continue
		}
		mounted = fields[sep+1] == "overlay"
	}
	return mounted, nil
}

func unescapeMountField(s string) string {
	return strings.NewReplacer(
		`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`,
	).Replace(s)
}
