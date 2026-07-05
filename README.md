# WebCodex

WebCodex brings Codex agent capabilities into ChatGPT Web.

**Codex limits are not consumed: the work happens through ChatGPT Web, so only the current chat limit is used while the experience stays close to Codex.**

The project is built for a setup where ChatGPT reaches a local environment through a public MCP endpoint, while the workstation keeps an outbound connection to a public gateway.

Russian version: [README.ru.md](README.ru.md)

## What WebCodex Provides

WebCodex uses the full Codex runtime as its foundation instead of reimplementing agent tools inside the gateway. ChatGPT Web receives direct MCP tools backed by the original Codex code, so tool execution keeps the same environment, behavior, and performance as the regular Codex agent path. The old `codex` and `codex-reply` wrapper tools are not exposed by default.

The public server does not execute local commands. It accepts MCP requests from ChatGPT, forwards them through the connected local agent stream, and returns the MCP response produced by the local machine.

## Architecture

The system has two executables.

`webcodex-gate` runs on a public host. It serves the ChatGPT-facing MCP endpoint, OAuth helper endpoints, and private endpoints for the agent stream.

`webcodex-agent` runs on the machine with the working environment. It starts the patched Codex MCP server over standard input/output, opens an outbound stream to the gateway, and executes forwarded MCP requests locally.

```text
ChatGPT Web
  -> public HTTPS MCP endpoint
  -> webcodex-gate
  -> outbound agent stream
  -> webcodex-agent
  -> patched Codex MCP server
  -> local filesystem / shell
```

## Security Model

WebCodex can be deployed with different access levels. A full-access deployment can expose filesystem tools, command execution, patch application, and commands running as root. A restricted deployment can expose only explicitly allowed MCP tools or deny selected tools at the gateway.

Authentication is split across two tokens:

| Variable | Used by | Purpose |
| --- | --- | --- |
| `WEBCODEX_PUBLIC_TOKEN` | ChatGPT / MCP clients | Bearer token issued by the OAuth shim and accepted by `/mcp` |
| `WEBCODEX_AGENT_TOKEN` | local agent | Bearer token for `/agent/stream` and `/agent/result` |
| `WEBCODEX_OAUTH_CLIENT_ID` | ChatGPT OAuth setup | optional client id check |
| `WEBCODEX_OAUTH_CLIENT_SECRET` | ChatGPT OAuth setup | optional client secret check |

Secrets belong in the deployment environment, not in git.

Tool exposure is configured at the gateway:

| Variable | Behavior |
| --- | --- |
| `WEBCODEX_ALLOWED_TOOLS` | Comma-separated list of allowed tools. Empty means all Codex tools are allowed unless explicitly denied. |
| `WEBCODEX_DENIED_TOOLS` | Comma-separated list of denied tools. Deny rules override allow rules. |

The policy is applied to both `tools/list` and `tools/call`, so hidden tools cannot be called directly by name through the public MCP endpoint.

## ChatGPT Connection

Use a custom MCP connector with OAuth authentication.

| Field | Value |
| --- | --- |
| Server URL | `https://<mcp-host>/mcp` |
| Authorization URL | `https://<mcp-host>/oauth/authorize` |
| Token URL | `https://<mcp-host>/oauth/token` |
| Client ID | value of `WEBCODEX_OAUTH_CLIENT_ID` |
| Client Secret | value of `WEBCODEX_OAUTH_CLIENT_SECRET` |
| Token endpoint auth | `client_secret_basic` or `client_secret_post` |

The OAuth implementation is intentionally minimal: it validates the configured client credentials and returns the configured public Bearer token for the MCP endpoint.

## Gateway Configuration

`webcodex-gate` is configured through environment variables.

| Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `WEBCODEX_ADDR` | no | `:8080` | HTTP server address |
| `WEBCODEX_PUBLIC_URL` | recommended | derived from `WEBCODEX_ADDR` | public HTTPS address for OAuth metadata |
| `WEBCODEX_PUBLIC_TOKEN` | yes | none | Bearer token for `/mcp` |
| `WEBCODEX_AGENT_TOKEN` | yes | none | Bearer token for local agent endpoints |
| `WEBCODEX_OAUTH_CLIENT_ID` | no | none | allowed OAuth client id |
| `WEBCODEX_OAUTH_CLIENT_SECRET` | no | none | allowed OAuth client secret |
| `WEBCODEX_CALL_TIMEOUT` | no | `2m` | maximum time for one forwarded MCP call |
| `WEBCODEX_ALLOWED_TOOLS` | no | none | comma-separated list of allowed MCP tools |
| `WEBCODEX_DENIED_TOOLS` | no | none | comma-separated list of denied MCP tools |

The public reverse proxy must route these paths to the gateway:

| Path | Purpose |
| --- | --- |
| `/mcp` | MCP JSON-RPC endpoint for ChatGPT |
| `/.well-known/oauth-protected-resource` | metadata for MCP authentication |
| `/.well-known/oauth-authorization-server` | OAuth server metadata |
| `/oauth/authorize` | OAuth authorization endpoint |
| `/oauth/token` | OAuth token endpoint |
| `/agent/stream` | private local-agent request stream |
| `/agent/result` | private local-agent response endpoint |

TLS is usually terminated at the reverse proxy.

## Agent Configuration

`webcodex-agent` is configured through environment variables.

| Variable | Required | Default | Description |
| --- | --- | --- | --- |
| `WEBCODEX_GATE_URL` | yes | none | public base URL of the gateway |
| `WEBCODEX_AGENT_TOKEN` | yes | none | shared private token for agent endpoints |
| `WEBCODEX_CODEX_MCP_CMD` | no | `third_party/codex/codex-rs/target/debug/codex-mcp-server` | shell command that starts the patched Codex MCP server |
| `WEBCODEX_MCP_CALL_TIMEOUT` | no | `2m` | maximum time for one local MCP request |

The agent process should run as the user that owns the intended Codex environment. For full local control, that user needs a Codex configuration like:

```toml
approval_policy = "never"
sandbox_mode = "danger-full-access"
```

Passwordless sudo is optional and should only be enabled for deployments where root-capable tools are expected.

## Patched Codex

The patched Codex source tree is vendored in `third_party/codex`.

The MCP patch changes `codex-mcp-server` so the server publishes direct internal Codex tools as separate MCP tools. Main change points:

```text
third_party/codex/codex-rs/core/src/codex_thread.rs
third_party/codex/codex-rs/mcp-server/src/message_processor.rs
```

`CODEX_MCP_LEGACY_TOOLS=1` restores the old `codex` and `codex-reply` wrapper tools.

## Build

Requirements:

| Component | Purpose |
| --- | --- |
| Go | `webcodex-gate` and `webcodex-agent` |
| Rust toolchain | patched `codex-mcp-server` |

Build local executables:

```bash
go build -o bin/webcodex-gate ./cmd/gate
go build -o bin/webcodex-agent ./cmd/agent
(cd third_party/codex/codex-rs && cargo build -p codex-mcp-server)
```

Build the gateway for another Linux architecture:

```bash
GOOS=linux GOARCH=arm64 go build -o bin/webcodex-gate-linux-arm64 ./cmd/gate
```

## Deployment Shape

A typical deployment:

| Host | Process | Network |
| --- | --- | --- |
| public server | `webcodex-gate` behind an HTTPS reverse proxy | accepts ChatGPT and agent connections |
| workstation | `webcodex-agent` under systemd | opens an outbound HTTPS connection to the gateway |

Example systemd unit files are in `deploy/`. They expect environment files:

```text
/etc/webcodex/gate.env
/etc/webcodex/agent.env
```

## Verification

For deployments where elevated commands are allowed, `exec_command` verifies the local execution user and sudo behavior:

```bash
curl -sS https://<mcp-host>/mcp \
  -H "Authorization: Bearer $WEBCODEX_PUBLIC_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"exec_command","arguments":{"cmd":"id && sudo -n id","workdir":"/tmp","yield_time_ms":1000,"max_output_tokens":2000}}}' | jq -r '.result.content[0].text'
```

## Development Checks

```bash
go test ./...
go vet ./...
(cd third_party/codex/codex-rs && cargo check -p codex-mcp-server)
```
