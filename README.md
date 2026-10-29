# Harbor

Expose coding-agent CLIs (`codebuddy`, `claude`, `opencode`, `codex`, and others)
as remote [A2A](https://a2a-protocol.org) v1.0 agents. Each CLI is served behind a
standard AgentCard with JSON-RPC and HTTP+JSON (REST) endpoints, bearer auth,
discovery, and a config-driven adapter layer.

- Design: [docs/DESIGN.md](docs/DESIGN.md)
- Engineering conventions: [AGENTS.md](AGENTS.md)

## Build and run

```sh
make build
./bin/harbor --config examples/agents.yaml
```

Requires Go 1.27.1+ (see `go.mod`). `make lint` additionally needs a
`golangci-lint` built with a Go toolchain at least as new as the module's `go`
directive. `make test` runs the suite with `-race`. `./bin/harbor --version`
prints the stamped build version; `make version` prints the version the next
build would use, and `make release` packages a release archive from a clean,
tagged HEAD.

## Adapters

An `agents[]` entry binds one CLI (`binary`) to exactly one **integration**
(adapter), chosen statically at configuration time. The four integrations are:

| Integration   | How the CLI is driven                                                                 |
|---------------|---------------------------------------------------------------------------------------|
| `stream-json` | Prompt (and later answers) over stdin as NDJSON; events streamed back on stdout. Gives token streaming, tool traces, and native interaction. |
| `exec`        | One-shot invocation; the prompt is substituted into `args` and the CLI's stdout is parsed (`text` by default; `codex`, `regex`, or `stream-json` via `parser`). |
| `acp`         | Agent Client Protocol over the CLI's stdio (`<cli> acp`, `codebuddy --acp`).          |
| `server`      | Harbor proxies the CLI's own HTTP/app server (opencode `serve`, codebuddy `--serve`, codex `app-server`) and reuses server-side sessions. |

Known CLIs get defaults (integration, parser, ACP and session-fork flags,
attachment and structured-output args, and a minimum supported version) from a
built-in profile; unknown binaries need an explicit `integration`.

### Supported adapters per CLI

| CLI         | `stream-json` | `exec` | `acp` | `server` | Highlights |
|-------------|:---:|:---:|:---:|:---:|---|
| **codebuddy** | default | ✅ | ✅ (`--acp`) | ✅ (`--serve`, `--acp-transport`) | image input/output (png/jpeg/gif/webp); `sdk_capabilities` for AskUserQuestion + elicitation forms; server exposes a REST API and ACP-over-HTTP |
| **claude**  | default | — | — | — | stream-json only; images are not accepted on this channel |
| **opencode**| — | default | ✅ (`acp`) | ✅ (`serve`) | file attachments via `-f {{.File}}` |
| **codex**   | — | default | — | ✅ (`app-server`) | structured output (`--output-schema`); image attachments via `-i` |
| **generic** | ✅ | ✅ | ✅ | — | any binary, with an explicit `integration` and `parser` (`text`/`stream-json`/`codex`/`regex`); `server` is implemented only for opencode/codebuddy/codex |

### codebuddy

```yaml
- id: codebuddy
  binary: codebuddy
  integration: stream-json
  args: ["-p", "--input-format", "stream-json", "--output-format", "stream-json", "--include-partial-messages"]
  sdk_capabilities: true   # let the agent ask via AskUserQuestion / elicitation forms
  session: { id_flag: "--session-id", resume_flag: "--resume", continue_flag: "-c" }

- id: codebuddy-acp           # ACP over stdio
  binary: codebuddy
  integration: acp
  args: ["--acp"]

- id: codebuddy-server        # `codebuddy --serve`, REST API (turn-level)
  binary: codebuddy
  integration: server
  # server_protocol: http    # default

- id: codebuddy-server-acp    # `codebuddy --serve`, ACP over HTTP (`/api/v1/acp`)
  binary: codebuddy
  integration: server
  server_protocol: acp

- id: codebuddy-streamable    # `codebuddy --acp --acp-transport streamable-http`
  binary: codebuddy
  integration: server
  server_protocol: acp-streamable-http
```

### claude

```yaml
- id: claude
  binary: claude
  integration: stream-json
  args: ["-p", "--input-format", "stream-json", "--output-format", "stream-json", "--include-partial-messages", "--verbose"]
  session: { id_flag: "--session-id", resume_flag: "--resume", continue_flag: "--continue" }
```

### opencode

```yaml
- id: opencode
  binary: opencode
  integration: exec
  args: ["run", "{{.Prompt}}", "-m", "{{.Model}}"]
  session: { id_flag: "--session", continue_flag: "--continue" }

- id: opencode-acp
  binary: opencode
  integration: acp
  args: ["acp"]              # default from the opencode profile, so it may be omitted

- id: opencode-server
  binary: opencode
  integration: server
  # server_url: "http://127.0.0.1:4096"   # connect to an existing server instead
```

### codex

```yaml
- id: codex
  binary: codex
  integration: exec
  args: ["exec", "--json", "--skip-git-repo-check", "{{.Prompt}}"]
  # output_schema_args and image_args default from the codex profile
  # (--output-schema {{.SchemaFile}}, -i {{.Image}})

- id: codex-server
  binary: codex
  integration: server       # `codex app-server`
```

### Generic CLI (configuration only)

```yaml
- id: my-cli
  binary: my-cli
  integration: exec
  parser: regex
  regex: { text: '^OUT:\s*(.+)$' }
  args: ["--prompt", "{{.Prompt}}"]
```

## Endpoints

For an agent with id `<id>`:

| Path | Purpose |
|---|---|
| `/agents/<id>/` | JSON-RPC (A2A) binding |
| `/agents/<id>/rest/` | HTTP+JSON (A2A REST) binding |
| `/agents/<id>/.well-known/agent-card.json` | that agent's card |
| `/.well-known/agent-card.json` | first available agent's card |
| `/.well-known/agents.json` | aggregate discovery: a JSON array of all cards (used by codebuddy) |
| `/healthz`, `/readyz`, `/metrics` | liveness, readiness (JSON), Prometheus metrics |

Invocation requests (JSON-RPC and REST) must carry `A2A-Version: 1.0`, either as
a header or as the `A2A-Version` query parameter. `/readyz` returns
`{"ready": bool, "agents": {...}}` and answers `503` when no agent is ready; an
agent is ready when its circuit breaker is closed, and CLI auth state is reported
separately without affecting readiness.

With `server.auth.mode: bearer`, the invocation endpoints (JSON-RPC, REST) require
`Authorization: Bearer <token>`; cards, discovery, health, and metrics stay public.
A non-loopback `server.listen` with `auth.mode: none` refuses to start unless
`server.allow_insecure: true`.

## Configuration

`examples/agents.yaml` is a commented starting point; the tables below are the
full reference. Durations use Go syntax (`30s`, `30m`, `1h`).

### Environment expansion

Values may reference the process environment: `$NAME`, `${NAME}`,
`${NAME:-default}` and `${NAME:=default}`. Variable names start with a letter or
underscore, so `$1` and `${1}` are left as-is. A bare `$` that starts a valid name
is always a reference, so write a literal dollar as `$$` (for example `$$schema`).
A reference must name a set variable unless it has a non-empty default;
otherwise loading fails. A variable that is set but empty is used as-is.
Expansion covers the whole file, comments included, so a reference written in a
comment must resolve too (or be escaped with `$$`). Quote
the YAML scalar holding a reference (for example `token: "${HARBOR_TOKEN}"`) so
the substituted value cannot introduce new YAML structure. Which quote to use
depends on the value: double quotes interpret escapes, so single-quote values
containing backslashes (Windows paths such as `'C:\work'`) or `"`, and double an
embedded `'` inside single quotes. An unquoted value containing `: ` or ` #` is
rejected or truncated, and a newline in a value is folded to a space. Names are
matched exactly, including on Windows, and `:=` behaves like `:-`: nothing is
assigned back to the environment.

A default value must be a literal; a variable inside a default
(`${NAME:-$HOME/bin}`) is not expanded, so keep such defaults literal or resolve
them in the surrounding environment. `${NAME:?message}` is not recognized as an
error form: with `NAME` set it yields its value, otherwise the load fails as for
any unset reference.

### Top level

| key | type | required | default | meaning |
|---|---|---|---|---|
| `version` | int | no | — | config schema version (currently `1`) |
| `server` | object | **yes** | — | HTTP server, auth, discovery, limits (requires `listen`) |
| `sandbox` | object | **yes** | — | workspace/env/container constraints (requires `state_dir`) |
| `telemetry` | object | no | — | OTLP traces/metrics export |
| `plugins` | string[] | no | — | Go plugin `.so` paths, each exporting `Register()` (cgo only) |
| `agents` | object[] | **yes** | — | at least one agent entry |

### `server`

| key | type | required | default | meaning |
|---|---|---|---|---|
| `listen` | string | **yes** | — | bind address (`host:port`) |
| `base_url` | string | no | — | externally reachable URL used in agent cards |
| `allow_insecure` | bool | no | `false` | allow a non-loopback bind with `auth.mode: none` |
| `push_allow_private_networks` | bool | no | `false` | disable the SSRF guard on push webhooks |
| `store` | string | no | — | persistence backend: unset = in-memory, `sqlite` = durable for the paths that are set (survives restart) |
| `store_path` | string | no | — | with `store: sqlite`, SQLite DB path for tasks (tasks stay in-memory without it) |
| `session_store_dir` | string | no | — | with `store: sqlite`, dir for per-agent session DBs (`<dir>/<id>.db`); sessions stay in-memory without it |
| `agent_inactivity_timeout` | duration | no | `10m` | fail a task if the server sees no event from the CLI for this long; `0` disables. A silent CLI is kept alive only when the agent also sets `limits.timeout` or `limits.idle_timeout` |
| `max_request_bytes` | int | no | `0` (off) | maximum incoming JSON-RPC/REST request body; larger requests fail with `413` before being buffered |
| `auth.mode` | string | no | `none` | `none` \| `bearer` |
| `auth.tokens` | string[] | no | — | bearer tokens (required when `mode: bearer`) |
| `cors_allowed_origins` | string[] | no | — | allowed CORS origins |
| `rate_limit.per_minute` | int | no | `0` (off) | sustained requests/min per (caller, agent) |
| `rate_limit.burst` | int | no | `per_minute` | token-bucket capacity |
| `tls.cert_file` | string | no | — | PEM certificate; setting it enables TLS on the listener |
| `tls.key_file` | string | with `tls.cert_file` | — | PEM private key |
| `tls.ca_file` | string | no | — | PEM CA bundle used to verify client certificates |
| `tls.client_auth` | bool | no | `false` | require and verify a client certificate (mTLS) |

`store: sqlite` requires at least one of `store_path` and `session_store_dir`.
Setting `tls.cert_file` enables TLS; `tls.key_file` is then required. With
`tls.client_auth`, clients must present a certificate verified against
`tls.ca_file` when set.

### `sandbox`

| key | type | required | default | meaning |
|---|---|---|---|---|
| `isolation` | string | no | `shared` | `shared` \| `worktree` \| `copy` \| `overlayfs` (per-context working views) |
| `isolation_root` | string | for `worktree`/`copy`/`overlayfs` | — | base dir for per-context views |
| `copy_strategy` | string | no | `full` (copy only) | copy isolation implementation: `full` or `cow` (copy-on-write clone) |
| `inherit_env` | bool | no | `true` | inherit the parent env; `false` restricts to an allowlist |
| `cli_home` | string | no | — | redirect the CLI's config/home here |
| `state_dir` | string | **yes** | — | writable dir for CLI process records used for orphan cleanup; use a private per-user dir when several instances or users share a host |
| `container` | object | no | — | run each CLI in a container (filesystem isolation; network and extra flags are deployer-owned) |
| `require_container_for_dangerous` | bool | no | `false` | refuse `allow_dangerous` agents without a container |
| `raw_stream_dir` | string | no | — | write each task's raw stdout/stderr to this dir |
| `raw_stream_compress` | bool | no | `false` | gzip the raw-stream files |

With `sandbox.container`, `shared` and `worktree` isolation mount
`agents[].workdir` (a git worktree keeps its metadata in the owning repository);
`copy` is self-contained and mounts only `isolation_root` (plus `cli_home`), so
the original workdir is not visible to the agent. `overlayfs` cannot be combined
with `sandbox.container`. For `copy` and `overlayfs`, `isolation_root` and the
workdir must not contain each other; otherwise a copy or overlay would include
its own destination.

`copy_strategy` selects how `copy` materializes each context. `full` is
portable: Go file APIs copy the tree but do not preserve ownership, ACLs,
xattrs, or mtimes, and symlinks need privileges on Windows. `cow` clones file
data with FICLONE (Linux) or clonefile (macOS), so the view costs little until
it is written; startup checks that the workdir and `isolation_root` share a
filesystem and probes the `isolation_root` clone support, failing when the
mechanism is unavailable. `overlayfs` is a Linux-only mount view with
read-only lower layers and per-context writable upper layers. It requires
`CAP_SYS_ADMIN` in Harbor's mount namespace (for example, run Harbor in a user
namespace that grants it) and cannot be combined with `sandbox.container`.

