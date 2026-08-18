/**
 * k8e-sandbox host-UI bridge (KIP-20 client half): the transport the browser
 * half talks to — a `/k8e-sandbox/api` status route, a lazy-chunk bundle route
 * (`/k8e-sandbox/bundle/<name>.js`), and a `/k8e-sandbox/ws/terminal`
 * WebSocket that bridges the sandbox's remote PTY (KIP-19 spawnTerminal) to a
 * browser terminal. The config page itself lives in the client half.
 * @module @k8e-sandbox/dsh-k8e-sandbox-host-ui
 */

import { readFile } from 'node:fs/promises'
import type { IncomingMessage } from 'node:http'
import { createRequire } from 'node:module'
import type { Duplex } from 'node:stream'
import { WebSocket, WebSocketServer } from 'ws'
import type { Context, K8eExecEvent, SandboxHttpRequest, SandboxHttpResponse } from './context-types.ts'

export const name = 'dsh-k8e-sandbox-host-ui'

/** Services required before mounting. */
export const inject = ['webServer', 'k8eSandbox']

function clamp(value: number, min: number, max: number, fallback: number): number {
  if (!Number.isFinite(value)) return fallback
  return Math.min(max, Math.max(min, value))
}

function writeJson(res: SandboxHttpResponse, status: number, body: unknown): void {
  res.writeHead(status, { 'content-type': 'application/json' })
  res.end(JSON.stringify(body))
}

/**
 * Parse a request's path-relative URL. `req.url` is always an origin-form
 * path + query (never an absolute URL), so parsing it directly avoids the
 * synthetic scheme a `new URL(…, base)` would otherwise require.
 */
function parseRequestUrl(req: SandboxHttpRequest): { pathname: string; searchParams: URLSearchParams } {
  const raw = req.url ?? '/'
  const queryIndex = raw.indexOf('?')
  const pathname = queryIndex === -1 ? raw : raw.slice(0, queryIndex)
  const query = queryIndex === -1 ? '' : raw.slice(queryIndex + 1)
  return { pathname, searchParams: new URLSearchParams(query) }
}

/**
 * Lazy client chunks (the heavy xterm stack). The core bundle references
 * `/k8e-sandbox/bundle/<name>.js`; this route resolves the built chunk from the
 * client-ui package and serves it fresh (no-cache — the browser reloads on HMR).
 */
const require_ = createRequire(import.meta.url)
const CHUNK_RESOLVER: Record<string, string> = {
  terminal: '@k8e-sandbox/dsh-k8e-sandbox-client-ui/client-terminal',
}

async function serveChunk(req: SandboxHttpRequest, res: SandboxHttpResponse): Promise<void> {
  const { pathname } = parseRequestUrl(req)
  const name = pathname.slice('/k8e-sandbox/bundle/'.length).replace(/\.js$/, '')
  const spec = CHUNK_RESOLVER[name]
  if (spec === undefined) {
    writeJson(res, 404, { ok: false, error: { code: 'not-found', message: `unknown chunk "${name}"` } })
    return
  }
  try {
    const path = require_.resolve(spec)
    const content = await readFile(path)
    res.writeHead(200, {
      'content-type': 'application/javascript; charset=utf-8',
      'cache-control': 'no-cache',
    })
    res.end(content)
  } catch (error) {
    writeJson(res, 404, { ok: false, error: { code: 'missing', message: error instanceof Error ? error.message : String(error) } })
  }
}

/** One recent sandbox execution, accumulated for the command-log replay. */
interface ExecLogEntry {
  id: string
  command: string
  cwd?: string
  startedAt: number
  stdout: string
  stderr: string
  exitCode: number | null
  signal: string | null
  settled: boolean
}

const MAX_HISTORY = 30
const MAX_OUTPUT = 256 * 1024

function sseWrite(res: SandboxHttpResponse, event: string, data: unknown): void {
  res.write(`event: ${event}\ndata: ${JSON.stringify(data)}\n\n`)
}

function appendCapped(base: string, chunk: string, max: number): string {
  const next = base + chunk
  return next.length > max ? next.slice(next.length - max) : next
}

/** Clamp a WebSocket close reason to the 123-byte UTF-8 limit without splitting code points. */
function clampCloseReason(reason: string): string {
  const MAX = 123
  if (Buffer.byteLength(reason, 'utf8') <= MAX) return reason
  const suffix = '…'
  const budget = MAX - Buffer.byteLength(suffix, 'utf8')
  let result = ''
  for (const char of reason) {
    const next = result + char
    if (Buffer.byteLength(next, 'utf8') > budget) break
    result = next
  }
  return result + suffix
}

type ExecStartEvent = Extract<K8eExecEvent, { phase: 'start' }>
type ExecOutputEvent = Extract<K8eExecEvent, { phase: 'output' }>
type ExecExitEvent = Extract<K8eExecEvent, { phase: 'exit' }>

