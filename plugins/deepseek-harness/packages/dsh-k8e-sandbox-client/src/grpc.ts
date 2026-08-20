/**
 * Phase 2 k8e-sandbox transport: a direct gRPC client (mTLS) for the full
 * sandbox surface — exec, files, sessions, and the PTY terminal primitive.
 * Loads the bundled `sandbox.proto` at runtime via @grpc/proto-loader; mTLS
 * material comes from the CLI's cert dir (~/.k8e/sandbox, KIP-14/KIP-17),
 * shared with `k8e-sandbox-cli`.
 *
 * One `GrpcK8eClient` instance owns one connection: every operation is a gRPC
 * RPC on that connection instead of spawning a 36MB `k8e-sandbox-cli` process
 * per call. Every unary call carries a deadline so a dead gateway fails fast
 * instead of hanging the chat preflight (KIP-20 perf follow-up).
 * @module @k8e-sandbox/dsh-k8e-sandbox-client/grpc
 */

// Op shapes shared with the CLI-backed transport live in the main entry
// (./index.ts); re-export them here so the gRPC surface stays type-identical
// with the CLI surface without duplicating the declarations.
import type {
  BackgroundResult,
  CreateSessionOptions,
  ExecResult,
  FileEntry,
  PollResult,
  RunOptions,
  StatusResult,
} from './index.ts'
export type {
  BackgroundResult,
  CreateSessionOptions,
  ExecResult,
  FileEntry,
  PollResult,
  RunOptions,
  StatusResult,
} from './index.ts'
import { readFileSync } from 'node:fs'
import { homedir } from 'node:os'
import { join } from 'node:path'
import { PassThrough } from 'node:stream'
import type { Readable } from 'node:stream'
import * as grpc from '@grpc/grpc-js'
import { loadSync } from '@grpc/proto-loader'

/** Terminal signal names shared with sandboxd /pty/signal. */
export type TerminalSignal = 'SIGINT' | 'SIGTERM' | 'SIGKILL' | 'SIGTSTP' | 'SIGHUP'

export interface CreateTerminalResponse {
  terminalId: string
  pid: number
}

export interface TerminalExit {
  exitCode: number
  signal: string
}

export interface TerminalForegroundResponse {
  processGroupId: number
  inputWaiting: boolean
}

export interface ExecStreamResult {
  /** Merged stdout+stderr, decoded from the gateway's SSE stream. */
  stdout: Readable
  /** Resolves when the process exits (exit frame observed or stream closed). */
  done: Promise<{ exitCode: number | null; signal: NodeJS.Signals | null }>
}

export interface ExecSSEEvent {
  data?: string
  exit?: number
}

/** Default deadline for a unary RPC with no explicit budget (ms). */
export const DEFAULT_RPC_DEADLINE_MS = 30_000

// ── Language wrapping (mirrors pkg/sandboxcli buildCommand) ──────────────────

function isMultiLine(code: string): boolean {
  return code.includes('\n')
}

function isInterpretedLang(lang: string | undefined): boolean {
  switch ((lang ?? '').toLowerCase()) {
    case 'python': case 'python3': case 'py':
    case 'node': case 'nodejs': case 'js': case 'javascript':
    case 'ts': case 'typescript':
      return true
    default:
      return false
  }
}

/**
 * Wrap code for a language the same way `k8e-sandbox-cli run` does: bash
 * passes through; python/node single-line uses `-c`/`-e`, multi-line runs a
 * workspace temp file written via WriteFile.
 */
export function buildSandboxCommand(lang: string | undefined, code: string): string {
  switch ((lang ?? 'bash').toLowerCase()) {
    case 'python': case 'python3': case 'py':
      return isMultiLine(code) ? 'python3 /workspace/_k8e_run.py' : `python3 -c ${JSON.stringify(code)}`
    case 'node': case 'nodejs': case 'js': case 'javascript':
      return isMultiLine(code) ? 'node /workspace/_k8e_run.js' : `node -e ${JSON.stringify(code)}`
    case 'ts': case 'typescript':
      return 'TMPDIR=/workspace tsx /workspace/_k8e_run.ts'
    default: // bash / sh
      return code
  }
}

