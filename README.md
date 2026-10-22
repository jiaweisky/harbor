# Harbor

Expose coding-agent CLIs (`codebuddy`, `claude`, `opencode`, `codex`, and others)
as remote [A2A](https://a2a-protocol.org) v1.0 agents. Each CLI is served behind a
standard AgentCard and JSON-RPC endpoint, with bearer auth, discovery, and a
config-driven adapter layer.

- Design: [docs/DESIGN.md](docs/DESIGN.md)
- Engineering conventions: [AGENTS.md](AGENTS.md)

## Build and run

```sh
make build
./bin/harbor --config examples/agents.yaml
```

Requires Go 1.27+ (see `go.mod`). `make lint` additionally needs a
`golangci-lint` built with a Go toolchain at least as new as the module's `go`
directive. `make test` runs the suite with `-race`.

## Adapters

An `agents[]` entry binds one CLI (`binary`) to exactly one **integration**
(adapter), chosen statically at configuration time. The four integrations are:

| Integration   | How the CLI is driven                                                                 |
|---------------|---------------------------------------------------------------------------------------|
| `stream-json` | Prompt (and later answers) over stdin as NDJSON; events streamed back on stdout. Gives token streaming, tool traces, and native interaction. |
| `exec`        | One-shot invocation; the prompt is substituted into `args` and the CLI's stdout is parsed (`text` by default; `codex`, `regex`, or `stream-json` via `parser`). |
| `acp`         | Agent Client Protocol over the CLI's stdio (`<cli> acp`, `codebuddy --acp`).          |
| `server`      | Harbor proxies the CLI's own HTTP/app server (opencode `serve`, codebuddy `--serve`, codex `app-server`) and reuses server-side sessions. |

Known CLIs get sensible defaults (integration, args, version check, session and
attachment flags) from a built-in profile; unknown binaries need an explicit
`integration`.

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
| `/.well-known/agent-card.json` | first agent's card |
| `/.well-known/agents.json` | aggregate discovery: a JSON array of all cards (used by codebuddy) |
| `/healthz`, `/readyz`, `/metrics` | liveness, readiness (JSON), Prometheus metrics |

With `server.auth.mode: bearer`, the invocation endpoints (JSON-RPC, REST) require
`Authorization: Bearer <token>`; cards and discovery stay public. A non-loopback
`server.listen` with `auth.mode: none` refuses to start unless
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
| `server` | object | no | — | HTTP server, auth, discovery, limits |
| `sandbox` | object | no | — | workspace/env/container constraints |
| `telemetry` | object | no | — | OTLP traces/metrics export |
| `plugins` | string[] | no | — | Go plugin `.so` paths, each exporting `Register()` |
| `agents` | object[] | **yes** | — | at least one agent entry |

### `server`

| key | type | required | default | meaning |
|---|---|---|---|---|
| `listen` | string | no | `127.0.0.1:8080` | bind address |
| `base_url` | string | no | — | externally reachable URL used in agent cards |
| `allow_insecure` | bool | no | `false` | allow a non-loopback bind with `auth.mode: none` |
| `push_allow_private_networks` | bool | no | `false` | disable the SSRF guard on push webhooks |
| `store` | string | no | — | persistence backend: unset = in-memory, `sqlite` = durable (survives restart) |
| `store_path` | string | for `store: sqlite` | — | SQLite DB path for tasks |
| `session_store_dir` | string | no | — | dir for per-agent session DBs (`<dir>/<id>.db`) |
| `agent_inactivity_timeout` | duration | no | `10m` | fail a task if the server sees no event from the CLI for this long; `0` disables. A silent CLI is kept alive only when the agent also sets `limits.timeout` or `limits.idle_timeout` |
| `auth.mode` | string | no | `none` | `none` \| `bearer` |
| `auth.tokens` | string[] | no | — | bearer tokens (required when `mode: bearer`) |
| `cors_allowed_origins` | string[] | no | — | allowed CORS origins |
| `rate_limit.per_minute` | int | no | `0` (off) | sustained requests/min per (caller, agent) |
| `rate_limit.burst` | int | no | `per_minute` | token-bucket capacity |

### `sandbox`

| key | type | required | default | meaning |
|---|---|---|---|---|
| `isolation` | string | no | `shared` | `shared` \| `worktree` \| `copy` (per-context working copies) |
| `isolation_root` | string | for `worktree`/`copy` | — | base dir for per-context copies |
| `inherit_env` | bool | no | `true` | inherit the parent env; `false` restricts to an allowlist |
| `cli_home` | string | no | — | redirect the CLI's config/home here |
| `state_dir` | string | no | temp dir `harbor` | runtime state (orphan-process records) |
| `container` | object | no | — | run each CLI in a container (strong isolation) |
| `require_container_for_dangerous` | bool | no | `false` | refuse `allow_dangerous` agents without a container |
| `non_image_file_artifacts` | bool | no | `false` | emit a `FilePart` per changed non-image file |
| `artifact_inline_max_bytes` | int | no | `268435456` | max bytes of one changed file inlined into a file or image artifact |
| `diff_inline_max_bytes` | int | no | `33554432` | max captured diff bytes before truncation |
| `non_git_change_globs` | string[] | no | — | relative glob patterns enabling change capture outside git; empty disables it (and its artifacts) |
| `raw_stream_dir` | string | no | — | write each task's raw stdout/stderr to this dir |
| `raw_stream_compress` | bool | no | `false` | gzip the raw-stream files |

`sandbox.file_artifacts` was renamed to `sandbox.non_image_file_artifacts`, and
`sandbox.file_artifact_max_bytes` to `sandbox.artifact_inline_max_bytes`; the old
keys are rejected.

Change capture outside git is opt-in: configure `sandbox.non_git_change_globs`
with relative patterns such as `src/**` or `*.md`. The baseline only hashes
matching files; unset or empty disables diffs and file/image artifacts for
non-git workdirs. Workdirs inside a git repository keep using git.

`sandbox.container`:

| key | type | required | default | meaning |
|---|---|---|---|---|
| `image` | string | **yes** | — | image providing the CLI |
| `network` | string | no | `none` | `none` \| `bridge` |
| `runtime` | string | no | `docker` | container runtime binary (e.g. `podman`) |

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
| `display_name` | string | **yes** | — | card `name` text |
| `description` | string | **yes** | — | card description text |
| `version` | string | no | probed | card version (skips probing) |
| `workdir` | string | no | Harbor cwd | project dir the CLI is started in (its project root); must exist |
| `args` | string[] | exec/acp rules | — | launch args. `exec` must contain `{{.Prompt}}`; `acp` needs the ACP subcommand |
| `env` | map | no | — | extra environment for the CLI |
| `parser` | string | no | `text` | `exec` framing: `text` \| `stream-json` \| `codex` \| `regex` |
| `regex.text` | string | for `parser: regex` | — | line regex with one capture group |
| `server_url` | string | no | — | `server`: connect to an existing instance instead of spawning |
| `server_protocol` | string | no | `http` | `server`: `http` \| `acp` \| `acp-streamable-http` |
| `allow_dangerous` | bool | no | `false` | enable the CLI's dangerous/permission-bypass flag |
| `interaction` | string | no | `auto` | `auto` \| `interactive` \| `non-interactive`: whether CLI permission/plan prompts surface as A2A `input-required` |
| `sdk_capabilities` | bool | no | `false` | `stream-json`: declare `askUserQuestion` before the first prompt |
| `output_schema_args` | string[] | no | profile | `exec` structured-output args (`{{.SchemaFile}}`/`{{.Schema}}`) |
| `file_args` | string[] | no | profile | attach files via args (`{{.File}}`) |
| `image_args` | string[] | no | profile | attach images via args (`{{.Image}}`) |
| `input_image` | string[] | no | profile | accepted image MIME types |

`agents[].session` (optional; profile defaults for known CLIs):

| key | type | meaning |
|---|---|---|
| `id_flag` | string | flag to set a session id |
| `resume_flag` | string | flag to resume a session (`--resume`) |
| `continue_flag` | string | flag to continue the latest session |
| `fork_flag` | string | flag to fork a session (used when `harbor.fork` is set) |

`agents[].limits` (all optional; `0` disables):

| key | type | meaning |
|---|---|---|
| `max_concurrent` | int | concurrent tasks for this agent (unbounded when `0`) |
| `timeout` | duration | total task timeout |
| `idle_timeout` | duration | fail a task with no events for this long |
| `max_turns` | int | max turns (start + resumes) per task |
| `breaker.threshold` | int | recent calls tracked (window size) |
| `breaker.failure_ratio` | float | trip when the failure ratio in the window reaches this (default `0.5`) |
| `breaker.half_open_successes` | int | consecutive probe successes that close the breaker (default `1`) |
| `breaker.cooldown` | duration | open duration before a probe (also the probe deadline) |
| `breaker.max_cooldown` | duration | backoff cap |

### `agents[].skills[]`

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

```yaml
- id: codebuddy
  binary: codebuddy
  integration: stream-json
  args: ["-p"]
  display_name: "CodeBuddy"
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
- **New parser or adapter (Go):** register a parser via
  `internal/parser.RegisterParser` or a full CLI adapter via
  `agent.RegisterAdapter` (compile-time `init()`); see docs/DESIGN.md §8.
- **Runtime adapter plugin:** build a Go plugin exporting `Register()` and list
  it under `plugins:` (optional, requires cgo on linux/darwin/freebsd).
