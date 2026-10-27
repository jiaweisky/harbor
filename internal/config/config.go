// Package config loads Harbor's agents.yaml.
package config

import (
	"time"

	"github.com/jiaweisky/harbor/internal/core"
)

// Config is the root configuration.
type Config struct {
	Version   int             `yaml:"version"`
	Server    ServerConfig    `yaml:"server"`
	Sandbox   SandboxConfig   `yaml:"sandbox"`
	Telemetry TelemetryConfig `yaml:"telemetry"`
	// Plugins are Go plugin (.so) paths loaded at startup; each must export a
	// Register() func that registers CLI adapters (optional, experimental).
	Plugins []string      `yaml:"plugins"`
	Agents  []AgentConfig `yaml:"agents"`
}

// TelemetryConfig configures OpenTelemetry trace export.
type TelemetryConfig struct {
	Endpoint    string `yaml:"endpoint"`     // OTLP gRPC collector address (host:port); empty disables tracing
	Insecure    bool   `yaml:"insecure"`     // send traces without TLS
	ServiceName string `yaml:"service_name"` // resource service.name; default harbor
}

// StoreKind selects the persistence backend for tasks and sessions.
type StoreKind string

const (
	StoreMemory StoreKind = ""       // in-memory (default)
	StoreSQLite StoreKind = "sqlite" // durable SQLite store
)

// ServerConfig configures the HTTP server and discovery.
type ServerConfig struct {
	Listen                   string     `yaml:"listen"` // required; bind address (host:port)
	BaseURL                  string     `yaml:"base_url"`
	AllowInsecure            bool       `yaml:"allow_insecure"`              // allow a non-loopback unauthenticated bind
	PushAllowPrivateNetworks bool       `yaml:"push_allow_private_networks"` // disable the push webhook SSRF guard
	StorePath                string     `yaml:"store_path"`                  // SQLite DB path for tasks (store: sqlite)
	SessionStoreDir          string     `yaml:"session_store_dir"`           // dir for per-agent session DBs
	Store                    StoreKind  `yaml:"store"`                       // persistence: "" (in-memory) | sqlite
	Auth                     AuthConfig `yaml:"auth"`
	CORSAllowedOrigins       []string   `yaml:"cors_allowed_origins"`

	// AgentInactivityTimeout bounds how long the server waits for any event
	// from an agent before failing the task; 0 disables it. Default 10m.
	AgentInactivityTimeout *time.Duration `yaml:"agent_inactivity_timeout"`

	// MaxRequestBytes bounds an incoming JSON-RPC/REST request body when set, so
	// oversized inline file parts are rejected before being read into memory.
	MaxRequestBytes int64 `yaml:"max_request_bytes"`

	RateLimit *RateLimitConfig `yaml:"rate_limit"`
	TLS       TLSConfig        `yaml:"tls"`
}

// TLSConfig configures TLS for the server listener.
type TLSConfig struct {
	CertFile   string `yaml:"cert_file"`
	KeyFile    string `yaml:"key_file"`
	CAFile     string `yaml:"ca_file"`     // verify a peer's client certificate
	ClientAuth bool   `yaml:"client_auth"` // require a client certificate (mTLS)
}

// RateLimitConfig configures a per-(user, agent) token-bucket rate limit on the
// invocation endpoints. A zero PerMinute disables it.
type RateLimitConfig struct {
	PerMinute int `yaml:"per_minute"` // sustained requests per minute per key
	Burst     int `yaml:"burst"`      // bucket capacity; default PerMinute
}

// AuthMode selects how the invocation endpoints authenticate callers.
type AuthMode string

const (
	AuthModeNone   AuthMode = "none"
	AuthModeBearer AuthMode = "bearer"
)

// AuthConfig configures bearer authentication for the JSON-RPC endpoints.
type AuthConfig struct {
	Mode   AuthMode `yaml:"mode"` // none | bearer
	Tokens []string `yaml:"tokens"`
}

