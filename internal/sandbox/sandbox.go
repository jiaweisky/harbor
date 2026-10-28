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
	Runtime   string // runtime binary (docker, podman, nerdctl)
	Image     string
	Mounts    []string // host roots bind-mounted at their identical paths
	ExtraArgs []string // deployer-provided runtime flags
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

// Mounts builds the bind-mount list. The source workdir is mounted only when the
// isolation strategy needs it; the isolation root and CLI home are always
// mounted when set. A path that appears more than once is mounted once.
func Mounts(workdir string, needsSource bool, isolationRoot, cliHome string) []string {
	var mounts []string
	add := func(path string) {
		if path == "" {
			return
		}
		for _, existing := range mounts {
			if existing == path {
				return
			}
		}
		mounts = append(mounts, path)
	}
	if needsSource {
		add(workdir)
	}
	add(isolationRoot)
	add(cliHome)
	return mounts
}

// infoTimeout bounds the startup probe of the container runtime.
const infoTimeout = 10 * time.Second

// CheckRuntime verifies that the runtime is supported on this platform, its
// binary exists, and its daemon or VM is reachable. It runs once at startup so a
// broken container sandbox fails before the first task instead of at task time.
func CheckRuntime(ctx context.Context, rt core.ContainerRuntime) error {
	if err := checkPlatform(runtime.GOOS, rt); err != nil {
		return err
	}

	path, err := exec.LookPath(string(rt))
	if err != nil {
		return fmt.Errorf("runtime %q not found: %w", rt, err)
	}

	probeCtx, cancel := context.WithTimeout(ctx, infoTimeout)
	defer cancel()

	out, err := exec.CommandContext(probeCtx, path, "info").CombinedOutput()
	if err == nil {
		return nil
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("runtime %q info probe: %w", rt, ctxErr)
	}

	if errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("runtime %q did not answer info within %s", rt, infoTimeout)
	}

	if msg := lastOutputLine(out); msg != "" {
		return fmt.Errorf("runtime %q is not reachable: %w: %s", rt, err, msg)
	}

	return fmt.Errorf("runtime %q is not reachable: %w", rt, err)
}

// checkPlatform reports whether rt can run the container sandbox on goos.
// Binding host paths at identical container paths requires a Linux host or a VM
// that exposes those paths unchanged; native Windows cannot satisfy that.
func checkPlatform(goos string, rt core.ContainerRuntime) error {
	switch rt {
	case core.ContainerRuntimeDocker, core.ContainerRuntimePodman:
		// Native on Linux; macOS exposes host paths unchanged through its VM.
		if goos == "windows" {
			return fmt.Errorf(
				"runtime %q is not supported for the container sandbox on Windows: "+
					"run Harbor in WSL so host paths are valid inside the Linux container",
				rt,
			)
		}
	case core.ContainerRuntimeNerdctl:
		if goos != "linux" {
			return fmt.Errorf("runtime %q is only supported on Linux", rt)
		}
	default:
		return fmt.Errorf("runtime %q is not supported", rt)
	}
	return nil
}

// lastOutputLine returns the last non-empty line of a runtime's combined output,
// where the actual failure usually appears after any warnings.
func lastOutputLine(out []byte) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}

	return ""
}