function recordExecStart(event: ExecStartEvent, entries: Map<string, ExecLogEntry>, order: string[]): void {
  entries.set(event.id, {
    id: event.id,
    command: event.command,
    ...(event.cwd !== undefined ? { cwd: event.cwd } : {}),
    startedAt: event.at,
    stdout: '',
    stderr: '',
    exitCode: null,
    signal: null,
    settled: false,
  })
  order.push(event.id)
  if (order.length <= MAX_HISTORY) return
  const evicted = order.shift()
  if (evicted !== undefined) entries.delete(evicted)
}

function recordExecOutput(event: ExecOutputEvent, entries: Map<string, ExecLogEntry>): void {
  const entry = entries.get(event.id)
  if (entry === undefined) return
  if (event.stream === 'stderr') entry.stderr = appendCapped(entry.stderr, event.data, MAX_OUTPUT)
  else entry.stdout = appendCapped(entry.stdout, event.data, MAX_OUTPUT)
}

function recordExecExit(event: ExecExitEvent, entries: Map<string, ExecLogEntry>): void {
  const entry = entries.get(event.id)
  if (entry === undefined) return
  entry.exitCode = event.exitCode
  entry.signal = event.signal
  entry.settled = true
}

/** Bridge one WebSocket to a sandbox PTY. */
async function attachTerminal(ctx: Context, ws: WebSocket, req: SandboxHttpRequest): Promise<void> {
  // Registered before any await so a client that disconnects during session
  // creation is still observed; cleanup is assigned once the terminal exists.
  let cleanup: (() => void) | undefined
  ws.on('close', () => { cleanup?.() })

  try {
    const { searchParams } = parseRequestUrl(req)
    const rows = clamp(Number.parseInt(searchParams.get('rows') ?? '24', 10), 1, 200, 24)
    const cols = clamp(Number.parseInt(searchParams.get('cols') ?? '80', 10), 1, 400, 80)

    const grpc = ctx.k8eSandbox.getGrpcClient()
    const sessionId = await ctx.k8eSandbox.getSession()
    const created = await grpc.createTerminal({
      sessionId,
      argv: ['bash', '-l'],
      workdir: ctx.k8eSandbox.cwd,
      rows,
      cols,
    })
    const id = created.terminalId

    let destroyed = false
    cleanup = (): void => {
      if (destroyed) return
      destroyed = true
      void grpc.terminalDestroy(id, 1000).catch(() => undefined)
    }

    // A write/resize against an exited terminal rejects; report it and tear the
    // session down instead of leaving a dead socket silently swallowing input.
    const fail = (reason: string): void => {
      if (ws.readyState === WebSocket.OPEN) {
        ws.send(`\r\n[terminal error: ${reason}]\r\n`)
      }
      cleanup?.()
      ws.close(1011, clampCloseReason(reason))
    }

    // The socket may have closed while the terminal was being created; destroy
    // it now instead of leaking the PTY and its gRPC stream.
    if (ws.readyState === WebSocket.CLOSED || ws.readyState === WebSocket.CLOSING) {
      cleanup()
      return
    }

    const stream = grpc.terminalStream(id)
    stream.on('data', (frame: any) => {
      if (frame.data !== undefined && ws.readyState === WebSocket.OPEN) {
        ws.send(frame.data)
      } else if (frame.exit !== undefined) {
        if (ws.readyState === WebSocket.OPEN) {
          ws.send(`\r\n[process exited with code ${String(frame.exit.exitCode ?? 0)}]\r\n`)
        }
        cleanup?.()
        ws.close(1000, 'terminal exited')
      }
    })
    stream.on('error', (err: unknown) => {
      fail(err instanceof Error ? err.message : String(err))
    })

    interface ControlFrame { type?: unknown; cols?: unknown; rows?: unknown }
    ws.on('message', (data) => {
      const text = data.toString('utf8')
      let control: ControlFrame | null = null
      try {
        const parsed: unknown = JSON.parse(text)
        if (parsed !== null && typeof parsed === 'object' && !Array.isArray(parsed)) {
          control = parsed as ControlFrame
        }
      } catch {
        // Not JSON: raw terminal input.
      }
      if (control !== null && control.type === 'close') {
        cleanup?.()
        return
      }
      if (control !== null && control.type === 'resize' && typeof control.cols === 'number' && typeof control.rows === 'number') {
        void grpc.terminalResize(id, clamp(control.cols, 1, 400, cols), clamp(control.rows, 1, 200, rows))
          .catch(() => fail('terminal resize failed; the terminal may have exited'))
        return
      }
      void grpc.terminalWrite(id, new TextEncoder().encode(text))
        .catch(() => fail('terminal write failed; the terminal may have exited'))
    })
  } catch (error) {
    cleanup?.()
    if (ws.readyState === WebSocket.OPEN) {
      ws.send(`\r\n[terminal failed: ${error instanceof Error ? error.message : String(error)}]\r\n`)
    }
    ws.close(1011, error instanceof Error ? error.message : String(error))
  }
}