// SandboxConfig configures workspace and env constraints.
type SandboxConfig struct {
	Isolation     core.Isolation    `yaml:"isolation"`      // shared | worktree | copy | overlayfs
	IsolationRoot string            `yaml:"isolation_root"` // required for worktree/copy/overlayfs
	CopyStrategy  core.CopyStrategy `yaml:"copy_strategy"`  // copy only: full | cow
	InheritEnv    *bool             `yaml:"inherit_env"`    // default true; false restricts to an allowlisted env
	CLIHome       string            `yaml:"cli_home"`       // optional root for the CLI's config/home
	StateDir      string            `yaml:"state_dir"`      // required; CLI process records for orphan cleanup
	Container     *ContainerConfig  `yaml:"container"`      // optional strong isolation: run each CLI in a container
	// RequireContainerForDangerous refuses to start when an agent has
	// allow_dangerous while no container sandbox is configured (optional hardening).
	RequireContainerForDangerous bool `yaml:"require_container_for_dangerous"`
	// NonImageFileArtifacts emits a per-file FilePart artifact for each changed
	// non-image file, in addition to the textual diff (optional; bounded per
	// ArtifactInlineMaxBytes bounds how much of one changed file is inlined into
	// DiffInlineMaxBytes bounds the captured diff before it is truncated; 0 uses
	// NonGitChangeGlobs enables change capture outside a git work tree. It lists
	// RawStreamDir, when set, writes each task's raw CLI stdout/stderr to a file
	// under it for replay/debugging (optional).
	RawStreamDir string `yaml:"raw_stream_dir"`
	// RawStreamCompress gzips the raw stream files.
	RawStreamCompress bool `yaml:"raw_stream_compress"`
}

// ContainerConfig runs each CLI inside a container for filesystem isolation.
// The task working directory (and the CLI home, when set) are bind-mounted at
// their host paths, so paths inside the container match the host and the CLI
// reaches exactly the task's files.
//
// The container's image and ExtraArgs are deployment choices: Harbor passes them
// through and does not constrain what they allow. Isolation bounds a
// prompt-injected agent under those choices, not the deployer who wrote them.
type ContainerConfig struct {
	Image     string                `yaml:"image"`
	Runtime   core.ContainerRuntime `yaml:"runtime"`    // required; docker | podman | nerdctl
	ExtraArgs []string              `yaml:"extra_args"` // passed verbatim to <runtime> run
}

// InheritEnvEnabled reports whether child processes inherit the parent
// environment. It defaults to true; set inherit_env: false to restrict the
// child to an allowlisted environment plus each agent's env.
func (s SandboxConfig) InheritEnvEnabled() bool {
	return s.InheritEnv == nil || *s.InheritEnv
}

// AgentConfig configures one exposed coding-agent CLI.
type AgentConfig struct {
	ID             string              `yaml:"id"`
	DisplayName    string              `yaml:"display_name"`
	Binary         string              `yaml:"binary"`
	Integration    core.Integration    `yaml:"integration"`
	Parser         core.ParserKind     `yaml:"parser"`          // exec framing: text | codex | regex; default text
	Regex          *RegexConfig        `yaml:"regex"`           // parser: regex
	ServerURL      string              `yaml:"server_url"`      // integration: server; connect instead of spawning
	ServerProtocol core.ServerProtocol `yaml:"server_protocol"` // "http" (default) | "acp" | "acp-streamable-http"
	Workdir        string              `yaml:"workdir"`         // required; "." means Harbor's cwd (stored absolute)
	Args           []string            `yaml:"args"`
	Env            map[string]string   `yaml:"env"`
	AllowDangerous bool                `yaml:"allow_dangerous"`
	// SDKCaps declares stream-json SDK capabilities (currently askUserQuestion)
	// before the first prompt, so the CLI routes AskUserQuestion through
	// can_use_tool and Harbor can surface it as an input-required question.
	SDKCaps bool `yaml:"sdk_capabilities"`
	// OutputSchemaArgs are appended (after writing the request's JSON Schema to a
	// file) when a request carries harbor.outputSchema. {{.SchemaFile}} is
	// replaced with the schema file path; {{.Schema}} with the schema JSON. Used
	// by CLIs whose structured-output flag is not --json-schema (e.g. codex
	// --output-schema). The CLI's final text is then emitted as a DataPart.
	OutputSchemaArgs []string `yaml:"output_schema_args"`
	// FileArgs are appended once per materialized file part ({{.File}} replaced
	// with the file path), so the CLI attaches the file natively (e.g. opencode
	// -f) instead of only mentioning its path in the prompt.
	FileArgs []string `yaml:"file_args"`
	// ImageArgs are appended once per materialized image part ({{.Image}}
	// replaced with the file path), for a CLI that takes images as arguments
	// (e.g. codex -i) rather than native content blocks.
	ImageArgs []string `yaml:"image_args"`
	// InputImage lists the image MIME types this agent accepts; unset uses the
	// CLI profile's list. They are sent as native content blocks when the
	// integration supports structured input, otherwise via ImageArgs.
	InputImage []string `yaml:"input_image"`
	// Description is the agent card's description text.
	Description string `yaml:"description"`
	// Skills lists the skills advertised on the agent card. When empty, no
	// skills are advertised (the A2A "skills" field is still present).
	Skills  []SkillConfig `yaml:"skills"`
	Version string        `yaml:"version"` // optional card version override; skips probing
	// Interaction controls whether the CLI's permission/plan/question prompts are
	// surfaced as A2A input-required. auto (default) derives it from the
	// integration; interactive forces it on; non-interactive turns it off.
	Interaction core.Interaction `yaml:"interaction"`
	Session     SessionConfig    `yaml:"session"`
	Limits      LimitsConfig     `yaml:"limits"`
}

