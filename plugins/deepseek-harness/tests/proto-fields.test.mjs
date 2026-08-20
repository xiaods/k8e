// Guards the proto-loader field-name mapping that GrpcK8eClient relies on:
// with `keepCase: false` (grpc.ts loadSync), every proto field surfaces as
// camelCase (sessionId, podIp, exitCode, …) — NOT snake_case. A request built
// with `{ session_id: … }` would serialize empty and a response read via
// `resp.session_id` would be undefined, surfacing as the gateway's
// "session  not found" (empty session id). This test pins the mapping so the
// client and the proto can never drift silently.
import assert from 'node:assert/strict'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { loadSync } from '@grpc/proto-loader'

const protoPath = join(dirname(fileURLToPath(import.meta.url)), '..', 'packages/dsh-k8e-sandbox-client/proto/sandbox.proto')
const def = loadSync(protoPath, {
  keepCase: false,
  longs: String,
  enums: String,
  defaults: true,
  oneofs: true,
})

/** Field names of one sandbox.v1 message as proto-loader (keepCase:false) reports them. */
function fieldNames(type) {
  const t = def[`sandbox.v1.${type}`]
  return (t?.type?.field ?? []).map((f) => f.name)
}

// Response fields grpc.ts reads — must be camelCase.
for (const [type, fields] of Object.entries({
  CreateSessionResponse: ['sessionId', 'podIp'],
  ExecResponse: ['exitCode', 'sessionId', 'durationMs', 'truncated'],
  CreateTerminalResponse: ['terminalId', 'pid'],
  PollRunResponse: ['runId', 'exitCode', 'durationMs', 'truncated'],
  TerminalForegroundResponse: ['processGroupId', 'inputWaiting'],
})) {
  const names = fieldNames(type)
  for (const f of fields) {
    assert.ok(names.includes(f), `${type} must expose camelCase field "${f}" (got ${names.join(', ')})`)
    // Only multi-word camelCase fields have a snake_case twin to guard against.
    const snake = f.replace(/[A-Z]/g, (c) => `_${c.toLowerCase()}`)
    if (snake !== f) {
      assert.ok(!names.includes(snake), `${type} must not expose snake_case "${snake}"`)
    }
  }
}

// Request fields grpc.ts builds — must be camelCase so they serialize.
for (const [type, fields] of Object.entries({
  CreateSessionRequest: ['tenantId', 'runtimeClass', 'allowedHosts'],
  ExecRequest: ['sessionId', 'command', 'timeout', 'workdir', 'background', 'language'],
  CreateTerminalRequest: ['sessionId', 'argv', 'workdir', 'rows', 'cols'],
  TerminalDestroyRequest: ['terminalId', 'graceMs'],
})) {
  const names = fieldNames(type)
  for (const f of fields) {
    assert.ok(names.includes(f), `${type} must expose camelCase field "${f}" (got ${names.join(', ')})`)
  }
}

console.log('✔ proto field-name mapping test passed (keepCase:false → camelCase, no snake_case)')
