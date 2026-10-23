// Package core defines runtime types shared by Harbor's layers. It must not
// depend on the YAML schema or on infrastructure packages, so both the
// configuration loader and the execution layers can use it.
package core

import "time"

// Limits bounds the execution of one agent's tasks.
type Limits struct {
	MaxConcurrent int
	Timeout       time.Duration
	IdleTimeout   time.Duration
	MaxTurns      int
	Breaker       *Breaker
}

// Breaker configures the per-agent failure circuit breaker.
type Breaker struct {
	Threshold         int
	FailureRatio      float64
	HalfOpenSuccesses int
	Cooldown          time.Duration
	MaxCooldown       time.Duration
}

// Interaction selects how the CLI's permission/plan/question prompts are
// surfaced.
type Interaction string

const (
	InteractionAuto           Interaction = "auto"
	InteractionInteractive    Interaction = "interactive"
	InteractionNonInteractive Interaction = "non-interactive"
)

// Integration selects how Harbor talks to a coding-agent CLI.
type Integration string

const (
	IntegrationStreamJSON Integration = "stream-json"
	IntegrationExec       Integration = "exec"
	IntegrationServer     Integration = "server"
	IntegrationACP        Integration = "acp"
)

// Known reports whether the integration is a recognized value.
func (i Integration) Known() bool {
	switch i {
	case IntegrationStreamJSON, IntegrationExec, IntegrationServer, IntegrationACP:
		return true
	}
	return false
}

// Integrations returns every recognized integration value.
func Integrations() []Integration {
	return []Integration{IntegrationStreamJSON, IntegrationExec, IntegrationServer, IntegrationACP}
}

// ContainerRuntime is a container runtime with a Docker-compatible CLI.
type ContainerRuntime string

const (
	// ContainerRuntimeDocker runs natively on Linux and through Docker Desktop
	// on macOS and Windows.
	ContainerRuntimeDocker ContainerRuntime = "docker"
	// ContainerRuntimePodman runs natively on Linux and through podman machine
	// on macOS and Windows.
	ContainerRuntimePodman ContainerRuntime = "podman"
	// ContainerRuntimeNerdctl requires containerd and is Linux-only.
	ContainerRuntimeNerdctl ContainerRuntime = "nerdctl"
)

// ParserKind selects how a CLI's exec output is parsed.
type ParserKind string

const (
	ParserText       ParserKind = "text"
	ParserStreamJSON ParserKind = "stream-json"
	ParserCodex      ParserKind = "codex"
	ParserRegex      ParserKind = "regex"
)

// ServerProtocol selects how Harbor drives a CLI server integration.
type ServerProtocol string

const (
	ServerProtocolHTTP              ServerProtocol = "http"
	ServerProtocolACP               ServerProtocol = "acp"
	ServerProtocolACPStreamableHTTP ServerProtocol = "acp-streamable-http"
)

// Regex configures the regex exec parser.
type Regex struct {
	Text string
}

// Session maps a CLI's session-continuation flags.
type Session struct {
	IDFlag, ResumeFlag, ContinueFlag, ForkFlag string
}

// Skill is one agent-card skill.
type Skill struct {
	ID, Name, Description                   string
	Tags, Examples, InputModes, OutputModes []string
}

// AgentSpec is an agent's runtime configuration, independent of the YAML schema.
type AgentSpec struct {
	ID, DisplayName, Description, Binary string
	Integration                          Integration
	Parser                               ParserKind
	Regex                                *Regex
	ServerURL                            string
	ServerProtocol                       ServerProtocol
	Args                                 []string
	Env                                  map[string]string
	AllowDangerous                       bool
	SDKCaps                              bool
	OutputSchemaArgs                     []string
	FileArgs                             []string
	ImageArgs                            []string
	InputImage                           []string
	Interaction                          Interaction
	Session                              Session
	Limits                               Limits
	Skills                               []Skill
	Version                              string
}
