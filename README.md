# Harbor

Expose coding-agent CLIs (`codebuddy`, `claude`, `opencode`, `codex`, and others)
as remote [A2A](https://a2a-protocol.org) v1.0 agents. Each CLI is served behind a
standard AgentCard and JSON-RPC endpoint, with bearer auth, discovery, and a
config-driven adapter layer.

- Design: [DESIGN.md](DESIGN.md)
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
| `exec`        | One-shot invocation; the prompt is substituted into `args` and the CLI's stdout is parsed (plain `text` or a `regex`). |
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

Set `agents[].grpc_listen: "127.0.0.1:50051"` to additionally serve that agent
over gRPC; the card then declares the gRPC interface.

With `server.auth.mode: bearer`, the invocation endpoints (JSON-RPC, REST, gRPC)
require `Authorization: Bearer <token>`; cards and discovery stay public. A
non-loopback `server.listen` (or `grpc_listen`) with `auth.mode: none` refuses to
start unless `server.allow_insecure: true`.

## Configuration

`examples/agents.yaml` is a commented starting point. Main sections:

- `server`: `listen`, `base_url`, `auth`, `cors_allowed_origins`, `rate_limit`,
  `task_store_file` (durable tasks/interactions), `session_store_dir`
  (persisted CLI session ids), `allow_insecure`.
- `sandbox`: `workdir_root`/`allow_outside` (workspace confinement),
  `isolation` (`shared`/`worktree`/`copy`), `inherit_env`, `cli_home`,
  `container` (run each CLI in a container), `require_container_for_dangerous`,
  `file_artifacts`, `raw_stream_dir`.
- `telemetry`: OTLP/gRPC `endpoint`, `insecure`, `service_name` (traces + metrics).
- `plugins`: Go plugin `.so` paths, each exporting `Register()`.
- `agents[]`: `id`, `binary`, `integration`, `args`, `env`, `session`,
  `limits`, plus integration-specific fields (`server_protocol`, `grpc_listen`,
  `sdk_capabilities`, `output_schema_args`, `file_args`, `image_args`,
  `input_image`, `allow_dangerous`).
- `agents[].description` sets the card description text (otherwise a default
  template is used). `agents[].skills` advertises capabilities to callers; when
  omitted, no skills are advertised (the A2A `skills` field is still present).
  Each skill requires `id`, `name`, and `description` and optionally `tags`,
  `examples`, `input_modes`, `output_modes`. `display_name` and `version` likewise
  shape the card. Skills are configured by the operator (not probed).

  ```yaml
  - id: codebuddy
    binary: codebuddy
    integration: stream-json
    args: ["-p"]
    description: "CodeBuddy is a helpful software-development assistant."
    # Optional: advertise capabilities to callers.
    # skills:
    #   - id: code-review
    #     name: Code review
    #     description: Review a code change for bugs, style, and quality issues
    #     tags: ["review", "quality"]
  ```

## Testing with a codebuddy client

Start Harbor, then point codebuddy's A2A client at its aggregate discovery
endpoint. The URL must match your `server.base_url` (the command below assumes
Harbor is on port 9099; the bundled example uses 8080):

```sh
make run            # builds and runs on the configured server.listen

# in another shell
CODEBUDDY_A2A_OUTBOUND_ENABLED=1 \
CODEBUDDY_REMOTE_AGENTS_URL=http://127.0.0.1:9099/.well-known/agents.json \
codebuddy
```

You can also inspect the agents directly:

```sh
curl -s http://127.0.0.1:9099/.well-known/agents.json | jq '.[].name'
curl -s http://127.0.0.1:9099/agents/codebuddy/.well-known/agent-card.json | jq
```

## Extending

- **New output format (no code change):** use `exec` with `parser: regex`.
- **New parser or adapter (Go):** register a parser via
  `internal/parser.RegisterParser` or a full CLI adapter via
  `agent.RegisterAdapter` (compile-time `init()`); see DESIGN.md §8.
- **Runtime adapter plugin:** build a Go plugin exporting `Register()` and list
  it under `plugins:` (optional, requires cgo on linux/darwin/freebsd).