/** Workspace temp file for multi-line interpreted code (mirrors writeCodeFile). */
function runFileFor(lang: string | undefined): string {
  switch ((lang ?? '').toLowerCase()) {
    case 'node': case 'nodejs': case 'js': case 'javascript':
      return '/workspace/_k8e_run.js'
    case 'ts': case 'typescript':
      return '/workspace/_k8e_run.ts'
    default:
      return '/workspace/_k8e_run.py'
  }
}

/**
 * Incremental decoder for the gateway's /exec/stream SSE framing
 * (`data: {"pid":N}`, `data: <raw>`, `data: {"exit":N}`). Chunks may split an
 * event anywhere; `push` returns only complete events.
 */
export class ExecSSEDecoder {
  private buffer = ''

  /** Feed one chunk; returns the decoded data/exit events in delivery order. */
  push(chunk: string): ExecSSEEvent[] {
    this.buffer += chunk
    const events: ExecSSEEvent[] = []
    while (true) {
      const idx = this.buffer.indexOf('\n\n')
      if (idx < 0) break
      const event = this.buffer.slice(0, idx)
      this.buffer = this.buffer.slice(idx + 2)
      if (!event.startsWith('data: ')) continue
      const payload = event.slice('data: '.length)
      if (payload.startsWith('{"pid"')) continue
      if (payload.startsWith('{"exit"')) {
        const m = /"exit":(-?\d+)/.exec(payload)
        events.push({ exit: m === null ? 0 : Number(m[1]) })
        continue
      }
      events.push({ data: payload })
    }
    return events
  }
}

export interface GrpcK8eClientOptions {
  /** gRPC gateway `host:port`. */
  endpoint: string
  /** mTLS material dir holding ca.crt / client.crt / client.key. */
  certDir?: string
  /** Path to the bundled sandbox.proto. */
  protoPath?: string
  /** Default deadline for unary RPCs (ms); dial/reconnect hangs are bounded by this. */
  deadlineMs?: number
}

// Dynamic client surface produced by proto-loader; typed loosely here.
interface SandboxServiceClient {
  createSession(request: unknown, metadata: grpc.Metadata, options: grpc.CallOptions, cb: (err: grpc.ServiceError | null, resp: any) => void): grpc.ClientUnaryCall
  destroySession(request: unknown, metadata: grpc.Metadata, options: grpc.CallOptions, cb: (err: grpc.ServiceError | null, resp: any) => void): grpc.ClientUnaryCall
  exec(request: unknown, metadata: grpc.Metadata, options: grpc.CallOptions, cb: (err: grpc.ServiceError | null, resp: any) => void): grpc.ClientUnaryCall
  pollRun(request: unknown, metadata: grpc.Metadata, options: grpc.CallOptions, cb: (err: grpc.ServiceError | null, resp: any) => void): grpc.ClientUnaryCall
  readFile(request: unknown, metadata: grpc.Metadata, options: grpc.CallOptions, cb: (err: grpc.ServiceError | null, resp: any) => void): grpc.ClientUnaryCall
  writeFile(request: unknown, metadata: grpc.Metadata, options: grpc.CallOptions, cb: (err: grpc.ServiceError | null, resp: any) => void): grpc.ClientUnaryCall
  listFiles(request: unknown, metadata: grpc.Metadata, options: grpc.CallOptions, cb: (err: grpc.ServiceError | null, resp: any) => void): grpc.ClientUnaryCall
  createTerminal(request: unknown, metadata: grpc.Metadata, options: grpc.CallOptions, cb: (err: grpc.ServiceError | null, resp: any) => void): grpc.ClientUnaryCall
  terminalWrite(request: unknown, metadata: grpc.Metadata, options: grpc.CallOptions, cb: (err: grpc.ServiceError | null, resp: any) => void): grpc.ClientUnaryCall
  terminalResize(request: unknown, metadata: grpc.Metadata, options: grpc.CallOptions, cb: (err: grpc.ServiceError | null, resp: any) => void): grpc.ClientUnaryCall
  terminalForeground(request: unknown, metadata: grpc.Metadata, options: grpc.CallOptions, cb: (err: grpc.ServiceError | null, resp: any) => void): grpc.ClientUnaryCall
  terminalSignal(request: unknown, metadata: grpc.Metadata, options: grpc.CallOptions, cb: (err: grpc.ServiceError | null, resp: any) => void): grpc.ClientUnaryCall
  terminalDestroy(request: unknown, metadata: grpc.Metadata, options: grpc.CallOptions, cb: (err: grpc.ServiceError | null, resp: any) => void): grpc.ClientUnaryCall
  terminalStream(request: unknown, metadata: grpc.Metadata): grpc.ClientReadableStream<any>
  execStream(request: unknown, metadata: grpc.Metadata): grpc.ClientReadableStream<any>
}