// RegexConfig configures the regex parser.
type RegexConfig struct {
	Text string `yaml:"text"`
}

// SkillConfig is one AgentSkill advertised on an agent's card. id, name, and
// description are required; tags/examples/modes are optional.
type SkillConfig struct {
	ID          string   `yaml:"id"`
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Tags        []string `yaml:"tags"`
	Examples    []string `yaml:"examples"`
	InputModes  []string `yaml:"input_modes"`
	OutputModes []string `yaml:"output_modes"`
}

// SessionConfig maps session-continuation flags for a CLI.
type SessionConfig struct {
	IDFlag       string `yaml:"id_flag"`
	ResumeFlag   string `yaml:"resume_flag"`
	ContinueFlag string `yaml:"continue_flag"`
	ForkFlag     string `yaml:"fork_flag"` // forks the session when resuming (harbor.fork)
}

// LimitsConfig bounds concurrent execution for an agent.
type LimitsConfig struct {
	MaxConcurrent int            `yaml:"max_concurrent"`
	Timeout       time.Duration  `yaml:"timeout"`      // total task timeout; 0 disables
	IdleTimeout   time.Duration  `yaml:"idle_timeout"` // fail if the CLI produces no output for this long; 0 disables
	MaxTurns      int            `yaml:"max_turns"`    // max turns (start + resumes) per task; 0 disables
	Breaker       *BreakerConfig `yaml:"breaker"`      // optional failure circuit breaker
}

// BreakerConfig configures the per-agent failure circuit breaker. It trips on a
// failure ratio over a sliding window of recent calls.
type BreakerConfig struct {
	Threshold         int           `yaml:"threshold"`           // recent calls tracked (window size)
	FailureRatio      float64       `yaml:"failure_ratio"`       // trip when failures/window >= this; default 0.5
	HalfOpenSuccesses int           `yaml:"half_open_successes"` // probe successes needed to close; default 1
	Cooldown          time.Duration `yaml:"cooldown"`            // open duration before a probe
	MaxCooldown       time.Duration `yaml:"max_cooldown"`        // backoff cap
}

// Spec maps the YAML agent configuration to its runtime form.
func (a AgentConfig) Spec() core.AgentSpec {
	spec := core.AgentSpec{
		ID: a.ID, DisplayName: a.DisplayName, Description: a.Description, Binary: a.Binary,
		Integration: a.Integration, Parser: a.Parser,
		ServerURL: a.ServerURL, ServerProtocol: a.ServerProtocol,
		Args: a.Args, Env: a.Env, AllowDangerous: a.AllowDangerous, SDKCaps: a.SDKCaps,
		OutputSchemaArgs: a.OutputSchemaArgs, FileArgs: a.FileArgs,
		ImageArgs: a.ImageArgs, InputImage: a.InputImage,
		Interaction: a.Interaction, Limits: a.Limits.Core(), Version: a.Version,
		Session: core.Session{
			IDFlag: a.Session.IDFlag, ResumeFlag: a.Session.ResumeFlag,
			ContinueFlag: a.Session.ContinueFlag, ForkFlag: a.Session.ForkFlag,
		},
	}
	if a.Regex != nil {
		spec.Regex = &core.Regex{Text: a.Regex.Text}
	}
	for _, s := range a.Skills {
		spec.Skills = append(spec.Skills, core.Skill{
			ID: s.ID, Name: s.Name, Description: s.Description,
			Tags: s.Tags, Examples: s.Examples, InputModes: s.InputModes, OutputModes: s.OutputModes,
		})
	}
	return spec
}
