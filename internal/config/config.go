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
	Version   int             `yaml:"version"`
	Server    ServerConfig    `yaml:"server"`
	Sandbox   SandboxConfig   `yaml:"sandbox"`
	Telemetry TelemetryConfig `yaml:"telemetry"`
	Storage   StorageConfig   `yaml:"storage"`
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

// Persistence backend for the task and session stores.
const (
	StoreSQLite = "sqlite"
)

// ServerConfig configures the HTTP server and discovery.
type ServerConfig struct {
	Listen                   string     `yaml:"listen"`
	BaseURL                  string     `yaml:"base_url"`
	AllowInsecure            bool       `yaml:"allow_insecure"`              // allow a non-loopback unauthenticated bind
	PushAllowPrivateNetworks bool       `yaml:"push_allow_private_networks"` // disable the push webhook SSRF guard
	StorePath                string     `yaml:"store_path"`                  // SQLite DB path for tasks (store: sqlite)
	SessionStoreDir          string     `yaml:"session_store_dir"`           // dir for per-agent session DBs
	Store                    string     `yaml:"store"`                       // persistence: "" (in-memory) | sqlite
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

// TLSConfig configures TLS for a listener or a client. For the server the
// certificate is what Harbor presents; for a client it is Harbor's client
// certificate (mutual TLS).
type TLSConfig struct {
	CertFile           string `yaml:"cert_file"`
	KeyFile            string `yaml:"key_file"`
	CAFile             string `yaml:"ca_file"`
	ClientAuth         bool   `yaml:"client_auth"`          // server: require a client certificate
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify"` // client: do not verify the peer
}

// StorageConfig configures the file-exchange data plane. An empty Backend
// disables it, and the card advertises no storage extension.
type StorageConfig struct {
	Backend        string           `yaml:"backend"`          // http | s3 | cos | local
	Prefix         string           `yaml:"prefix"`           // key prefix within the store
	InlineMaxBytes int64            `yaml:"inline_max_bytes"` // local inline threshold; 0 uses the default
	MediaTypes     []string         `yaml:"media_types"`      // allowed ingest media types; unset uses the defaults
	InboundMedia   string           `yaml:"inbound_media"`    // spool (default) | inline
	Retention      time.Duration    `yaml:"retention"`        // reap local objects older than this; 0 disables
	Download       DownloadConfig   `yaml:"download"`
	HTTP           HTTPStoreConfig  `yaml:"http"`
	S3             S3StoreConfig    `yaml:"s3"`
	COS            COSStoreConfig   `yaml:"cos"`
	Local          LocalStoreConfig `yaml:"local"`
}

// DownloadConfig bounds reading ingested objects back from the store.
type DownloadConfig struct {
	MaxBytes   int64                   `yaml:"max_bytes"`
	Timeout    time.Duration           `yaml:"timeout"`
	Credential []StoreCredentialConfig `yaml:"credentials"`
}

// StoreCredentialConfig is a credential for a private HTTPS store origin.
type StoreCredentialConfig struct {
	Host  string `yaml:"host"`
	Auth  string `yaml:"auth"` // bearer | basic
	Token string `yaml:"token"`
	User  string `yaml:"username"`
	Pass  string `yaml:"password"`
}

// HTTPStoreConfig configures the self-hosted HTTP store.
type HTTPStoreConfig struct {
	PutURL        string        `yaml:"put_url"`
	GetBase       string        `yaml:"get_base"`
	Auth          string        `yaml:"auth"` // none | basic | bearer (Harbor's own credential)
	Username      string        `yaml:"username"`
	Password      string        `yaml:"password"`
	Token         string        `yaml:"token"`
	TLS           TLSConfig     `yaml:"tls"`
	PresignURL    string        `yaml:"presign_url"`
	PresignSecret string        `yaml:"presign_secret"`
	PresignTTL    time.Duration `yaml:"presign_ttl"`
}

// S3StoreConfig configures an S3-compatible store (AWS S3, OSS, MinIO, COS via
// SigV4).
type S3StoreConfig struct {
	Endpoint     string        `yaml:"endpoint"`
	Region       string        `yaml:"region"`
	Bucket       string        `yaml:"bucket"`
	AccessKey    string        `yaml:"access_key"`
	SecretKey    string        `yaml:"secret_key"`
	SessionToken string        `yaml:"session_token"`
	PathStyle    bool          `yaml:"path_style"`
	TLS          TLSConfig     `yaml:"tls"`
	PresignTTL   time.Duration `yaml:"presign_ttl"`
}

// COSStoreConfig configures Tencent COS over its native REST API.
type COSStoreConfig struct {
	Region       string        `yaml:"region"`
	Bucket       string        `yaml:"bucket"`
	SecretID     string        `yaml:"secret_id"`
	SecretKey    string        `yaml:"secret_key"`
	SessionToken string        `yaml:"session_token"`
	Endpoint     string        `yaml:"endpoint"`
	TLS          TLSConfig     `yaml:"tls"`
	PresignTTL   time.Duration `yaml:"presign_ttl"`
}

// LocalStoreConfig configures a shared-directory store.
type LocalStoreConfig struct {
	Root         string `yaml:"root"`
	ServeBaseURL string `yaml:"serve_base_url"`
}

// RateLimitConfig configures a per-(user, agent) token-bucket rate limit on the
// invocation endpoints. A zero PerMinute disables it.
type RateLimitConfig struct {
	PerMinute int `yaml:"per_minute"` // sustained requests per minute per key
	Burst     int `yaml:"burst"`      // bucket capacity; default PerMinute
}

// AuthConfig configures bearer authentication for the JSON-RPC endpoints.
type AuthConfig struct {
	Mode   string   `yaml:"mode"` // none | bearer
	Tokens []string `yaml:"tokens"`
}

// SandboxConfig configures workspace and env constraints.
type SandboxConfig struct {
	Isolation     string           `yaml:"isolation"`      // shared | worktree | copy
	IsolationRoot string           `yaml:"isolation_root"` // base dir for worktree/copy (required for those)
	InheritEnv    *bool            `yaml:"inherit_env"`    // default true; false restricts to an allowlisted env
	CLIHome       string           `yaml:"cli_home"`       // optional root for the CLI's config/home
	StateDir      string           `yaml:"state_dir"`      // runtime state (orphan tracking); default under the temp dir
	Container     *ContainerConfig `yaml:"container"`      // optional strong isolation: run each CLI in a container
	// RequireContainerForDangerous refuses to start when an agent has
	// allow_dangerous while no container sandbox is configured (optional hardening).
	RequireContainerForDangerous bool `yaml:"require_container_for_dangerous"`
	// FileArtifacts emits a per-file FilePart artifact for each changed file, in
	// addition to the textual diff (optional; bounded per file).
	FileArtifacts bool `yaml:"file_artifacts"`
	// RawStreamDir, when set, writes each task's raw CLI stdout/stderr to a file
	// under it for replay/debugging (optional).
	RawStreamDir string `yaml:"raw_stream_dir"`
	// RawStreamCompress gzips the raw stream files.
	RawStreamCompress bool `yaml:"raw_stream_compress"`
}

// ContainerConfig runs each CLI inside a container for filesystem and network
// isolation. The task working directory (and the CLI home, when set) are
// bind-mounted at their host paths, so paths inside the container match the host
// and the CLI reaches exactly the task's files.
type ContainerConfig struct {
	Image   string `yaml:"image"`
	Network string `yaml:"network"` // none | bridge; default none
	Runtime string `yaml:"runtime"` // container runtime binary; default docker
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
	// ServerProtocolACP speaks ACP over the CLI's HTTP/SSE endpoint (codebuddy
	// --serve).
	ServerProtocolACP = "acp"
	// ServerProtocolACPStreamableHTTP speaks ACP over a dedicated streamable-http
	// endpoint (codebuddy --acp --acp-transport streamable-http); the client
	// protocol is identical to ServerProtocolACP.
	ServerProtocolACPStreamableHTTP = "acp-streamable-http"
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
	ServerProtocol string            `yaml:"server_protocol"` // "http" (default) | "acp" | "acp-streamable-http"
	Workdir        string            `yaml:"workdir"`         // project dir the CLI is started in (its project root)
	Args           []string          `yaml:"args"`
	Env            map[string]string `yaml:"env"`
	AllowDangerous bool              `yaml:"allow_dangerous"`
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
	// Description overrides the agent card's description. Empty uses the default
	// template ("Remote A2A wrapper for the <name> coding agent CLI").
	Description string `yaml:"description"`
	// Skills lists the skills advertised on the agent card. When empty, no
	// skills are advertised (the A2A "skills" field is still present).
	Skills  []SkillConfig `yaml:"skills"`
	Version string        `yaml:"version"` // optional card version override; skips probing
	// Interaction controls whether the CLI's permission/plan/question prompts are
	// surfaced as A2A input-required. auto (default) derives it from the
	// integration; interactive forces it on; non-interactive turns it off.
	Interaction string        `yaml:"interaction"`
	Session     SessionConfig `yaml:"session"`
	Limits      LimitsConfig  `yaml:"limits"`
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
	ForkFlag    string        // default session-fork flag (e.g. --fork-session)
	// OutputSchemaArgs is the default structured-output args for this CLI,
	// applied when the agent config leaves output_schema_args unset.
	OutputSchemaArgs []string
	// FileArgs is the default file-attachment args for this CLI, applied when
	// the agent config leaves file_args unset.
	FileArgs []string
	// ImageArgs is the default image-attachment args for this CLI, applied when
	// the agent config leaves image_args unset.
	ImageArgs []string
	// InputImage lists image MIME types the CLI accepts; nil means images are
	// not accepted. They are sent as native content blocks when the CLI's
	// integration supports structured input, otherwise attached via ImageArgs.
	InputImage []string
	// OutputImage lists image MIME types the CLI can produce (for example a
	// text-to-image model writing files into the workdir); nil means Harbor does
	// not advertise image output for this CLI.
	OutputImage []string
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
		Supported:   []Integration{IntegrationStreamJSON, IntegrationExec, IntegrationACP, IntegrationServer},
		InputImage:  []string{"image/png", "image/jpeg", "image/gif", "image/webp"},
		OutputImage: []string{"image/png", "image/jpeg", "image/gif", "image/webp"},
		ForkFlag:    "--fork-session"},
	{Binary: "claude", Integration: IntegrationStreamJSON, MinVersion: "2.1.292", VersionArgs: []string{"--version"},
		Supported: []Integration{IntegrationStreamJSON}, ForkFlag: "--fork-session"},
	{Binary: "opencode", Integration: IntegrationExec, MinVersion: "1.18.35", VersionArgs: []string{"--version"},
		Supported: []Integration{IntegrationExec, IntegrationACP, IntegrationServer}, ACPArgs: []string{"acp"},
		FileArgs: []string{"-f", "{{.File}}"}, ForkFlag: "--fork"},
	{Binary: "codex", Integration: IntegrationExec, Parser: "codex",
		MinVersion: "0.160.1", VersionArgs: []string{"--version"},
		Supported:        []Integration{IntegrationExec, IntegrationServer},
		OutputSchemaArgs: []string{"--output-schema", "{{.SchemaFile}}"},
		InputImage:       []string{"image/png", "image/jpeg", "image/gif", "image/webp"},
		ImageArgs:        []string{"-i", "{{.Image}}"}},
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

// Load reads and validates a config file, applying defaults.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	expanded, err := expandEnv(string(b))
	if err != nil {
		return nil, err
	}
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(expanded))
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