export function apply(ctx: Context): void {
  // ── JSON API ────────────────────────────────────────────────────────────
  ctx.effect(() => ctx.webServer.register({
    kind: 'prefix',
    path: '/k8e-sandbox/api',
    handler: async (req, res) => {
      if (req.method !== 'POST') {
        writeJson(res, 405, { ok: false, error: { code: 'method-error', message: 'method not allowed' } })
        return
      }
      const { pathname } = parseRequestUrl(req)
      const method = pathname.startsWith('/k8e-sandbox/api/') ? pathname.slice('/k8e-sandbox/api/'.length) : undefined
      if (method === 'status') {
        let grpcAvailable = false
        try {
          ctx.k8eSandbox.getGrpcClient()
          grpcAvailable = true
        } catch {
          grpcAvailable = false
        }
        writeJson(res, 200, {
          ok: true,
          grpcAvailable,
          cwd: ctx.k8eSandbox.cwd,
          endpoint: ctx.k8eSandbox.endpoint,
          certDir: ctx.k8eSandbox.certDir,
          runtimeClass: ctx.k8eSandbox.runtimeClass,
        })
        return
      }
      writeJson(res, 404, { ok: false, error: { code: 'not-found', message: `unknown method "${method ?? ''}"` } })
    },
  }), 'k8e-sandbox-host-ui: /k8e-sandbox/api routes')

  // ── Lazy chunk bundle route ────────────────────────────────────────────
  ctx.effect(() => ctx.webServer.register({
    kind: 'prefix',
    path: '/k8e-sandbox/bundle',
    handler: (req, res) => { void serveChunk(req, res) },
  }), 'k8e-sandbox-host-ui: /k8e-sandbox/bundle chunk route')

  // ── Sandbox activity SSE (KIP-20 M3) ────────────────────────────────────
  // Replays a small recent-command log on connect, then streams `k8e-sandbox/exec`
  // events live so the browser terminal panel renders running commands and the
  // core bundle can auto-open on activity.
  const execLog = new Map<string, ExecLogEntry>()
  const execOrder: string[] = []
  const activityClients = new Set<SandboxHttpResponse>()

  const broadcastExec = (event: K8eExecEvent): void => {
    for (const client of activityClients) {
      try {
        sseWrite(client, 'exec', event)
      } catch {
        activityClients.delete(client)
      }
    }
  }

  const onExec = (event: K8eExecEvent): void => {
    if (event.phase === 'start') recordExecStart(event, execLog, execOrder)
    else if (event.phase === 'output') recordExecOutput(event, execLog)
    else recordExecExit(event, execLog)
    broadcastExec(event)
  }

  ctx.effect(() => {
    const off = ctx.on('k8e-sandbox/exec', onExec)
    return () => { off() }
  }, 'k8e-sandbox-host-ui: exec log subscription')

  ctx.effect(() => ctx.webServer.register({
    kind: 'exact',
    path: '/k8e-sandbox/activity',
    handler: (req, res) => {
      res.writeHead(200, {
        'content-type': 'text/event-stream; charset=utf-8',
        'cache-control': 'no-cache',
        'connection': 'keep-alive',
      })
      const history = execOrder.map((id) => execLog.get(id)).filter((entry): entry is ExecLogEntry => entry !== undefined)
      sseWrite(res, 'history', { entries: history })
      activityClients.add(res)
      res.on('close', () => { activityClients.delete(res) })
    },
  }), 'k8e-sandbox-host-ui: /k8e-sandbox/activity SSE')

  // ── Terminal WebSocket ──────────────────────────────────────────────────
  const wss = new WebSocketServer({ noServer: true })
  ctx.effect(() => ctx.webServer.registerUpgrade({
    path: '/k8e-sandbox/ws/terminal',
    handler: (req, socket, head) => {
      wss.handleUpgrade(req as unknown as IncomingMessage, socket as unknown as Duplex, head as Buffer, (ws) => {
        void attachTerminal(ctx, ws, req)
      })
    },
  }), 'k8e-sandbox-host-ui: terminal WebSocket')

  ctx.effect(() => () => { wss.close() }, 'k8e-sandbox-host-ui: teardown')
}

// Named exports only (no default): the loader returns the module namespace when
// there is no `default`, which is how it reads both `apply` and `inject`. A
// `export default apply` would drop the `inject` binding and fail with
// "cannot get property ... without inject".