For `worktree`, `copy`, and `overlayfs`, Harbor takes an exclusive lock on
`isolation_root`, so a second instance fails to start. At startup, after taking
the lock, it removes stale copy staging directories and unmounts stale overlayfs
views; on shutdown it detaches active overlayfs mounts while keeping their upper
layers for resumed contexts.

Isolation directories are keyed by the agent, workdir, isolation mode, and copy
strategy. Changing any of them starts a new per-context tree; the previous
directory stays under `isolation_root` and is not reused. Trees created by
versions that keyed only by context ID are likewise not reused.

`sandbox.container`:

| key | type | required | default | meaning |
|---|---|---|---|---|
| `image` | string | **yes** | — | image providing the CLI |
| `runtime` | string | **yes** | — | container runtime with a Docker-compatible CLI: `docker` \| `podman` \| `nerdctl` |
| `extra_args` | string[] | no | — | extra runtime flags passed verbatim before the image; deployer-owned and may weaken isolation |

Container settings are deployer-owned: Harbor passes `image`, `runtime`, and
`extra_args` through without constraining them. The sandbox bounds a
prompt-injected agent under those choices, not the deployer who wrote them. The
runtime's default network applies unless `extra_args` overrides it. Runtime
availability is platform-specific: `docker` runs natively on Linux and through
Docker Desktop on macOS; `podman` natively on Linux and through `podman machine`
on macOS; `nerdctl` requires containerd and is Linux-only. On Windows, run
Harbor in WSL: native Windows cannot bind-mount host paths unchanged into a
Linux container, so startup rejects the container sandbox there. At startup
Harbor checks the platform, probes `<runtime> info`, and fails startup when the
binary or daemon/VM is unavailable.