function defaultCertDir(): string {
  return process.env.K8E_SANDBOX_CERT_DIR ?? join(homedir(), '.k8e', 'sandbox')
}

function createCredentials(certDir: string): grpc.ChannelCredentials {
  const read = (name: string): Buffer | undefined => {
    try {
      return readFileSync(join(certDir, name))
    } catch {
      return undefined
    }
  }
  const caCert = read('ca.crt')
  const clientCert = read('client.crt')
  const clientKey = read('client.key')
  if (caCert !== undefined && clientCert !== undefined && clientKey !== undefined) {
    return grpc.credentials.createSsl(caCert, clientKey, clientCert)
  }
  if (caCert !== undefined) {
    return grpc.credentials.createSsl(caCert)
  }
  return grpc.credentials.createSsl()
}

function terminalSignalEnum(signal: TerminalSignal): number {
  switch (signal) {
    case 'SIGINT': return 1
    case 'SIGTERM': return 2
    case 'SIGKILL': return 3
    case 'SIGTSTP': return 4
    case 'SIGHUP': return 5
  }
}

type UnaryMethod = (
  request: unknown,
  metadata: grpc.Metadata,
  options: grpc.CallOptions,
  cb: (err: grpc.ServiceError | null, resp: any) => void,
) => grpc.ClientUnaryCall

/**
 * Direct gRPC client for the sandbox gateway. One instance owns one
 * connection; all fs/exec/session ops are RPCs on it (no per-op CLI spawn).
 * Every unary call carries a deadline so a dead gateway fails fast.
 */
export class GrpcK8eClient {
  private readonly client: SandboxServiceClient
  private readonly metadata = new grpc.Metadata()
  private readonly deadlineMs: number

  constructor(private readonly opts: GrpcK8eClientOptions) {
    const protoPath = opts.protoPath ?? join(import.meta.dirname, '..', 'proto', 'sandbox.proto')
    const definition = loadSync(protoPath, {
      keepCase: false,
      longs: String,
      enums: String,
      defaults: true,
      oneofs: true,
    })
    const pkg = grpc.loadPackageDefinition(definition) as any
    const SandboxService = pkg.sandbox.v1.SandboxService
    this.deadlineMs = opts.deadlineMs ?? DEFAULT_RPC_DEADLINE_MS
    // Fast-fail reconnect: a dead endpoint must surface a per-call deadline
    // quickly instead of sitting in grpc-js exponential backoff.
    this.client = new SandboxService(opts.endpoint, createCredentials(opts.certDir ?? defaultCertDir()), {
      'grpc.initial_reconnect_backoff_ms': 200,
      'grpc.max_reconnect_backoff_ms': 2_000,
    }) as SandboxServiceClient
  }

  private call<T>(method: UnaryMethod, request: unknown, deadlineMs?: number): Promise<T> {
    return new Promise((resolve, reject) => {
      const deadline = Date.now() + (deadlineMs ?? this.deadlineMs)
      method.call(this.client, request, this.metadata, { deadline }, (err, resp) => {
        if (err !== null) reject(err)
        else resolve(resp as T)
      })
    })
  }

  // ── Sessions ──────────────────────────────────────────────────────────────

  async createSession(opts: CreateSessionOptions = {}): Promise<{ sessionId: string; podIp: string }> {
    const resp = await this.call<any>(this.client.createSession, {
      sessionId: '',
      ...(opts.tenant !== undefined ? { tenantId: opts.tenant } : {}),
      ...(opts.allowedHosts !== undefined ? { allowedHosts: opts.allowedHosts } : {}),
      ...(opts.runtimeClass !== undefined ? { runtimeClass: opts.runtimeClass } : {}),
    }, 45_000)
    return { sessionId: resp.sessionId as string, podIp: resp.podIp as string }
  }

  async destroySession(sessionId: string): Promise<void> {
    await this.call(this.client.destroySession, { sessionId: sessionId }, 10_000)
  }

