# KIP-4: Sandbox MCP Skill

| Author | Updated | Status |
|--------|---------|--------|
| @xiaods | 2026-04-02 | Draft |

## Summary

Embed a **Model Context Protocol (MCP) server** into the K8E binary as `k8e sandbox-mcp`. AI agent tools — kiro-cli, claude code, gemini cli, and any MCP-compatible agent — install this skill once and gain the ability to dispatch code-execution tasks to K8E's isolated sandbox infrastructure with zero manual configuration.

## Motivation

KIP-3 delivered a production-grade gRPC sandbox API. However, AI agents cannot yet discover or call it without custom integration work per agent. The missing piece is a **standard adapter layer** between the agent's tool-use interface and K8E's gRPC service.

By shipping an MCP server inside the k8e binary, we get:

- One implementation that works across all MCP-compatible agents
- Zero extra binaries to install — agents just run `k8e sandbox-mcp`
- Auto-discovery of the local K8E cluster — no manual endpoint configuration
- Full lifecycle management exposed as discrete, composable tools

## Background

### Model Context Protocol (MCP)

MCP is a JSON-RPC 2.0 protocol over stdio. An MCP server exposes **tools** that agents can discover (`tools/list`) and invoke (`tools/call`). The agent process spawns the MCP server as a child process and communicates via stdin/stdout.

```
Agent Process
    │
    ├─ stdin  ──▶  MCP Server (k8e sandbox-mcp)
    └─ stdout ◀──  │
                   └─ gRPC (TLS) ──▶ sandbox-grpc-gateway:50051
                                              │
                                     SandboxService (K8E)
                                              │
                                     Isolated Pod (gVisor/Kata/Firecracker)
```

MCP session lifecycle:
1. Agent spawns `k8e sandbox-mcp` as subprocess
2. Agent sends `initialize` — server responds with capabilities and tool list
3. Agent calls `tools/call` with tool name + arguments
4. Server proxies to gRPC, returns result as MCP `content` array
5. Agent process exits → subprocess is killed → no dangling sessions

### Agent Configuration Patterns

**claude code:**
```bash
claude mcp add k8e-sandbox -- k8e sandbox-mcp
```

**kiro-cli** (`.kiro/settings.json`):
```json
{
  "mcpServers": {
    "k8e-sandbox": {
      "command": "k8e",
      "args": ["sandbox-mcp"]
    }
  }
}
```

**gemini cli** (`~/.gemini/settings.json`):
```json
{
  "mcpServers": {
    "k8e-sandbox": {
      "command": "k8e",
      "args": ["sandbox-mcp"]
    }
  }
}
```

### Auto-Discovery Strategy

The MCP server resolves the gRPC endpoint without user configuration by probing in order:

1. `K8E_SANDBOX_ENDPOINT` env var (explicit override)
2. `K8E_SANDBOX_CERT` / `K8E_SANDBOX_KEY` env vars (TLS override)
3. `/var/lib/k8e/server/tls/serving-kube-apiserver.crt` (server node, root)
4. `/etc/k8e/k8e.yaml` kubeconfig → extract server CA (agent node / non-root)
5. Default: `127.0.0.1:50051` with system CA pool

### Existing gRPC Surface (from KIP-3)

The `SandboxService` proto defines 10 RPC methods that map 1:1 to MCP tools:

| gRPC Method | MCP Tool | Description |
|---|---|---|
| `CreateSession` | `sandbox_create_session` | Provision an isolated sandbox pod |
| `DestroySession` | `sandbox_destroy_session` | Tear down session and clean up |
| `Exec` | `sandbox_exec` | Run a command, return stdout/stderr/exit_code |
| `ExecStream` | `sandbox_exec_stream` | Stream command output chunk by chunk |
| `WriteFile` | `sandbox_write_file` | Write a file into the sandbox workspace |
| `ReadFile` | `sandbox_read_file` | Read a file from the sandbox workspace |
| `ListFiles` | `sandbox_list_files` | List files modified since a timestamp |
| `PipInstall` | `sandbox_pip_install` | Install Python packages inside sandbox |
| `RunSubAgent` | `sandbox_run_subagent` | Spawn a child sandbox (depth ≤ 1) |
| `ConfirmAction` | `sandbox_confirm_action` | Gate irreversible actions on user approval |

