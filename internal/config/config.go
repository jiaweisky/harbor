// Package config loads Harbor's agents.yaml.
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration.
type Config struct {
	Version int           `yaml:"version"`
	Server  ServerConfig  `yaml:"server"`
	Sandbox SandboxConfig `yaml:"sandbox"`
	Agents  []AgentConfig `yaml:"agents"`
}

// ServerConfig configures the HTTP server and discovery.
type ServerConfig struct {
	Listen                   string     `yaml:"listen"`
	BaseURL                  string     `yaml:"base_url"`
	AllowInsecure            bool       `yaml:"allow_insecure"`              // permit a non-loopback bind without auth
	PushAllowPrivateNetworks bool       `yaml:"push_allow_private_networks"` // disable the SSRF guard on push webhooks
	TaskStoreFile            string     `yaml:"task_store_file"`             // persist tasks to this JSON file (optional)
	Auth                     AuthConfig `yaml:"auth"`
	CORSAllowedOrigins       []string   `yaml:"cors_allowed_origins"`
}

// AuthConfig configures bearer authentication for the JSON-RPC endpoints.
type AuthConfig struct {
	Mode   string   `yaml:"mode"` // none | bearer
	Tokens []string `yaml:"tokens"`
}

// SandboxConfig configures workspace and env constraints.
type SandboxConfig struct {
	WorkdirRoot   string `yaml:"workdir_root"`
	AllowOutside  bool   `yaml:"allow_outside"`
	Isolation     string `yaml:"isolation"`      // shared | worktree | copy
	IsolationRoot string `yaml:"isolation_root"` // base dir for worktree/copy (required for those)
	InheritEnv    *bool  `yaml:"inherit_env"`    // default true; false restricts to an allowlisted env
	CLIHome       string `yaml:"cli_home"`       // optional root for the CLI's config/home
	StateDir      string `yaml:"state_dir"`      // runtime state (orphan tracking); default under the temp dir
}

// InheritEnvEnabled reports whether child processes inherit the parent
// environment. It defaults to true; set inherit_env: false to restrict the
// child to an allowlisted environment plus each agent's env.
func (s SandboxConfig) InheritEnvEnabled() bool {
	return s.InheritEnv == nil || *s.InheritEnv
}

// Integration selects how Harbor talks to a coding-agent CLI.
type Integration string

const (
	// IntegrationStreamJSON drives a CLI's structured duplex stream: the prompt
	// is written to stdin and NDJSON events are read from stdout. Use it when
	// the CLI supports a structured stream mode (e.g. `-p --output-format
	// stream-json`). Gives token streaming and native interaction.
	IntegrationStreamJSON Integration = "stream-json"
	// IntegrationExec runs a one-shot invocation and reads plain text output.
	// Use it when the CLI only offers one-shot execution (no structured stream).
	// Streaming is line-level only and there is no native interaction.
	IntegrationExec Integration = "exec"
	// IntegrationServer proxies a CLI's own HTTP server or app-server, keeping
	// one long-lived process and reusing server-side sessions across turns.
	IntegrationServer Integration = "server"
	// IntegrationACP speaks the Agent Client Protocol to a CLI.
	IntegrationACP Integration = "acp"
)

// Server protocols for IntegrationServer. A CLI's HTTP server may expose a REST
// API, ACP over SSE, or both (for example `codebuddy --serve`); the protocol
// selects which one Harbor drives.
const (
	// ServerProtocolHTTP proxies the CLI's REST/HTTP API (default).
	ServerProtocolHTTP = "http"
	// ServerProtocolACP speaks ACP over the CLI's HTTP/SSE endpoint.
	ServerProtocolACP = "acp"
)

// knownIntegrations lists every recognized integration value.
var knownIntegrations = []Integration{
	IntegrationStreamJSON, IntegrationExec, IntegrationServer, IntegrationACP,
}

// Known reports whether the integration is a recognized value.
func (i Integration) Known() bool {
	for _, k := range knownIntegrations {
		if i == k {
			return true
		}
	}
	return false
}

// integrationList renders the known values for error messages.
func integrationList() string { return integrationNames(knownIntegrations) }

// integrationNames renders a set of integrations for error messages.
func integrationNames(vals []Integration) string {
	names := make([]string, 0, len(vals))
	for _, v := range vals {
		names = append(names, string(v))
	}
	return strings.Join(names, ", ")
}

