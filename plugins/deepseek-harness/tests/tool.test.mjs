// Fake-ctx test for the k8e-sandbox tool provider: the tool schemas are strict
// (additionalProperties: false) and the transport returns extra fields
// (sessionId/status/language) plus durationMs as a string (proto int64 via
// longs: String). The tool must return exactly the declared shape with a
// numeric durationMs, or dsh rejects the tool output.
import assert from 'node:assert/strict'
import { K8eSandboxTools } from '@k8e-sandbox/dsh-k8e-sandbox-tool'

function makeOwner() {
  const calls = []
  const client = {
    calls,
    async run(code, opts) {
      calls.push(['run', code, opts])
      // Transport result: extra fields + string durationMs (proto int64).
      return {
        stdout: 'hello\n',
        stderr: '',
        exitCode: 0,
        sessionId: 'sess-1',
        status: 'completed',
        durationMs: '123',
        truncated: false,
        language: 'bash',
      }
    },
    async runBackground(code, opts) {
      calls.push(['runBackground', code, opts])
      return { runId: 'bg-1', status: 'started', sessionId: 'sess-1' }
    },
    async poll(runId) {
      calls.push(['poll', runId])
      return { runId, status: 'completed', stdout: 'out\n', stderr: 'err\n', exitCode: 0, durationMs: '456', truncated: false }
    },
    async exposeService(sessionId, port, host) {
      calls.push(['exposeService', sessionId, port, host])
      return { url: `http://gw/k8e/expose/${sessionId}/${port}/` }
    },
    async unexposeService(sessionId, port) {
      calls.push(['unexposeService', sessionId, port])
      return { ok: true }
    },
    async listExposed(sessionId) {
      calls.push(['listExposed', sessionId])
      return [{ port: 8080, url: `http://gw/k8e/expose/${sessionId}/8080/`, host: '127.0.0.1', startedAt: 0 }]
    },
    async updateAllowedHosts(sessionId, hosts) {
      calls.push(['updateAllowedHosts', sessionId, hosts])
      return hosts
    },
  }
  // The KIP-24 tools dial the gateway directly (getGrpcClient), not the
  // transport — expose is a gateway operation and k8e-sandbox-cli does not
  // exist inside the sandbox (web-terminal path).
  const grpc = {
    calls,
    async exposeService(sessionId, port, host) {
      calls.push(['grpc.exposeService', sessionId, port, host])
      return { url: `http://gw/k8e/expose/${sessionId}/${port}/` }
    },
    async unexposeService(sessionId, port) {
      calls.push(['grpc.unexposeService', sessionId, port])
      return { ok: true }
    },
    async updateAllowedHosts(sessionId, hosts) {
      calls.push(['grpc.updateAllowedHosts', sessionId, hosts])
      return hosts
    },
  }
  return { getClient: () => client, getGrpcClient: () => grpc, getSession: async () => 'sess-1', calls }
}

function mount() {
  const owner = makeOwner()
  const registered = []
  const ctx = {
    k8eSandbox: owner,
    reflect: { provide() { return () => {} } },
    effect() { return () => {} },
    tools: { register(def) { registered.push(def) } },
  }
  // eslint-disable-next-line no-new
  new K8eSandboxTools(ctx)
  const byName = new Map(registered.map((d) => [d.name, d]))
  return { owner, byName }
}

// k8e_sandbox_exec returns exactly the declared shape (no sessionId/status/
// language) with numeric durationMs.
{
  const { byName } = mount()
  const tool = byName.get('k8e_sandbox_exec')
  assert.ok(tool, 'k8e_sandbox_exec registered')
  const out = await tool.execute({ code: 'echo hello' })
  assert.deepEqual(Object.keys(out).sort(), ['durationMs', 'exitCode', 'stderr', 'stdout', 'truncated'])
  assert.equal(typeof out.durationMs, 'number')
  assert.equal(out.stdout, 'hello\n')
  assert.equal(out.exitCode, 0)
}

// k8e_sandbox_poll returns the declared shape with numeric durationMs.
{
  const { byName } = mount()
  const tool = byName.get('k8e_sandbox_poll')
  const out = await tool.execute({ runId: 'bg-1' })
  assert.deepEqual(Object.keys(out).sort(), ['durationMs', 'exitCode', 'runId', 'status', 'stderr', 'stdout', 'truncated'])
  assert.equal(typeof out.durationMs, 'number')
  assert.equal(out.stdout, 'out\n')
}

// k8e_sandbox_run_background keeps its declared shape.
{
  const { byName } = mount()
  const tool = byName.get('k8e_sandbox_run_background')
  const out = await tool.execute({ code: 'sleep 1' })
  assert.deepEqual(Object.keys(out).sort(), ['runId', 'sessionId', 'status'])
  assert.equal(out.runId, 'bg-1')
}

// KIP-24: k8e_sandbox_expose passes session + port and returns the URL.
{
  const { owner, byName } = mount()
  const tool = byName.get('k8e_sandbox_expose')
  assert.ok(tool, 'k8e_sandbox_expose registered')
  const out = await tool.execute({ port: 8080, host: '127.0.0.1' })
  assert.deepEqual(Object.keys(out).sort(), ['port', 'url'])
  assert.equal(out.url, 'http://gw/k8e/expose/sess-1/8080/')
  assert.deepEqual(owner.calls.at(-1), ['grpc.exposeService', 'sess-1', 8080, '127.0.0.1'])
}

// KIP-24: k8e_sandbox_unexpose passes session + port and returns ok.
{
  const { owner, byName } = mount()
  const tool = byName.get('k8e_sandbox_unexpose')
  assert.ok(tool, 'k8e_sandbox_unexpose registered')
  const out = await tool.execute({ port: 8080 })
  assert.deepEqual(out, { ok: true, port: 8080 })
  assert.deepEqual(owner.calls.at(-1), ['grpc.unexposeService', 'sess-1', 8080])
}

// KIP-24: k8e_sandbox_allow_hosts passes the allowlist through and echoes it.
{
  const { owner, byName } = mount()
  const tool = byName.get('k8e_sandbox_allow_hosts')
  assert.ok(tool, 'k8e_sandbox_allow_hosts registered')
  const out = await tool.execute({ hosts: ['pypi.org', 'internal.example.com'] })
  assert.deepEqual(out, { hosts: ['pypi.org', 'internal.example.com'] })
  assert.deepEqual(owner.calls.at(-1), ['grpc.updateAllowedHosts', 'sess-1', ['pypi.org', 'internal.example.com']])
}

console.log('✔ tool output-shape test passed (strict schema, numeric durationMs, KIP-24 expose/allow_hosts)')