// expandEnv substitutes ${NAME} and ${NAME:-default} from the process
// environment; a literal "${" is written "$${".
func expandEnv(s string) ([]byte, error) {
	var out bytes.Buffer
	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], "$${"):
			out.WriteString("${")
			i += 3
		case strings.HasPrefix(s[i:], "${"):
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				return nil, fmt.Errorf("config: unterminated ${")
			}
			out.WriteString(lookupEnv(s[i+2 : i+2+end]))
			i += 2 + end + 1
		default:
			out.WriteByte(s[i])
			i++
		}
	}
	return out.Bytes(), nil
}

// lookupEnv resolves a ${NAME} or ${NAME:-default} expression.
func lookupEnv(expr string) string {
	name, def, hasDefault := strings.Cut(expr, ":-")
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	if hasDefault {
		return def
	}
	return ""
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
	if c.Telemetry.ServiceName == "" {
		c.Telemetry.ServiceName = "harbor"
	}
	if c.Sandbox.Isolation == "" {
		c.Sandbox.Isolation = "shared"
	}
	if c.Sandbox.StateDir == "" {
		c.Sandbox.StateDir = filepath.Join(os.TempDir(), "harbor")
	}
	if ct := c.Sandbox.Container; ct != nil {
		if ct.Network == "" {
			ct.Network = "none"
		}
		if ct.Runtime == "" {
			ct.Runtime = "docker"
		}
	}
	c.applyStorageDefaults()
	for i := range c.Agents {
		a := &c.Agents[i]
		if a.Interaction == "" {
			a.Interaction = "auto"
		}
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
			if a.Session.ForkFlag == "" {
				a.Session.ForkFlag = p.ForkFlag
			}
			if len(a.OutputSchemaArgs) == 0 {
				a.OutputSchemaArgs = append([]string(nil), p.OutputSchemaArgs...)
			}
			if len(a.FileArgs) == 0 {
				a.FileArgs = append([]string(nil), p.FileArgs...)
			}
			if len(a.ImageArgs) == 0 {
				a.ImageArgs = append([]string(nil), p.ImageArgs...)
			}
			if len(a.InputImage) == 0 {
				a.InputImage = append([]string(nil), p.InputImage...)
			}
		}
	}
}