## Design

### Package Layout

```
pkg/sandboxmcp/
  client.go      — gRPC client with auto-discovery
  tools.go       — MCP tool definitions (schema + handlers)
  server.go      — MCP stdio server (JSON-RPC 2.0 loop)
pkg/cli/cmds/
  sandbox_mcp.go — CLI subcommand entry point
cmd/k8e/main.go  — register sandbox-mcp subcommand
```

### MCP Server State Machine

```
stdin line ──▶ parse JSON-RPC ──▶ dispatch
                                    │
                    ┌───────────────┼───────────────┐
                    ▼               ▼               ▼
              initialize      tools/list      tools/call
                    │               │               │
              respond caps    return tool     invoke handler
                              schema list     ──▶ gRPC call
                                              ◀── result JSON
                                              write stdout
```

### Tool Schema Example — `sandbox_exec`

```json
{
  "name": "sandbox_exec",
  "description": "Execute a shell command inside an isolated K8E sandbox pod. Returns stdout, stderr, and exit code.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "session_id": { "type": "string", "description": "Sandbox session ID from sandbox_create_session" },
      "command":    { "type": "string", "description": "Shell command to execute" },
      "timeout":    { "type": "integer", "description": "Timeout in seconds (default 30)" },
      "workdir":    { "type": "string",  "description": "Working directory (default /workspace)" }
    },
    "required": ["session_id", "command"]
  }
}
```

### Tool Schema Example — `sandbox_create_session`

```json
{
  "name": "sandbox_create_session",
  "description": "Create a new isolated sandbox session in K8E. Returns a session_id for subsequent calls.",
  "inputSchema": {
    "type": "object",
    "properties": {
      "session_id":    { "type": "string", "description": "Optional custom session ID; auto-generated if omitted" },
      "tenant_id":     { "type": "string", "description": "Tenant identifier for multi-tenant isolation" },
      "runtime_class": { "type": "string", "description": "Isolation backend: gvisor (default), kata, firecracker" },
      "allowed_hosts": {
        "type": "array",
        "items": { "type": "string" },
        "description": "Egress allowlist (FQDN). Defaults to pypi.org, github.com, etc."
      }
    }
  }
}
```

### Error Handling

MCP tool errors are returned as `isError: true` content items (not JSON-RPC errors), so the agent can read the error message and decide how to recover:

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "result": {
    "content": [{ "type": "text", "text": "gRPC error: session not found" }],
    "isError": true
  }
}
```

gRPC connection failures return a JSON-RPC `-32603` internal error to signal the agent that the tool infrastructure itself is unavailable.

## Implementation Plan

### Task 1 — gRPC Client with Auto-Discovery (`pkg/sandboxmcp/client.go`)

Implement `NewClient() (*Client, error)` that:
- Probes TLS cert paths in priority order (env vars → server TLS dir → kubeconfig CA)
- Returns a connected `pb.SandboxServiceClient`
- Exposes `Close()` for cleanup

**Test:** unit test for path-probe logic with temp cert files; mock gRPC server connection.

**Demo:** `NewClient()` succeeds on a machine with K8E installed, zero config required.

### Task 2 — MCP stdio Server (`pkg/sandboxmcp/server.go`)

Implement `Server.Run(ctx)` that:
- Reads newline-delimited JSON from stdin
- Handles `initialize`, `tools/list`, `tools/call`
- Writes JSON-RPC responses to stdout
- Gracefully exits on context cancellation or stdin EOF

**Test:** pipe-based test: send `initialize` request, assert capabilities response contains `tools`.

**Demo:** `echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{...}}' | k8e sandbox-mcp` returns valid MCP capabilities JSON.