// AgentConfig configures one exposed coding-agent CLI.
type AgentConfig struct {
	ID             string            `yaml:"id"`
	DisplayName    string            `yaml:"display_name"`
	Binary         string            `yaml:"binary"`
	Integration    Integration       `yaml:"integration"`
	Parser         string            `yaml:"parser"`          // exec framing: text | codex | regex; default text
	Regex          *RegexConfig      `yaml:"regex"`           // parser: regex
	ServerURL      string            `yaml:"server_url"`      // integration: server; connect instead of spawning
	ServerProtocol string            `yaml:"server_protocol"` // integration: server; "http" (default) or "acp"
	Workdir        string            `yaml:"workdir"`         // default working dir (under workdir_root)
	Args           []string          `yaml:"args"`
	Env            map[string]string `yaml:"env"`
	AllowDangerous bool              `yaml:"allow_dangerous"`
	// SDKCaps declares stream-json SDK capabilities (currently askUserQuestion)
	// before the first prompt, so the CLI routes AskUserQuestion through
	// can_use_tool and Harbor can surface it as an input-required question.
	SDKCaps bool          `yaml:"sdk_capabilities"`
	Version string        `yaml:"version"` // optional card version override; skips probing
	Session SessionConfig `yaml:"session"`
	Limits  LimitsConfig  `yaml:"limits"`
}

// RegexConfig configures the regex parser.
type RegexConfig struct {
	Text string `yaml:"text"`
}

// Profile describes a known coding-agent CLI: which integrations it supports,
// the default integration, the minimum supported version, and how to read the
// version. The CLI's authentication surface lives with the CLI's adapter.
type Profile struct {
	Binary      string        // matched against the binary's base name
	Integration Integration   // default integration
	Parser      string        // default exec parser
	Supported   []Integration // accepted integrations
	MinVersion  string        // minimum supported version; below it Harbor refuses to start
	VersionArgs []string      // version flag(s); nil disables probing
	ACPArgs     []string      // args selecting ACP mode, when unset in the agent config
}

// Supports reports whether the profile accepts the integration.
func (p Profile) Supports(i Integration) bool {
	for _, s := range p.Supported {
		if s == i {
			return true
		}
	}
	return false
}

// knownProfiles maps well-known CLIs to their capabilities. MinVersion is the
// version Harbor has been validated against; older versions are rejected so the
// user upgrades rather than hitting undefined behavior. Unknown binaries fall
// through to explicit configuration.
var knownProfiles = []Profile{
	{Binary: "codebuddy", Integration: IntegrationStreamJSON, MinVersion: "2.161.4", VersionArgs: []string{"--version"},
		Supported: []Integration{IntegrationStreamJSON, IntegrationExec, IntegrationACP, IntegrationServer}},
	{Binary: "claude", Integration: IntegrationStreamJSON, MinVersion: "2.1.292", VersionArgs: []string{"--version"},
		Supported: []Integration{IntegrationStreamJSON}},
	{Binary: "opencode", Integration: IntegrationExec, MinVersion: "1.18.35", VersionArgs: []string{"--version"},
		Supported: []Integration{IntegrationExec, IntegrationACP, IntegrationServer}, ACPArgs: []string{"acp"}},
	{Binary: "codex", Integration: IntegrationExec, Parser: "codex",
		MinVersion: "0.160.1", VersionArgs: []string{"--version"},
		Supported: []Integration{IntegrationExec, IntegrationServer}},
}

// LookupProfile returns the profile for a binary path or name.
func LookupProfile(binary string) (Profile, bool) {
	name := filepath.Base(binary)
	for _, p := range knownProfiles {
		if p.Binary == name {
			return p, true
		}
	}
	return Profile{}, false
}

// SessionConfig maps session-continuation flags for a CLI.
type SessionConfig struct {
	IDFlag       string `yaml:"id_flag"`
	ResumeFlag   string `yaml:"resume_flag"`
	ContinueFlag string `yaml:"continue_flag"`
}

// LimitsConfig bounds concurrent execution for an agent.
type LimitsConfig struct {
	MaxConcurrent int            `yaml:"max_concurrent"`
	Timeout       time.Duration  `yaml:"timeout"`      // total task timeout; 0 disables
	IdleTimeout   time.Duration  `yaml:"idle_timeout"` // fail if the CLI produces no output for this long; 0 disables
	Breaker       *BreakerConfig `yaml:"breaker"`      // optional failure circuit breaker
}

// BreakerConfig configures the per-agent failure circuit breaker.
type BreakerConfig struct {
	Threshold   int           `yaml:"threshold"`    // consecutive failures that open the breaker
	Cooldown    time.Duration `yaml:"cooldown"`     // open duration before a probe
	MaxCooldown time.Duration `yaml:"max_cooldown"` // backoff cap
}