### `telemetry`

| key | type | required | default | meaning |
|---|---|---|---|---|
| `endpoint` | string | no | — | OTLP/gRPC collector `host:port` (traces + metrics); empty disables |
| `insecure` | bool | no | `false` | send without TLS |
| `service_name` | string | no | `harbor` | resource `service.name` |

### `agents[]` (general)

| key | type | required | default | meaning |
|---|---|---|---|---|
| `id` | string | **yes** | — | agent id (route/card/skill base) |
| `binary` | string | **yes** | — | CLI executable (name or path) |
| `integration` | string | for unknown binaries | profile default | `exec` \| `stream-json` \| `acp` \| `server` |
| `card` | object | **yes** | — | A2A AgentCard configuration; see `agents[].card` |
| `cli_version` | string | no | probed | CLI version override; not the A2A card version |
| `workdir` | string | **yes** | — | project dir the CLI is started in (its project root); must exist; `"."` means Harbor's working directory and is stored absolute |
| `args` | string[] | exec/acp rules | — | launch args. `exec` must contain `{{.Prompt}}`; `acp` needs the ACP subcommand; `server` launchers generate their own args |
| `env` | map | no | — | extra environment for the CLI |
| `parser` | string | no | profile (`text` when unknown) | `exec` framing: `text` \| `stream-json` \| `codex` \| `regex` |
| `regex.text` | string | for `parser: regex` | — | line regex with one capture group |
| `server_url` | string | no | — | `server`: connect to an existing opencode/codebuddy instance; codex always spawns `codex app-server` |
| `server_protocol` | string | no | `http` | `server`: `http` \| `acp` \| `acp-streamable-http` (`acp` forms are codebuddy only) |
| `allow_dangerous` | bool | no | `false` | `stream-json`: append `--dangerously-skip-permissions`; other integrations express this in their own args |
| `interaction` | string | no | `auto` | `auto` \| `interactive` \| `non-interactive`: whether CLI permission/plan/question prompts surface as A2A `input-required` |
| `sdk_capabilities` | bool | no | `false` | `stream-json`: declare `askUserQuestion` before the first prompt |
| `output_schema_args` | string[] | no | profile | `exec` structured-output args (`{{.SchemaFile}}`/`{{.Schema}}`) |
| `file_args` | string[] | no | profile | `exec`: attach files via args (`{{.File}}`) |
| `image_args` | string[] | no | profile | `exec`: attach images via args (`{{.Image}}`) |
| `input_image` | string[] | no | profile | accepted image MIME types; sent as native blocks on `stream-json`, via `image_args` on `exec` |