### Task 3 — 10 MCP Tool Definitions (`pkg/sandboxmcp/tools.go`)

Implement `AllTools() []Tool` and per-tool handler functions. Each handler: unmarshal args → call gRPC → marshal result to MCP content text.

**Test:** table-driven test for each tool handler using a mock gRPC client.

**Demo:** `tools/list` returns all 10 tools with correct JSON Schema definitions.

### Task 4 — CLI Subcommand (`pkg/cli/cmds/sandbox_mcp.go` + `cmd/k8e/main.go`)

Add `NewSandboxMCPCommand` following the `sandbox_gateway.go` pattern. Register in `main.go` `app.Commands`. Optional flags: `--endpoint`, `--tls-cert`, `--tls-key` (all override auto-discovery).

**Test:** `k8e sandbox-mcp --help` exits 0 with correct usage text.

**Demo:** `k8e sandbox-mcp` starts and an agent can connect and list tools.

### Task 5 — End-to-End Integration + Agent Config Docs

- `docs/sandbox-mcp-quickstart.md`: per-agent install snippets (claude code, kiro-cli, gemini cli)
- Integration test: spawn `k8e sandbox-mcp` subprocess → `initialize` → `tools/list` (assert 10 tools) → `sandbox_create_session` → `sandbox_exec` → `sandbox_destroy_session`

**Demo:** In claude code, say "run this Python snippet in a sandbox" — agent automatically calls K8E sandbox tools and returns the output.

## Sequence Diagram — Typical Agent Interaction

```
Agent                  k8e sandbox-mcp          sandbox-grpc-gateway
  │                          │                          │
  │── initialize ──────────▶ │                          │
  │◀─ {capabilities} ─────── │                          │
  │                          │                          │
  │── tools/list ──────────▶ │                          │
  │◀─ [10 tools] ──────────── │                          │
  │                          │                          │
  │── tools/call             │                          │
  │   sandbox_create_session ▶│                          │
  │                          │── CreateSession ────────▶ │
  │                          │◀─ {session_id, pod_ip} ── │
  │◀─ {session_id} ────────── │                          │
  │                          │                          │
  │── tools/call             │                          │
  │   sandbox_exec ─────────▶│                          │
  │                          │── Exec ─────────────────▶ │
  │                          │◀─ {stdout, exit_code} ─── │
  │◀─ {stdout, exit_code} ─── │                          │
  │                          │                          │
  │── tools/call             │                          │
  │   sandbox_destroy_session▶│                          │
  │                          │── DestroySession ───────▶ │
  │                          │◀─ {ok: true} ──────────── │
  │◀─ {ok: true} ──────────── │                          │
```

## Alternatives Considered

| Option | Rejected Reason |
|---|---|
| Per-agent native plugin | 3x implementation effort, diverges over time |
| HTTP REST adapter | Extra network hop, no streaming support |
| Python MCP SDK | Requires Python runtime on every agent machine |
| Separate binary `k8e-sandbox-mcp` | Breaks single-binary promise of K8E |

## Security Considerations

- MCP server runs as the invoking user; gRPC TLS cert access requires appropriate file permissions
- Session IDs are opaque strings; agents cannot enumerate other tenants' sessions
- `sandbox_confirm_action` enforces human-in-the-loop for irreversible operations
- `allowed_hosts` egress allowlist is enforced at the Cilium eBPF layer, not in the MCP layer

## Open Questions

1. Should `sandbox_exec_stream` accumulate chunks or use SSE transport?
   (Recommendation: accumulate chunks, return as single text block for stdio transport)
2. Should the MCP server auto-create a default session if `session_id` is omitted on `sandbox_exec`?
   (Recommendation: yes, with auto-cleanup on server exit via defer)
