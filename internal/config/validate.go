package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/jiaweisky/harbor/internal/core"
)

// hasParentSegment reports whether p contains a ".." path segment.
func hasParentSegment(p string) bool {
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

// pathContains reports whether target is root or lies under it.
func pathContains(root, target string) (bool, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return false, err
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false, err
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))), nil
}

func (c *Config) validate() error {
	if len(c.Agents) == 0 {
		return fmt.Errorf("at least one agent must be configured")
	}
	if err := c.validateServer(); err != nil {
		return err
	}
	if err := c.validateSandbox(); err != nil {
		return err
	}
	for i := range c.Agents {
		if err := validateAgent(i, &c.Agents[i]); err != nil {
			return err
		}
	}
	return nil
}

func (c *Config) validateServer() error {
	if c.Server.Listen == "" {
		return fmt.Errorf("server.listen is required")
	}
	switch c.Server.Auth.Mode {
	case AuthModeNone, AuthModeBearer:
	default:
		return fmt.Errorf("server.auth.mode must be one of none, bearer; got %q", c.Server.Auth.Mode)
	}
	if c.Server.Auth.Mode == AuthModeNone && !c.Server.AllowInsecure && !isLoopback(c.Server.Listen) {
		return fmt.Errorf(
			"refusing to bind %q with auth.mode=none; set server.auth.mode or server.allow_insecure",
			c.Server.Listen,
		)
	}
	switch c.Server.Store {
	case StoreMemory, StoreSQLite:
	default:
		return fmt.Errorf("server.store must be empty (in-memory) or sqlite; got %q", c.Server.Store)
	}
	if c.Server.Store == StoreSQLite && c.Server.StorePath == "" && c.Server.SessionStoreDir == "" {
		return fmt.Errorf("server.store: sqlite requires store_path or session_store_dir")
	}
	return nil
}

func (c *Config) validateSandbox() error {
	if c.Sandbox.StateDir == "" {
		return fmt.Errorf("sandbox.state_dir is required")
	}
	if c.Sandbox.ArtifactInlineMaxBytes < 0 {
		return fmt.Errorf("sandbox.artifact_inline_max_bytes must not be negative")
	}
	if c.Sandbox.DiffInlineMaxBytes < 0 {
		return fmt.Errorf("sandbox.diff_inline_max_bytes must not be negative")
	}
	for _, g := range c.Sandbox.NonGitChangeGlobs {
		g = filepath.ToSlash(strings.TrimSpace(g))
		if g == "" || filepath.IsAbs(g) || hasParentSegment(g) {
			return fmt.Errorf("sandbox.non_git_change_globs must be relative patterns: %q", g)
		}
		if !doublestar.ValidatePattern(g) {
			return fmt.Errorf("sandbox.non_git_change_globs has an invalid pattern: %q", g)
		}
	}
	switch c.Sandbox.Isolation {
	case core.IsolationShared:
	case core.IsolationWorktree, core.IsolationCopy:
		if c.Sandbox.IsolationRoot == "" {
			return fmt.Errorf("sandbox.isolation_root is required for isolation %q", c.Sandbox.Isolation)
		}
	default:
		return fmt.Errorf("sandbox.isolation must be one of shared, worktree, copy; got %q", c.Sandbox.Isolation)
	}
	// copy isolation is self-contained, so a container mounts only the isolation
	// root; keep the source workdir outside it or it stays visible to the agent.
	if c.Sandbox.CopyStrategy != "" && c.Sandbox.Isolation != core.IsolationCopy {
		return fmt.Errorf("sandbox.copy_strategy is only valid with isolation copy")
	}
	if c.Sandbox.Isolation == core.IsolationCopy {
		if !c.Sandbox.CopyStrategy.Known() {
			return fmt.Errorf(
				"sandbox.copy_strategy must be one of full, reflink, overlay, hardlink; got %q",
				c.Sandbox.CopyStrategy,
			)
		}
		for i := range c.Agents {
			workdir := c.Agents[i].Workdir
			if workdir == "" {
				continue
			}
			contained, err := pathContains(c.Sandbox.IsolationRoot, workdir)
			if err != nil {
				return fmt.Errorf("sandbox.isolation_root: %w", err)
			}
			if contained {
				return fmt.Errorf(
					"sandbox.isolation_root must not contain agents[%d].workdir for copy isolation: %q",
					i, workdir,
				)
			}
		}
	}
	if ct := c.Sandbox.Container; ct != nil {
		if ct.Image == "" {
			return fmt.Errorf("sandbox.container.image is required")
		}
		if ct.Runtime == "" {
			return fmt.Errorf("sandbox.container.runtime is required")
		}
		// Value check only: platform support and daemon reachability are
		// environment properties and are probed at startup.
		switch ct.Runtime {
		case core.ContainerRuntimeDocker, core.ContainerRuntimePodman, core.ContainerRuntimeNerdctl:
		default:
			return fmt.Errorf(
				"sandbox.container.runtime must be one of docker, podman, nerdctl; got %q",
				ct.Runtime,
			)
		}
	}
	if c.Sandbox.RequireContainerForDangerous && c.Sandbox.Container == nil {
		for i := range c.Agents {
			if c.Agents[i].AllowDangerous {
				return fmt.Errorf(
					"agent %q: allow_dangerous requires sandbox.container when require_container_for_dangerous is set",
					c.Agents[i].ID,
				)
			}
		}
	}
	return nil
}

func validateAgent(i int, a *AgentConfig) error {
	if a.ID == "" {
		return fmt.Errorf("agents[%d]: id is required", i)
	}
	if a.Binary == "" {
		return fmt.Errorf("agent %q: binary is required", a.ID)
	}
	if a.DisplayName == "" {
		return fmt.Errorf("agent %q: display_name is required", a.ID)
	}
	if a.Description == "" {
		return fmt.Errorf("agent %q: description is required", a.ID)
	}
	if a.Workdir == "" {
		return fmt.Errorf("agents[%d]: workdir is required", i)
	}
	info, err := os.Stat(a.Workdir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("agent %q: workdir %q is not a directory", a.ID, a.Workdir)
	}
	switch a.ServerProtocol {
	case "", core.ServerProtocolHTTP, core.ServerProtocolACP, core.ServerProtocolACPStreamableHTTP:
	default:
		return fmt.Errorf(
			"agent %q: server_protocol must be one of %q, %q, %q; got %q",
			a.ID, core.ServerProtocolHTTP, core.ServerProtocolACP, core.ServerProtocolACPStreamableHTTP, a.ServerProtocol,
		)
	}
	for j, s := range a.Skills {
		if s.ID == "" || s.Name == "" || s.Description == "" {
			return fmt.Errorf("agent %q: skills[%d] requires id, name, and description", a.ID, j)
		}
	}
	return nil
}

// isLoopback reports whether listen addresses only the loopback interface.
func isLoopback(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		host = listen
	}
	if host == "localhost" {
		return true
	}
	// An empty host (":8080") binds all interfaces; ParseIP returns nil for it.
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