  /**
   * Lightweight availability probe (mirrors `k8e-sandbox-cli status`): a noop
   * DestroySession RPC that completes the handshake; NotFound means the
   * gateway is up (unknown id), any other error means unavailable.
   */
  async status(): Promise<StatusResult> {
    try {
      await this.call(this.client.destroySession, { sessionId: 'healthcheck-probe-noop' }, 5_000)
      return { available: true, sessionId: '', tenantId: '', error: '' }
    } catch (err) {
      // The gateway wraps the noop session lookup in Internal with a
      // "not found" message (mirroring k8e-sandbox-cli's errSessionNotFound
      // text match); either signal means the handshake + auth succeeded.
      const code = (err as { code?: number }).code
      if (code === grpc.status.NOT_FOUND || (err instanceof Error && err.message.includes('not found'))) {
        return { available: true, sessionId: '', tenantId: '', error: '' }
      }
      return { available: false, sessionId: '', tenantId: '', error: err instanceof Error ? err.message : String(err) }
    }
  }

  // ── Exec ──────────────────────────────────────────────────────────────────

  /**
   * Write multi-line interpreted code to its workspace temp file and return the
   * shell command to run it (mirrors pkg/sandboxcli writeCodeFile + buildCommand).
   */
  private async prepareCommand(sessionId: string, code: string, lang: string): Promise<string> {
    if (isMultiLine(code) && isInterpretedLang(lang)) {
      await this.write(sessionId, runFileFor(lang), code)
    }
    return buildSandboxCommand(lang, code)
  }

  /**
   * Execute a command in the sandbox over the shared connection and return the
   * collected output. Mirrors `k8e-sandbox-cli run`; the deadline covers the
   * dial plus sandbox execution (sandbox timeout + 15s slack).
   */
  async run(code: string, opts: RunOptions = {}): Promise<ExecResult> {
    if (opts.sessionId === undefined) throw new Error('k8e sandbox grpc: run requires a sessionId')
    const lang = opts.lang ?? 'bash'
    const command = await this.prepareCommand(opts.sessionId, code, lang)
    const timeout = opts.timeout ?? 30
    const resp = await this.call<any>(this.client.exec, {
      sessionId: opts.sessionId,
      command,
      timeout,
      workdir: '/workspace',
      background: false,
      language: lang,
    }, timeout * 1000 + 15_000)
    return {
      stdout: resp.stdout as string,
      stderr: resp.stderr as string,
      exitCode: resp.exitCode as number,
      sessionId: resp.sessionId as string,
      status: resp.status as string,
      durationMs: Number(resp.durationMs as string) || 0,
      truncated: resp.truncated as boolean,
      language: resp.language as string,
    }
  }

  /** Submit asynchronously; returns a run id to poll. */
  async runBackground(code: string, opts: RunOptions = {}): Promise<BackgroundResult> {
    if (opts.sessionId === undefined) throw new Error('k8e sandbox grpc: runBackground requires a sessionId')
    const lang = opts.lang ?? 'bash'
    const command = await this.prepareCommand(opts.sessionId, code, lang)
    const resp = await this.call<any>(this.client.exec, {
      sessionId: opts.sessionId,
      command,
      timeout: 0,
      workdir: '/workspace',
      background: true,
      language: lang,
    }, 15_000)
    return { runId: resp.runId as string, status: resp.status as string, sessionId: opts.sessionId }
  }

  async poll(runId: string): Promise<PollResult> {
    const resp = await this.call<any>(this.client.pollRun, { runId: runId }, 15_000)
    return {
      runId: resp.runId as string,
      status: resp.status as string,
      stdout: resp.stdout as string,
      stderr: resp.stderr as string,
      exitCode: resp.exitCode as number,
      durationMs: Number(resp.durationMs as string) || 0,
      truncated: resp.truncated as boolean,
    }
  }

  // ── Files ─────────────────────────────────────────────────────────────────

  async read(sessionId: string, path: string): Promise<string> {
    const resp = await this.call<any>(this.client.readFile, { sessionId: sessionId, path }, 15_000)
    return resp.content as string
  }

  async write(sessionId: string, path: string, content: string): Promise<void> {
    await this.call(this.client.writeFile, { sessionId: sessionId, path, content }, 15_000)
  }

