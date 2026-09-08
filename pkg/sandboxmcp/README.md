# MCP HTTP boundary (in progress)

`Server` implements the KIP-8 MCP 2026-07-28 request/response boundary without
opening a listener. It is not yet a deployable MCP service.

Implemented: authenticated principal injection, exact Origin allowlist, request
size/deadline limits, per-request protocol metadata, header/body checks,
`server/discover`, stable `tools/list`, and `tools/call`. Only tools capability is
advertised. JSON responses include the modern result discriminator. There is no
protocol session, initialization handshake, SSE stream, or Tasks extension.

Tool callbacks own argument validation and resource authorization. Authentication
is a required dependency; the fake authentication in tests is not a supported
production credential mechanism. No generic CLI handler or process environment
is used. Schemas currently describe object arguments; actual K8E schemas and
validation must be implemented together with the backend adapter. Do not attach
schemas with `x-mcp-header` until custom parameter header validation is supported.

Next implementation slice:

1. Add the principal/owner and durable operation store, including concurrent
   admission and unknown-result recovery tests.
2. Add concrete K8E tool schemas and a gRPC backend; enforce ownership on every
   handle, including background run polling.
3. Add standards-compatible HTTP authorization, HTTPS deployment configuration,
   and an opt-in listener. No anonymous or shared-default-session mode.
4. Validate real-client interoperability and restart/cross-replica behavior before
   marking the revised KIP implemented. CLI behavior remains unchanged.

Validation:

```sh
go test -race ./pkg/sandboxmcp -count=1
```
