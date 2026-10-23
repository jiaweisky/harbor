// Package sandbox owns Harbor's container sandbox behavior: which host roots
// are mounted, how the runtime command line is built, and whether a configured
// runtime is usable on this platform.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/jiaweisky/harbor/internal/core"
)

// Container describes one agent's container sandbox.
type Container struct {
	Runtime   string // runtime binary (docker, podman, nerdctl, or a wrapper)
	Image     string
	Mounts    []string // host roots bind-mounted at their identical paths
	ExtraArgs []string // deployer-provided runtime flags
}

// Mounts builds the bind-mount list. The source workdir is mounted only when the
// isolation strategy needs it; the isolation root and CLI home are always
// mounted when set.
func Mounts(workdir string, needsSource bool, isolationRoot, cliHome string) []string {
	var mounts []string
	if needsSource && workdir != "" {
		mounts = append(mounts, workdir)
	}
	for _, m := range []string{isolationRoot, cliHome} {
		if m != "" {
			mounts = append(mounts, m)
		}
	}
	return mounts
}

// Command builds the runtime invocation: bind-mount the sandbox roots at their
// host paths, set the working directory, forward the environment, add the
// deployer's ExtraArgs, then run the CLI. Environment values are not placed on
// the runtime argv (world readable via /proc): only the variable names are
// passed with -e, so the runtime reads each value from its own (owner-only)
// environment.
func (c *Container) Command(bin string, args []string, dir string, env []string) (string, []string) {
	argv := []string{"run", "--rm", "-i"}
	for _, m := range c.Mounts {
		argv = append(argv, "-v", m+":"+m)
	}
	if dir != "" {
		argv = append(argv, "-w", dir)
	}
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		argv = append(argv, "-e", key)
	}
	argv = append(argv, c.ExtraArgs...)
	argv = append(argv, c.Image, bin)
	argv = append(argv, args...)
	return c.Runtime, argv
}

// infoTimeout bounds the startup probe of the container runtime.
const infoTimeout = 10 * time.Second

// CheckRuntime verifies that the runtime is supported on this platform, its
// binary exists, and its daemon or VM is reachable. It runs once at startup so a
// broken container sandbox fails before the first task instead of at task time.
func CheckRuntime(ctx context.Context, rt core.ContainerRuntime) error {
	switch rt {
	case core.ContainerRuntimeDocker, core.ContainerRuntimePodman:
		// Native on Linux; macOS and Windows run them through a VM.
	case core.ContainerRuntimeNerdctl:
		if runtime.GOOS != "linux" {
			return fmt.Errorf("runtime %q is only supported on Linux", rt)
		}
	default:
		return fmt.Errorf("runtime %q is not supported", rt)
	}
	path, err := exec.LookPath(string(rt))
	if err != nil {
		return fmt.Errorf("runtime %q not found: %w", rt, err)
	}
	probeCtx, cancel := context.WithTimeout(ctx, infoTimeout)
	defer cancel()
	out, err := exec.CommandContext(probeCtx, path, "info").CombinedOutput()
	if errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("runtime %q did not answer info within %s", rt, infoTimeout)
	}
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if i := strings.IndexByte(msg, '\n'); i >= 0 {
			msg = strings.TrimSpace(msg[:i])
		}
		if msg != "" {
			return fmt.Errorf("runtime %q is not reachable: %w: %s", rt, err, msg)
		}
		return fmt.Errorf("runtime %q is not reachable: %w", rt, err)
	}
	return nil
}
