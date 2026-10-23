package cli

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/jiaweisky/harbor/internal/config"
	"github.com/jiaweisky/harbor/internal/core"
	"github.com/jiaweisky/harbor/internal/plugins"
	"github.com/jiaweisky/harbor/internal/workspace"
)

// Load reads a config file, loads configured plugins, and applies the CLI
// preparation step.
func Load(path string) (*config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	if err := workspace.CheckCopyStrategy(cfg.Sandbox.Isolation, cfg.Sandbox.CopyStrategy); err != nil {
		return nil, err
	}
	if err := plugins.Load(cfg.Plugins); err != nil {
		return nil, fmt.Errorf("plugins: %w", err)
	}
	if err := Prepare(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Prepare applies profile-derived defaults to every agent and validates the
// CLI-specific parts of the agent configuration. It must run after config.Load.
func Prepare(cfg *config.Config) error {
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		applyDefaults(a)
		if err := validateAgent(a); err != nil {
			return err
		}
	}
	return nil
}

func applyDefaults(a *config.AgentConfig) {
	p, ok := LookupProfile(a.Binary)
	if !ok {
		return
	}
	if a.Integration == "" {
		a.Integration = p.Integration
	}
	if a.Parser == "" {
		a.Parser = p.Parser
	}
	if a.Integration == core.IntegrationACP && len(a.Args) == 0 {
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

func validateAgent(a *config.AgentConfig) error {
	p, known := LookupProfile(a.Binary)
	if known {
		if !p.Supports(a.Integration) {
			return fmt.Errorf(
				"agent %q: binary %q does not support integration %q (supported: %s)",
				a.ID, filepath.Base(a.Binary), a.Integration, core.IntegrationNames(p.Supported),
			)
		}
	} else if a.Integration == "" {
		return fmt.Errorf(
			"agent %q: unknown binary %q; set \"integration\" explicitly (known values: %s)",
			a.ID, a.Binary, core.IntegrationList(),
		)
	}
	if !a.Integration.Known() {
		return fmt.Errorf(
			"agent %q: unknown integration %q (known values: %s)",
			a.ID, a.Integration, core.IntegrationList(),
		)
	}
	if a.Integration == core.IntegrationExec && !hasPromptTemplate(a.Args) {
		return fmt.Errorf(
			"agent %q: integration %q passes the prompt through args but none contains {{.Prompt}}; "+
				"add {{.Prompt}} to args, or use %q if the CLI has a structured stream",
			a.ID, core.IntegrationExec, core.IntegrationStreamJSON,
		)
	}
	if a.Integration == core.IntegrationACP && len(a.Args) == 0 {
		return fmt.Errorf(
			"agent %q: integration %q requires args selecting the CLI's ACP mode "+
				"(for example [\"acp\"]); without them the CLI would start its interactive UI",
			a.ID, core.IntegrationACP,
		)
	}
	switch a.Interaction {
	case core.InteractionAuto, core.InteractionInteractive, core.InteractionNonInteractive:
	default:
		return fmt.Errorf(
			"agent %q: interaction must be one of auto, interactive, non-interactive; got %q",
			a.ID, a.Interaction,
		)
	}
	if a.Interaction == core.InteractionInteractive &&
		a.Integration != core.IntegrationStreamJSON && a.Integration != core.IntegrationACP {
		return fmt.Errorf(
			"agent %q: interaction \"interactive\" requires a pausable integration (%q or %q); got %q",
			a.ID, core.IntegrationStreamJSON, core.IntegrationACP, a.Integration,
		)
	}
	return nil
}

func defaultStrings(dst *[]string, src []string) {
	if len(*dst) == 0 {
		*dst = append([]string(nil), src...)
	}
}

func hasPromptTemplate(args []string) bool {
	for _, a := range args {
		if strings.Contains(a, "{{.Prompt}}") {
			return true
		}
	}
	return false
}
