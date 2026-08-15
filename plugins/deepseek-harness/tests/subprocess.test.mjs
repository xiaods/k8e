// Fake-ctx runtime test for the subprocess provider (learned from
// dsh-context's host.test.mjs): mount K8eSubprocessRuntime on a fake ctx with a
// fake owner + fake gRPC client, then drive spawnTerminal / spawn and assert
// the handle maps correctly. No harness, no gateway, no cluster.
import assert from 'node:assert/strict'
import { PassThrough } from 'node:stream'
import { K8eSubprocessRuntime } from '@k8e/dsh-k8e-sandbox-subprocess'

function makeGrpcClient() {
  const calls = []
  const terminalStream = new PassThrough({ objectMode: true })
  const client = {
    calls,
    terminalStream(id) {
      calls.push(['terminalStream', id])
      return terminalStream
    },
    async createTerminal(req) {
      calls.push(['createTerminal', req])
      return { terminalId: 't1', pid: 42 }
    },
    async terminalWrite(terminalId, data) {
      calls.push(['terminalWrite', terminalId, data])
    },
    async terminalForeground(terminalId) {
      calls.push(['terminalForeground', terminalId])
      return { processGroupId: 7, inputWaiting: false }
    },
    async terminalSignal(terminalId, signal) {
      calls.push(['terminalSignal', terminalId, signal])
      return 7
    },
    async terminalDestroy(terminalId, graceMs) {
      calls.push(['terminalDestroy', terminalId, graceMs])
    },
    execStream(sessionId, command) {
      calls.push(['execStream', sessionId, command])
      return { stdout: new PassThrough(), done: Promise.resolve({ exitCode: 0, signal: null }) }
    },
  }
  client.terminalStreamRef = terminalStream
  return client
}

function makeCtx(owner) {
  const ctx = {
    k8eSandbox: owner,
    reflect: { provide() { return () => {} } },
    effect() { return () => {} },
  }
  return ctx
}

// ---- spawnTerminal: full mapping --------------------------------
{
  const client = makeGrpcClient()
  const owner = { getGrpcClient: () => client, getSession: async () => 's1' }
  const runtime = new K8eSubprocessRuntime(makeCtx(owner))

  const handle = await runtime.spawnTerminal({
    argv: ['bash', '-l'],
    cwd: '/workspace',
    env: { FOO: 'bar' },
    rows: 24,
    cols: 80,
    graceMs: 5000,
  })

  // createTerminal received the resolved request (sessionId + argv + env + size).
  assert.deepEqual(client.calls[0], ['createTerminal', {
    sessionId: 's1',
    argv: ['bash', '-l'],
    workdir: '/workspace',
    env: { FOO: 'bar' },
    rows: 24,
    cols: 80,
  }])
  assert.equal(handle.pid, 42)

  // write -> terminalWrite(terminalId, utf8 bytes)
  await handle.write('echo hi\n')
  const writeCall = client.calls.find((c) => c[0] === 'terminalWrite')
  assert.equal(writeCall[1], 't1')
  assert.equal(Buffer.from(writeCall[2]).toString('utf8'), 'echo hi\n')

  // output stream receives data frames; done resolves on the exit frame.
  const chunks = []
  handle.output.on('data', (c) => chunks.push(c))
  client.terminalStreamRef.write({ data: Buffer.from('hello\n') })
  client.terminalStreamRef.write({ exit: { exitCode: 3, signal: 'SIGTERM' } })
  client.terminalStreamRef.end()

  const outcome = await handle.done
  assert.deepEqual(outcome, { exitCode: 3, signal: 'SIGTERM' })
  assert.equal(Buffer.concat(chunks).toString('utf8'), 'hello\n')

  // inspectForeground -> terminalForeground
  const fg = await handle.inspectForeground()
  assert.deepEqual(fg, { processGroupId: 7, inputWaiting: false })

  // signalForeground -> terminalSignal, returns the pgid
  assert.equal(await handle.signalForeground('SIGINT'), 7)
  assert.deepEqual(client.calls.find((c) => c[0] === 'terminalSignal'), ['terminalSignal', 't1', 'SIGINT'])

  // terminate -> terminalDestroy with the spec grace
  await handle.terminate()
  assert.deepEqual(client.calls.find((c) => c[0] === 'terminalDestroy'), ['terminalDestroy', 't1', 5000])
}