`agents[].session` (optional; the session-fork flag defaults from the CLI profile):

| key | type | meaning |
|---|---|---|
| `id_flag` | string | flag to set a session id |
| `resume_flag` | string | flag to resume a session (`--resume`) |
| `continue_flag` | string | flag to continue the latest session |
| `fork_flag` | string | flag to fork a session (used when `harbor.fork` is set) |

`agents[].limits` (all optional; `0` disables a bound or leaves the breaker default):

| key | type | meaning |
|---|---|---|
| `max_concurrent` | int | concurrent tasks for this agent (unbounded when `0`) |
| `timeout` | duration | total task timeout |
| `idle_timeout` | duration | fail a task with no events for this long |
| `max_turns` | int | max turns (start + resumes) per task |
| `breaker.threshold` | int | recent calls tracked (window size); `0` disables the breaker |
| `breaker.failure_ratio` | float | trip when the failure ratio in the window reaches this (default `0.5`) |
| `breaker.half_open_successes` | int | consecutive probe successes that close the breaker (default `1`) |
| `breaker.cooldown` | duration | open duration before a probe (also the probe deadline; default `30s`) |
| `breaker.max_cooldown` | duration | backoff cap (default `10m`) |

### `agents[].card`

The AgentCard is configured separately from how the CLI is run.