  /** List workspace files; the gateway may include type/size per entry. */
  async list(sessionId: string, since?: number): Promise<FileEntry[]> {
    const resp = await this.call<any>(this.client.listFiles, {
      sessionId: sessionId,
      ...(since !== undefined ? { since } : {}),
    }, 15_000)
    const out: FileEntry[] = []
    for (const f of (resp.files as any[]) ?? []) {
      const entry: FileEntry = { path: f.path as string, modified: Number(f.modified ?? 0) }
      const rawType = f.type as FileEntry['type'] | undefined
      if (rawType !== undefined) entry.type = rawType
      if (f.size !== undefined) entry.size = Number(f.size)
      out.push(entry)
    }
    return out
  }

  // ── Terminal ──────────────────────────────────────────────────────────────

  async createTerminal(request: { sessionId: string; argv: string[]; workdir?: string; env?: Record<string, string>; rows: number; cols: number }): Promise<CreateTerminalResponse> {
    const resp = await this.call<any>(this.client.createTerminal, {
      sessionId: request.sessionId,
      argv: request.argv,
      workdir: request.workdir ?? '/workspace',
      env: request.env ?? {},
      rows: request.rows,
      cols: request.cols,
    }, 15_000)
    return { terminalId: resp.terminalId as string, pid: resp.pid as number }
  }

  async terminalWrite(terminalId: string, data: Uint8Array): Promise<void> {
    await this.call(this.client.terminalWrite, { terminalId: terminalId, data }, 10_000)
  }

  async terminalResize(terminalId: string, rows: number, cols: number): Promise<void> {
    await this.call(this.client.terminalResize, { terminalId: terminalId, rows, cols }, 10_000)
  }

  async terminalForeground(terminalId: string): Promise<TerminalForegroundResponse> {
    const resp = await this.call<any>(this.client.terminalForeground, { terminalId: terminalId }, 10_000)
    return { processGroupId: resp.processGroupId as number, inputWaiting: resp.inputWaiting as boolean }
  }

  async terminalSignal(terminalId: string, signal: TerminalSignal): Promise<number> {
    const resp = await this.call<any>(this.client.terminalSignal, {
      terminalId: terminalId,
      signal: terminalSignalEnum(signal),
    }, 10_000)
    return resp.processGroupId as number
  }

  async terminalDestroy(terminalId: string, graceMs: number): Promise<void> {
    await this.call(this.client.terminalDestroy, { terminalId: terminalId, graceMs: graceMs }, 10_000)
  }

  /** Open the terminal output stream; the stream yields data frames then a final exit frame. */
  terminalStream(terminalId: string): grpc.ClientReadableStream<any> {
    return this.client.terminalStream({ terminalId: terminalId }, this.metadata)
  }

  /**
   * Run one command with streaming merged output. The gateway forwards
   * sandboxd's SSE (`data: {"pid":N}`, `data: <raw>`, `data: {"exit":N}`) as
   * raw chunks; this decodes them into a clean stdout stream plus an exit
   * promise.
   */
  execStream(sessionId: string, command: string): ExecStreamResult {
    const stdout = new PassThrough()
    const stream = this.client.execStream({ sessionId: sessionId, command }, this.metadata)

    const decoder = new ExecSSEDecoder()
    let exitCode: number | null = null
    let settled = false

    const done = new Promise<{ exitCode: number | null; signal: NodeJS.Signals | null }>((resolve, reject) => {
      const settle = (signal: NodeJS.Signals | null): void => {
        stdout.end()
        if (!settled) {
          settled = true
          resolve({ exitCode, signal })
        }
      }
      stream.on('data', (chunk: Buffer | string) => {
        const text = typeof chunk === 'string' ? chunk : chunk.toString('utf8')
        for (const event of decoder.push(text)) {
          if (event.exit !== undefined) {
            exitCode = event.exit
            settle(null)
          } else if (event.data !== undefined) {
            stdout.write(event.data)
          }
        }
      })
      stream.on('error', (err) => {
        stdout.destroy(err instanceof Error ? err : new Error(String(err)))
        if (!settled) {
          settled = true
          reject(err instanceof Error ? err : new Error(String(err)))
        }
      })
      stream.on('end', () => settle(null))
    })

    return { stdout, done }
  }

  close(): void {
    ;(this.client as unknown as grpc.Client).close()
  }
}
