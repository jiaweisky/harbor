package config

import (
	"path/filepath"

	"github.com/jiaweisky/harbor/internal/core"
)

// Core maps the configured limits to their runtime form.
func (l LimitsConfig) Core() core.Limits {
	return core.Limits{
		MaxConcurrent: l.MaxConcurrent,
		Timeout:       l.Timeout,
		IdleTimeout:   l.IdleTimeout,
		MaxTurns:      l.MaxTurns,
		Breaker:       l.Breaker.Core(),
	}
}

// Core maps the configured breaker to its runtime form.
func (b *BreakerConfig) Core() *core.Breaker {
	if b == nil {
		return nil
	}
	return &core.Breaker{
		Threshold:         b.Threshold,
		FailureRatio:      b.FailureRatio,
		HalfOpenSuccesses: b.HalfOpenSuccesses,
		Cooldown:          b.Cooldown,
		MaxCooldown:       b.MaxCooldown,
	}
}

func (c *Config) applyDefaults() {
	c.applyAuthDefaults()
	c.applyTelemetryDefaults()
	c.applySandboxDefaults()
	for i := range c.Agents {
		applyAgentDefaults(&c.Agents[i])
	}
}

func (c *Config) applyAuthDefaults() {
	if c.Server.Auth.Mode == "" {
		c.Server.Auth.Mode = AuthModeNone
	}
}

func (c *Config) applyTelemetryDefaults() {
	if c.Telemetry.ServiceName == "" {
		c.Telemetry.ServiceName = "harbor"
	}
}

func (c *Config) applySandboxDefaults() {
	c.Sandbox.IsolationRoot = absolutePath(c.Sandbox.IsolationRoot)
	c.Sandbox.CLIHome = absolutePath(c.Sandbox.CLIHome)
	if c.Sandbox.Isolation == "" {
		c.Sandbox.Isolation = core.IsolationShared
	}
	if c.Sandbox.Isolation == core.IsolationCopy && c.Sandbox.CopyStrategy == "" {
		c.Sandbox.CopyStrategy = core.CopyFull
	}
}

func applyAgentDefaults(a *AgentConfig) {
	a.Workdir = absolutePath(a.Workdir)
	if a.Interaction == "" {
		a.Interaction = core.InteractionAuto
	}
}

// absolutePath normalizes a non-empty path for container bind mounts, which
// require host paths to be absolute.
func absolutePath(path string) string {
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
}