| key | type | required | meaning |
|---|---|---|---|
| `name` | string | **yes** | human-readable card name |
| `description` | string | **yes** | card description |
| `version` | string | no | card version; defaults to the probed CLI version |
| `icon_url` | string | no | card icon URL |
| `documentation_url` | string | no | card documentation URL |
| `provider.organization` | string | with `provider` | service provider name |
| `provider.url` | string | with `provider` | service provider URL |
| `skills` | list | no | advertised skills; see `agents[].card.skills[]` |
| `extensions` | list | no | protocol extensions; see `agents[].card.extensions[]` |
| `signing` | object | no | JWS signing; see `agents[].card.signing` |

### `agents[].card.skills[]`

Optional; when omitted, the card advertises an empty skills list.

| key | type | required | meaning |
|---|---|---|---|
| `id` | string | **yes** | unique skill id |
| `name` | string | **yes** | human-readable name |
| `description` | string | **yes** | what the skill does |
| `tags` | string[] | no | keywords (emitted as `[]` when unset) |
| `examples` | string[] | no | example prompts |
| `input_modes` | string[] | no | per-skill input MIME override |
| `output_modes` | string[] | no | per-skill output MIME override |

### `agents[].card.extensions[]`

Optional; additional protocol extensions declared on the card. A configured
extension with Harbor's interaction extension URI replaces the built-in
declaration.

