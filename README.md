# Harbor

Expose coding-agent CLIs (`opencode`, `codebuddy`, `claude`, `codex`, and others)
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

## Test

```sh
make test
```
