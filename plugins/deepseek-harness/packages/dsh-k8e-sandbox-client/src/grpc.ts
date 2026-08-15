/**
 * Phase 2 k8e-sandbox transport: a direct gRPC client (mTLS) for the terminal
 * primitive. Loads the bundled `sandbox.proto` at runtime via
 * @grpc/proto-loader; mTLS material comes from the CLI's cert dir
 * (~/.k8e/sandbox, KIP-14/KIP-17), shared with `k8e-sandbox-cli`.
 * @module @k8e/dsh-k8e-sandbox-client/grpc
 */

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
}

// Dynamic client surface produced by proto-loader; typed loosely here.
interface SandboxServiceClient {
  createTerminal(request: unknown, metadata: grpc.Metadata, cb: (err: grpc.ServiceError | null, resp: any) => void): grpc.ClientUnaryCall
  terminalWrite(request: unknown, metadata: grpc.Metadata, cb: (err: grpc.ServiceError | null, resp: any) => void): grpc.ClientUnaryCall
  terminalResize(request: unknown, metadata: grpc.Metadata, cb: (err: grpc.ServiceError | null, resp: any) => void): grpc.ClientUnaryCall
  terminalForeground(request: unknown, metadata: grpc.Metadata, cb: (err: grpc.ServiceError | null, resp: any) => void): grpc.ClientUnaryCall
  terminalSignal(request: unknown, metadata: grpc.Metadata, cb: (err: grpc.ServiceError | null, resp: any) => void): grpc.ClientUnaryCall
  terminalDestroy(request: unknown, metadata: grpc.Metadata, cb: (err: grpc.ServiceError | null, resp: any) => void): grpc.ClientUnaryCall
  terminalStream(request: unknown, metadata: grpc.Metadata): grpc.ClientReadableStream<any>
  execStream(request: unknown, metadata: grpc.Metadata): grpc.ClientReadableStream<any>
  exec(request: unknown, metadata: grpc.Metadata, cb: (err: grpc.ServiceError | null, resp: any) => void): grpc.ClientUnaryCall
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

/**
 * Direct gRPC client for the sandbox gateway's PTY terminal primitive. Phase 2
 * uses this for `spawnTerminal`; Phase 1 shells out to the CLI for the rest.
 */
export class GrpcK8eClient {
  private readonly client: SandboxServiceClient
  private readonly metadata = new grpc.Metadata()

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
    this.client = new SandboxService(opts.endpoint, createCredentials(opts.certDir ?? defaultCertDir())) as SandboxServiceClient
  }

  private call<T>(
    method: (req: unknown, md: grpc.Metadata, cb: (err: grpc.ServiceError | null, resp: any) => void) => grpc.ClientUnaryCall,
    request: unknown,
  ): Promise<T> {
    return new Promise((resolve, reject) => {
      method.call(this.client, request, this.metadata, (err, resp) => {
        if (err !== null) reject(err)
        else resolve(resp as T)
      })
    })
  }

  async createTerminal(request: { sessionId: string; argv: string[]; workdir?: string; env?: Record<string, string>; rows: number; cols: number }): Promise<CreateTerminalResponse> {
    const resp = await this.call<any>(this.client.createTerminal, {
      session_id: request.sessionId,
      argv: request.argv,
      workdir: request.workdir ?? '/workspace',
      env: request.env ?? {},
      rows: request.rows,
      cols: request.cols,
    })
    return { terminalId: resp.terminal_id as string, pid: resp.pid as number }
  }

  async terminalWrite(terminalId: string, data: Uint8Array): Promise<void> {
    await this.call(this.client.terminalWrite, { terminal_id: terminalId, data })
  }

  async terminalResize(terminalId: string, rows: number, cols: number): Promise<void> {
    await this.call(this.client.terminalResize, { terminal_id: terminalId, rows, cols })
  }

  async terminalForeground(terminalId: string): Promise<TerminalForegroundResponse> {
    const resp = await this.call<any>(this.client.terminalForeground, { terminal_id: terminalId })
    return { processGroupId: resp.process_group_id as number, inputWaiting: resp.input_waiting as boolean }
  }

  async terminalSignal(terminalId: string, signal: TerminalSignal): Promise<number> {
    const resp = await this.call<any>(this.client.terminalSignal, {
      terminal_id: terminalId,
      signal: terminalSignalEnum(signal),
    })
    return resp.process_group_id as number
  }

  async terminalDestroy(terminalId: string, graceMs: number): Promise<void> {
    await this.call(this.client.terminalDestroy, { terminal_id: terminalId, grace_ms: graceMs })
  }

  /** Open the terminal output stream; the stream yields data frames then a final exit frame. */
  terminalStream(terminalId: string): grpc.ClientReadableStream<any> {
    return this.client.terminalStream({ terminal_id: terminalId }, this.metadata)
  }

  /**
   * Run one command with streaming merged output. The gateway forwards
   * sandboxd's SSE (`data: {"pid":N}`, `data: <raw>`, `data: {"exit":N}`) as
   * raw chunks; this decodes them into a clean stdout stream plus an exit
   * promise.
   */
  execStream(sessionId: string, command: string): ExecStreamResult {
    const stdout = new PassThrough()
    const stream = this.client.execStream({ session_id: sessionId, command }, this.metadata)

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