// ---- spawnTerminal: unresolvable foreground -> undefined ----------------
{
  const client = makeGrpcClient()
  client.terminalForeground = async (id) => ({ processGroupId: -1, inputWaiting: false })
  const owner = { getGrpcClient: () => client, getSession: async () => 's1' }
  const runtime = new K8eSubprocessRuntime(makeCtx(owner))
  const handle = await runtime.spawnTerminal({ argv: ['sh'], cwd: '/w', rows: 24, cols: 80, graceMs: 1000 })
  assert.equal(await handle.inspectForeground(), undefined)
}

// ---- spawn: gRPC streaming path ------------------------------------
{
  const client = makeGrpcClient()
  // Make execStream's done controllable so we can write to its stdout first.
  let resolveExec
  const execDone = new Promise((resolve) => { resolveExec = resolve })
  client.execStream = (sessionId, command) => {
    client.calls.push(['execStream', sessionId, command])
    const stdout = new PassThrough()
    client.execStreamRef = stdout
    return { stdout, done: execDone }
  }
  const owner = { getGrpcClient: () => client, getSession: async () => 's1' }
  const runtime = new K8eSubprocessRuntime(makeCtx(owner))

  const handle = runtime.spawn({
    argv: ['echo', 'hi'],
    cwd: '/workspace',
    stdio: { stdin: 'ignore', stdout: 'pipe', stderr: 'pipe' },
    graceMs: 5000,
  })
  // Let the async done() reach execStream (await getSession yields a microtask).
  await new Promise((r) => setImmediate(r))
  assert.ok(client.execStreamRef, 'execStream was called')
  assert.ok(client.calls.some((c) => c[0] === 'execStream'), 'gRPC path used when endpoint present')

  const chunks = []
  handle.stdout.on('data', (c) => chunks.push(c))
  client.execStreamRef.end('streamed\n')
  resolveExec({ exitCode: 0, signal: null })
  const outcome = await handle.done
  assert.deepEqual(outcome, { exitCode: 0, signal: null })
  assert.equal(Buffer.concat(chunks).toString('utf8'), 'streamed\n')
}

// ---- spawn: CLI fallback when no gRPC client --------------------------
{
  const cliCalls = []
  const owner = {
    getGrpcClient: () => { throw new Error('no endpoint') },
    getSession: async () => 's1',
    getClient: () => ({ run: async (code, opts) => { cliCalls.push([code, opts]); return { stdout: 'cli-out\n', stderr: '', exitCode: 7 } } }),
  }
  const runtime = new K8eSubprocessRuntime(makeCtx(owner))
  const handle = runtime.spawn({
    argv: ['echo', 'hi'],
    cwd: '/workspace',
    stdio: { stdin: 'ignore', stdout: 'pipe', stderr: 'pipe' },
    graceMs: 5000,
  })

  const chunks = []
  handle.stdout.on('data', (c) => chunks.push(c))
  const outcome = await handle.done
  assert.deepEqual(outcome, { exitCode: 7, signal: null })
  assert.equal(Buffer.concat(chunks).toString('utf8'), 'cli-out\n')
  assert.equal(cliCalls.length, 1, 'CLI fallback used exactly once')
  assert.equal(cliCalls[0][0], "'echo' 'hi'", 'argv is shell-quoted into one command')
}

console.log('✔ subprocess fake-ctx test passed (spawnTerminal mapping, stream frames, spawn gRPC + CLI fallback)')
