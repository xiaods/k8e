/**
 * Phase 1 k8e-sandbox transport: shell out to `k8e-sandbox-cli`, reusing its
 * mTLS, profile, and session-state handling. Each operation is one process
 * spawn; Phase 2 replaces this with a direct gRPC client for streaming
 * fidelity (KIP-20).
 * @module @k8e/dsh-k8e-sandbox-client
 */

import { spawn } from 'node:child_process'

/** Result of a single-shot sandbox exec (`k8e-sandbox-cli run`). */
export interface ExecResult {
  stdout: string
  stderr: string
  exitCode: number
  sessionId: string
  status: string
  durationMs: number
  truncated: boolean
  language: string
}

/** Result of a background submit (`run --background`). */
export interface BackgroundResult {
  runId: string
  status: string
  sessionId: string
}

/** Result of a poll (`k8e-sandbox-cli poll <run-id>`). */
export interface PollResult {
  runId: string
  status: string
  stdout: string
  stderr: string
  exitCode: number
  durationMs: number
  truncated: boolean
}

export interface FileEntry {
  path: string
  modified: number
}

export interface CreateSessionOptions {
  runtimeClass?: string
  tenant?: string
  allowedHosts?: string[]
}

export interface StatusResult {
  available: boolean
  sessionId: string
  tenantId: string
  error: string
}

/** Options for the CLI-backed transport. */
export interface CliK8eClientOptions {
  /** Path to the k8e-sandbox-cli binary; defaults to `k8e-sandbox-cli` on PATH. */
  bin?: string
  /** Named profile from ~/.k8e/sandbox/profiles.yaml (KIP-17). */
  profile?: string
  /** gRPC gateway endpoint override. */
  endpoint?: string
}

interface CliResult {
  stdout: string
  stderr: string
  exitCode: number
}

function runCli(args: string[], opts: CliK8eClientOptions, stdin?: string): Promise<CliResult> {
  return new Promise((resolve, reject) => {
    const full: string[] = []
    if (opts.profile) full.push('--profile', opts.profile)
    if (opts.endpoint) full.push('--endpoint', opts.endpoint)
    full.push(...args)

    const child = spawn(opts.bin ?? 'k8e-sandbox-cli', full, { stdio: ['pipe', 'pipe', 'pipe'] })
    let stdout = ''
    let stderr = ''
    child.stdout.on('data', (chunk: Buffer) => { stdout += chunk.toString() })
    child.stderr.on('data', (chunk: Buffer) => { stderr += chunk.toString() })
    child.on('error', reject)
    child.on('close', (code) => { resolve({ stdout, stderr, exitCode: code ?? -1 }) })
    if (stdin !== undefined) child.stdin.write(stdin)
    child.stdin.end()
  })
}

function parseJSON<T>(result: CliResult, what: string): T {
  if (result.exitCode !== 0) {
    throw new Error(`k8e-sandbox-cli ${what} failed (exit ${result.exitCode}): ${result.stderr.trim()}`)
  }
  try {
    return JSON.parse(result.stdout) as T
  } catch (cause) {
    throw new Error(`k8e-sandbox-cli ${what} returned invalid JSON: ${result.stdout}`, { cause })
  }
}

/** CLI-backed k8e-sandbox client. */
export class CliK8eClient {
  constructor(private readonly opts: CliK8eClientOptions = {}) {}

  /** Run one command in the sandbox and collect stdout/stderr/exit code. */
  async run(code: string, opts: { lang?: string; timeout?: number; sessionId?: string; tenant?: string } = {}): Promise<ExecResult> {
    const args = ['run', code]
    if (opts.lang) args.push('--lang', opts.lang)
    if (opts.timeout !== undefined) args.push('--timeout', String(opts.timeout))
    if (opts.sessionId) args.push('--session-id', opts.sessionId)
    if (opts.tenant) args.push('--tenant', opts.tenant)
    return parseJSON<ExecResult>(await runCli(args, this.opts), 'run')
  }

  /** Submit asynchronously; returns a run id to poll. */
  async runBackground(code: string, opts: { lang?: string; sessionId?: string; tenant?: string } = {}): Promise<BackgroundResult> {
    const args = ['run', code, '--background']
    if (opts.lang) args.push('--lang', opts.lang)
    if (opts.sessionId) args.push('--session-id', opts.sessionId)
    if (opts.tenant) args.push('--tenant', opts.tenant)
    return parseJSON<BackgroundResult>(await runCli(args, this.opts), 'run --background')
  }

  async poll(runId: string): Promise<PollResult> {
    return parseJSON<PollResult>(await runCli(['poll', runId], this.opts), 'poll')
  }

  async write(sessionId: string, path: string, content: string): Promise<void> {
    await parseJSON<{ ok: boolean }>(await runCli(['write', sessionId, path], this.opts, content), 'write')
  }

  async read(sessionId: string, path: string): Promise<string> {
    const result = await runCli(['read', sessionId, path, '--raw'], this.opts)
    if (result.exitCode !== 0) {
      throw new Error(`k8e-sandbox-cli read failed (exit ${result.exitCode}): ${result.stderr.trim()}`)
    }
    return result.stdout
  }

  async list(sessionId: string, since?: number): Promise<FileEntry[]> {
    const args = ['list', sessionId]
    if (since !== undefined) args.push('--since', String(since))
    const out = await parseJSON<{ files: FileEntry[] }>(await runCli(args, this.opts), 'list')
    return out.files ?? []
  }

  async createSession(opts: CreateSessionOptions = {}): Promise<{ sessionId: string; podIp: string }> {
    const args = ['create']
    if (opts.runtimeClass) args.push('--runtime', opts.runtimeClass)
    if (opts.tenant) args.push('--tenant', opts.tenant)
    if (opts.allowedHosts?.length) args.push('--allowed-hosts', opts.allowedHosts.join(','))
    return parseJSON<{ sessionId: string; podIp: string }>(await runCli(args, this.opts), 'create')
  }

  async destroySession(sessionId: string): Promise<void> {
    await parseJSON<{ ok: boolean }>(await runCli(['destroy', sessionId], this.opts), 'destroy')
  }

  async status(): Promise<StatusResult> {
    return parseJSON<StatusResult>(await runCli(['status'], this.opts), 'status')
  }
}