// defaultMediaTypes is the ingest allowlist applied when media_types is unset.
func defaultMediaTypes() []string {
	return []string{"text/*", "application/json", "application/xml", "image/*"}
}

// applyStorageDefaults fills the storage data-plane defaults.
func (c *Config) applyStorageDefaults() {
	s := &c.Storage
	if s.InlineMaxBytes == 0 {
		s.InlineMaxBytes = 256 << 10
	}
	if s.InboundMedia == "" {
		s.InboundMedia = "spool"
	}
	if len(s.MediaTypes) == 0 {
		s.MediaTypes = defaultMediaTypes()
	}
	if s.Download.MaxBytes == 0 {
		s.Download.MaxBytes = 256 << 20
	}
	if s.HTTP.PresignTTL == 0 {
		s.HTTP.PresignTTL = 15 * time.Minute
	}
	if s.S3.PresignTTL == 0 {
		s.S3.PresignTTL = 15 * time.Minute
	}
	if s.COS.PresignTTL == 0 {
		s.COS.PresignTTL = 15 * time.Minute
	}
}

// validateStorage validates the storage data plane. An empty backend disables
// it and requires nothing else.
func (c *Config) validateStorage() error {
	s := c.Storage
	if s.Backend == "" {
		return nil
	}
	switch s.Backend {
	case "http", "s3", "cos", "local":
	default:
		return fmt.Errorf("storage.backend must be one of http, s3, cos, local; got %q", s.Backend)
	}
	switch s.InboundMedia {
	case "spool", "inline":
	default:
		return fmt.Errorf("storage.inbound_media must be one of spool, inline; got %q", s.InboundMedia)
	}
	switch s.Backend {
	case "http":
		if s.HTTP.GetBase == "" {
			return fmt.Errorf("storage.http.get_base is required for the http backend")
		}
	case "s3":
		if s.S3.Bucket == "" || s.S3.Region == "" {
			return fmt.Errorf("storage.s3.bucket and storage.s3.region are required for the s3 backend")
		}
	case "cos":
		if s.COS.Bucket == "" || s.COS.Region == "" {
			return fmt.Errorf("storage.cos.bucket and storage.cos.region are required for the cos backend")
		}
	case "local":
		if s.Local.Root == "" {
			return fmt.Errorf("storage.local.root is required for the local backend")
		}
	}
	return nil
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
	if err := c.validateStorage(); err != nil {
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
	switch c.Server.Auth.Mode {
	case "none", "bearer":
	default:
		return fmt.Errorf("server.auth.mode must be one of none, bearer; got %q", c.Server.Auth.Mode)
	}
	switch c.Server.Store {
	case "", StoreSQLite:
	default:
		return fmt.Errorf("server.store must be sqlite; got %q", c.Server.Store)
	}
	if c.Server.Store == StoreSQLite && c.Server.StorePath == "" && c.Server.SessionStoreDir == "" {
		return fmt.Errorf("server.store: sqlite requires store_path or session_store_dir")
	}
	return nil
}

func (c *Config) validateSandbox() error {
	switch c.Sandbox.Isolation {
	case "shared":
	case "worktree", "copy":
		if c.Sandbox.IsolationRoot == "" {
			return fmt.Errorf("sandbox.isolation_root is required for isolation %q", c.Sandbox.Isolation)
		}
	default:
		return fmt.Errorf("sandbox.isolation must be one of shared, worktree, copy; got %q", c.Sandbox.Isolation)
	}
	if ct := c.Sandbox.Container; ct != nil {
		if ct.Image == "" {
			return fmt.Errorf("sandbox.container.image is required")
		}
		switch ct.Network {
		case "none", "bridge":
		default:
			return fmt.Errorf("sandbox.container.network must be one of none, bridge; got %q", ct.Network)
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
	if a.Workdir != "" {
		info, err := os.Stat(a.Workdir)
		if err != nil || !info.IsDir() {
			return fmt.Errorf("agent %q: workdir %q is not a directory", a.ID, a.Workdir)
		}
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
	case "", ServerProtocolHTTP, ServerProtocolACP, ServerProtocolACPStreamableHTTP:
	default:
		return fmt.Errorf(
			"agent %q: server_protocol must be one of %q, %q, %q; got %q",
			a.ID, ServerProtocolHTTP, ServerProtocolACP, ServerProtocolACPStreamableHTTP, a.ServerProtocol,
		)
	}
	switch a.Interaction {
	case "auto", "interactive", "non-interactive":
	default:
		return fmt.Errorf(
			"agent %q: interaction must be one of auto, interactive, non-interactive; got %q",
			a.ID, a.Interaction,
		)
	}
	if a.Interaction == "interactive" &&
		a.Integration != IntegrationStreamJSON && a.Integration != IntegrationACP {
		return fmt.Errorf(
			"agent %q: interaction \"interactive\" requires a pausable integration (%q or %q); got %q",
			a.ID, IntegrationStreamJSON, IntegrationACP, a.Integration,
		)
	}
	for j, s := range a.Skills {
		if s.ID == "" || s.Name == "" || s.Description == "" {
			return fmt.Errorf("agent %q: skills[%d] requires id, name, and description", a.ID, j)
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