// Load reads and validates a config file, applying defaults.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, err
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// MustLoad is Load that panics on error.
func MustLoad(path string) *Config {
	cfg, err := Load(path)
	if err != nil {
		panic(err)
	}
	return cfg
}

func (c *Config) applyDefaults() {
	if c.Server.Listen == "" {
		c.Server.Listen = "127.0.0.1:8080"
	}
	if c.Server.Auth.Mode == "" {
		c.Server.Auth.Mode = "none"
	}
	if c.Sandbox.Isolation == "" {
		c.Sandbox.Isolation = "shared"
	}
	if c.Sandbox.StateDir == "" {
		c.Sandbox.StateDir = filepath.Join(os.TempDir(), "harbor")
	}
	for i := range c.Agents {
		a := &c.Agents[i]
		if p, ok := LookupProfile(a.Binary); ok {
			if a.Integration == "" {
				a.Integration = p.Integration
			}
			if a.Parser == "" {
				a.Parser = p.Parser
			}
			if a.Integration == IntegrationACP && len(a.Args) == 0 {
				a.Args = append([]string(nil), p.ACPArgs...)
			}
		}
	}
}

func (c *Config) validate() error {
	switch c.Server.Auth.Mode {
	case "none", "bearer":
	default:
		return fmt.Errorf("server.auth.mode must be one of none, bearer; got %q", c.Server.Auth.Mode)
	}
	if len(c.Agents) == 0 {
		return fmt.Errorf("at least one agent must be configured")
	}
	if c.Sandbox.WorkdirRoot != "" {
		info, err := os.Stat(c.Sandbox.WorkdirRoot)
		if err != nil || !info.IsDir() {
			return fmt.Errorf("sandbox.workdir_root %q is not a directory", c.Sandbox.WorkdirRoot)
		}
	}
	switch c.Sandbox.Isolation {
	case "shared":
	case "worktree", "copy":
		if c.Sandbox.IsolationRoot == "" {
			return fmt.Errorf("sandbox.isolation_root is required for isolation %q", c.Sandbox.Isolation)
		}
	default:
		return fmt.Errorf("sandbox.isolation must be one of shared, worktree, copy; got %q", c.Sandbox.Isolation)
	}
	for i := range c.Agents {
		a := &c.Agents[i]
		if a.ID == "" {
			return fmt.Errorf("agents[%d]: id is required", i)
		}
		if a.Binary == "" {
			return fmt.Errorf("agent %q: binary is required", a.ID)
		}
		if p, ok := LookupProfile(a.Binary); ok {
			if !p.Supports(a.Integration) {
				return fmt.Errorf(
					"agent %q: binary %q does not support integration %q (supported: %s)",
					a.ID, filepath.Base(a.Binary), a.Integration, integrationNames(p.Supported),
				)
			}
		} else if a.Integration == "" {
			return fmt.Errorf(
				"agent %q: unknown binary %q; set \"integration\" explicitly (known values: %s)",
				a.ID, a.Binary, integrationList(),
			)
		}
		if !a.Integration.Known() {
			return fmt.Errorf("agent %q: unknown integration %q (known values: %s)", a.ID, a.Integration, integrationList())
		}
		if a.Integration == IntegrationExec && !hasPromptTemplate(a.Args) {
			return fmt.Errorf(
				"agent %q: integration %q passes the prompt through args but none contains {{.Prompt}}; "+
					"add {{.Prompt}} to args, or use %q if the CLI has a structured stream",
				a.ID, IntegrationExec, IntegrationStreamJSON,
			)
		}
		if a.Integration == IntegrationACP && len(a.Args) == 0 {
			return fmt.Errorf(
				"agent %q: integration %q requires args selecting the CLI's ACP mode "+
					"(for example [\"acp\"]); without them the CLI would start its interactive UI",
				a.ID, IntegrationACP,
			)
		}
		switch a.ServerProtocol {
		case "", ServerProtocolHTTP, ServerProtocolACP:
		default:
			return fmt.Errorf(
				"agent %q: server_protocol must be %q or %q; got %q",
				a.ID, ServerProtocolHTTP, ServerProtocolACP, a.ServerProtocol,
			)
		}
	}
	return nil
}

// IntegrationList renders every known integration value, for error messages.
func IntegrationList() string { return integrationList() }

// IntegrationNames renders a set of integrations, for error messages.
func IntegrationNames(vals []Integration) string { return integrationNames(vals) }

// hasPromptTemplate reports whether any argument carries the prompt placeholder.
func hasPromptTemplate(args []string) bool {
	for _, a := range args {
		if strings.Contains(a, "{{.Prompt}}") {
			return true
		}
	}
	return false
}
