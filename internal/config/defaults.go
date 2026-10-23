package config

import (
	"path/filepath"

	"github.com/jiaweisky/harbor/internal/cli"
	"github.com/jiaweisky/harbor/internal/core"
	"github.com/jiaweisky/harbor/internal/workspace"
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
	if c.Sandbox.Isolation == "" {
		c.Sandbox.Isolation = workspace.IsolationShared
	}
	if c.Sandbox.Isolation == workspace.IsolationCopy && c.Sandbox.CopyStrategy == "" {
		c.Sandbox.CopyStrategy = workspace.CopyFull
	}
}

func applyAgentDefaults(a *AgentConfig) {
	// Normalize to an absolute path: container bind mounts cannot use relative
	// paths, and change capture needs a stable directory.
	if a.Workdir != "" {
		if abs, err := filepath.Abs(a.Workdir); err == nil {
			a.Workdir = abs
		}
	}
	if a.Interaction == "" {
		a.Interaction = InteractionAuto
	}
	p, ok := cli.Lookup(a.Binary)
	if !ok {
		return
	}
	if a.Integration == "" {
		a.Integration = p.Integration
	}
	if a.Parser == "" {
		a.Parser = p.Parser
	}
	if a.Integration == IntegrationACP && len(a.Args) == 0 {
		a.Args = append([]string(nil), p.ACPArgs...)
	}
	if a.Session.ForkFlag == "" {
		a.Session.ForkFlag = p.ForkFlag
	}
	defaultStrings(&a.OutputSchemaArgs, p.OutputSchemaArgs)
	defaultStrings(&a.FileArgs, p.FileArgs)
	defaultStrings(&a.ImageArgs, p.ImageArgs)
	defaultStrings(&a.InputImage, p.InputImage)
}

// defaultStrings replaces an empty dst with a copy of src.
func defaultStrings(dst *[]string, src []string) {
	if len(*dst) == 0 {
		*dst = append([]string(nil), src...)
	}
}