| key | type | required | meaning |
|---|---|---|---|
| `uri` | string | **yes** | unique extension URI |
| `description` | string | no | human-readable description |
| `params` | map | no | extension-specific configuration |
| `required` | bool | no | whether clients must understand the extension |

### `agents[].card.signing`

Optional; signs the final AgentCard with JWS (RFC 7515). The key is read at
startup; a signing failure disables that agent.

| key | type | required | meaning |
|---|---|---|---|
| `key_file` | string | **yes** | PEM private key: PKCS#8, EC, or PKCS#1 |
| `key_id` | string | **yes** | JWS `kid` |
| `algorithm` | string | no | JWS `alg`; inferred from the key when empty |
| `jwks_url` | string | no | optional JWS `jku` |

```yaml
- id: codebuddy
  binary: codebuddy
  integration: stream-json
  args: ["-p"]
  card:
    name: "CodeBuddy"
    description: "CodeBuddy is a helpful software-development assistant."
    skills:
      - id: code-review
        name: Code review
        description: Review a code change for bugs, style, and quality issues
        tags: ["review", "quality"]
```

### Restricting an agent's tools

Harbor passes tool/permission flags through `agents[].args` (or `agents[].env`);
each CLI enforces its own rules. For a read-only codebuddy, pre-approve the
read-only tools and deny the rest without prompting:

```yaml
- id: codebuddy-readonly
  binary: codebuddy
  integration: stream-json
  args: ["-p", "--input-format", "stream-json", "--output-format", "stream-json",
         "--permission-mode", "dontAsk",
         "--allowedTools", "Read", "Glob", "Grep", "WebFetch", "WebSearch", "LSP"]
```

`--allowedTools` alone only pre-approves (other tools still prompt); pairing it
with `--permission-mode dontAsk` denies them. Other CLIs: claude
`--allowedTools`/`--disallowedTools`/`--permission-mode`; opencode `permission`
rules in config (`OPENCODE_CONFIG_CONTENT` or a project `opencode.json`); codex
`-s/--sandbox` and `-a/--ask-for-approval`.

## Testing with a codebuddy client

Start Harbor, then point codebuddy's A2A client at its aggregate discovery
endpoint. The URL must match your `server.base_url`; the bundled example listens
on `127.0.0.1:8080`:

```sh
make run            # builds and runs on the configured server.listen

# in another shell
CODEBUDDY_A2A_OUTBOUND_ENABLED=1 \
CODEBUDDY_REMOTE_AGENTS_URL=http://127.0.0.1:8080/.well-known/agents.json \
codebuddy
```

You can also inspect the agents directly:

```sh
curl -s http://127.0.0.1:8080/.well-known/agents.json | jq '.[].name'
curl -s http://127.0.0.1:8080/agents/codebuddy/.well-known/agent-card.json | jq
```

## Extending

- **New output format (no code change):** use `exec` with `parser: regex`.
- **New parser (Go):** add a `core.ParserKind` and a builder to the registry in
  `internal/parser/registry.go`.
- **New CLI adapter (Go):** register a `cli.Descriptor` with `cli.Register` for
  a new binary, or attach a launcher builder with `cli.AttachBuilder` for a known
  binary, from a compile-time `init()`; see docs/DESIGN.md §8.
- **Runtime adapter plugin:** build a Go plugin exporting `Register()` and list
  it under `plugins:` (optional, requires cgo on linux/darwin/freebsd).
