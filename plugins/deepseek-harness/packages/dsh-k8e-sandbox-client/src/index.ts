/**
 * Phase 1 k8e-sandbox transport: shell out to `k8e-sandbox-cli`, reusing its
 * mTLS, profile, and session-state handling. Each operation is one process
 * spawn; Phase 2 replaces this with a direct gRPC client for streaming
 * fidelity (KIP-20).
 * @module @k8e-sandbox/dsh-k8e-sandbox-client
 */

import { spawn } from 'node:child_process'
import { readFileSync } from 'node:fs'
import { homedir } from 'node:os'
import { join } from 'node:path'

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
  /** Present when the gateway's ListFiles reports entry facts (KIP-20 perf). */
  type?: 'file' | 'directory' | 'symlink' | 'other'
  size?: number
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
  /** Default wall-clock budget for one CLI invocation (ms); the child is killed on expiry. */
  timeoutMs?: number
}

/** Options for a sandbox `run`/`runBackground` call (shared transport contract). */
export interface RunOptions {
  lang?: string
  timeout?: number
  sessionId?: string
  tenant?: string
}

/** One live gateway-proxied exposure (KIP-24). */
export interface ExposedServiceInfo {
  port: number
  url: string
  host: string
  startedAt: number
}

/**
 * Shared op surface implemented by both transports (CLI-backed and direct
 * gRPC). `K8eSandboxRuntime.getClient()` returns whichever is active; the fs
 * / tool / subprocess providers call only these methods.
 */
export interface SandboxTransport {
  run(code: string, opts?: RunOptions): Promise<ExecResult>
  runBackground(code: string, opts?: RunOptions): Promise<BackgroundResult>
  poll(runId: string): Promise<PollResult>
  read(sessionId: string, path: string): Promise<string>
  write(sessionId: string, path: string, content: string): Promise<void>
  list(sessionId: string, since?: number): Promise<FileEntry[]>
  createSession(opts?: CreateSessionOptions): Promise<{ sessionId: string; podIp: string }>
  destroySession(sessionId: string): Promise<void>
  status(): Promise<StatusResult>
  // KIP-24: expose in-sandbox services through the k8e API Gateway.
  exposeService(sessionId: string, port: number, host?: string): Promise<{ url: string }>
  unexposeService(sessionId: string, port: number): Promise<{ ok: boolean }>
  listExposed(sessionId: string): Promise<ExposedServiceInfo[]>
  updateAllowedHosts(sessionId: string, hosts: string[]): Promise<string[]>
  close?(): void
}

// ── Profile resolution (mirrors pkg/sandboxcli/profile.go) ───────────────────

interface SandboxProfile {
  endpoint: string
  certDir: string
  deviceName: string
}

interface SandboxProfileFile {
  currentProfile: string
  profiles: Record<string, SandboxProfile>
}

/** Candidate profile file paths in CLI priority order. */
function profileConfigPaths(): string[] {
  const paths: string[] = []
  const explicit = process.env.K8E_SANDBOX_CONFIG
  if (explicit !== undefined && explicit !== '') paths.push(explicit)
  const certDir = process.env.K8E_SANDBOX_CERT_DIR
  const defaultDir = join(homedir(), '.k8e', 'sandbox')
  paths.push(join(certDir ?? defaultDir, 'profiles.yaml'))
  if (certDir === undefined) paths.push(join(defaultDir, 'profiles.yaml'))
  return [...new Set(paths)]
}

/**
 * Parse the k8e profiles.yaml subset (KIP-17): scalar `key: value` lines plus
 * one level of 2-space-indented maps. Avoids a YAML dependency for a file
 * whose shape the CLI writes.
 *
 *   version: 1
 *   current_profile: default
 *   profiles:
 *     default:
 *       endpoint: host:50051
 *       cert_dir: ""
 */
function parseProfilesYaml(text: string): SandboxProfileFile {
  const result: SandboxProfileFile = { currentProfile: '', profiles: {} }
  let current: SandboxProfile | undefined
  for (const rawLine of text.split('\n')) {
    const line = rawLine.replace(/\r$/, '')
    const trimmed = line.trim()
    if (trimmed === '' || trimmed.startsWith('#')) continue
    const indent = line.length - line.trimStart().length
    const m = /^([A-Za-z0-9_-]+):\s*(.*)$/.exec(trimmed)
    if (m === null) continue
    // Indent 0: top-level scalars (`current_profile`, `version`, `profiles:`).
    if (indent === 0) {
      if (m[1] === 'current_profile') result.currentProfile = m[2]!.replace(/^"(.*)"$/, '$1').trim()
      continue
    }
    // Indent 2 with empty value: a profile name opens a new profile map.
    if (indent === 2 && m[2] === '') {
      current = { endpoint: '', certDir: '', deviceName: '' }
      result.profiles[m[1]!] = current
      continue
    }
    // Indent 4+: profile fields.
    if (current === undefined) continue
    const value = m[2]!.replace(/^"(.*)"$/, '$1').trim()
    if (m[1] === 'endpoint') current.endpoint = value
    else if (m[1] === 'cert_dir') current.certDir = value
    else if (m[1] === 'device_name') current.deviceName = value
  }
  return result
}

/**
 * Resolve the effective endpoint + cert dir for the sandbox gateway
 * (explicit → env → profile → undefined), mirroring `k8e-sandbox-cli`
 * (KIP-17). Returns undefined when no endpoint can be resolved (local
 * auto-discovery territory).
 */
export function resolveSandboxTransport(
  explicit?: { endpoint?: string; certDir?: string; profile?: string },
): { endpoint: string; certDir?: string; source: 'config' | 'env' | 'profile'; profile?: string } | undefined {
  const endpoint = explicit?.endpoint ?? process.env.K8E_SANDBOX_ENDPOINT
  if (endpoint !== undefined && endpoint !== '') {
    const out: { endpoint: string; certDir?: string; source: 'config' | 'env' | 'profile'; profile?: string } = {
      endpoint,
      source: explicit?.endpoint !== undefined && explicit.endpoint !== '' ? 'config' : 'env',
    }
    const certDir = explicit?.certDir ?? process.env.K8E_SANDBOX_CERT_DIR
    if (certDir !== undefined && certDir !== '') out.certDir = certDir
    return out
  }
  const profileName = explicit?.profile ?? process.env.K8E_SANDBOX_PROFILE
  let file: SandboxProfileFile | undefined
  for (const path of profileConfigPaths()) {
    try {
      file = parseProfilesYaml(readFileSync(path, 'utf8'))
      break
    } catch {
      // missing / invalid profile file → try the next candidate
    }
  }
  if (file === undefined) return undefined
  // Mirror pkg/sandboxcli SelectProfileName: explicit → env → current_profile
  // (trimmed, empty treated as unset) → "default" when present. An empty
  // current_profile must NOT short-circuit the documented default fallback.
  const explicitProfile = profileName !== undefined && profileName.trim() !== '' ? profileName.trim() : undefined
  const currentProfile = file.currentProfile !== undefined && file.currentProfile.trim() !== '' ? file.currentProfile.trim() : undefined
  const selected = explicitProfile ?? currentProfile ?? (file.profiles?.default !== undefined ? 'default' : undefined)
  if (selected === undefined) return undefined
  const profile = file.profiles?.[selected]
  if (profile === undefined || profile.endpoint === undefined || profile.endpoint === '') return undefined
  const out: { endpoint: string; certDir?: string; source: 'config' | 'env' | 'profile'; profile?: string } = {
    endpoint: profile.endpoint,
    source: 'profile',
    ...(selected !== undefined ? { profile: selected } : {}),
  }
  if (profile.certDir !== undefined && profile.certDir !== '') out.certDir = profile.certDir
  return out
}

interface CliResult {
  stdout: string
  stderr: string
  exitCode: number
}

function runCli(args: string[], opts: CliK8eClientOptions, stdin?: string, timeoutMs?: number): Promise<CliResult> {
  return new Promise((resolve, reject) => {
    const full: string[] = []
    if (opts.profile) full.push('--profile', opts.profile)
    if (opts.endpoint) full.push('--endpoint', opts.endpoint)
    full.push(...args)

    const child = spawn(opts.bin ?? 'k8e-sandbox-cli', full, { stdio: ['pipe', 'pipe', 'pipe'] })
    let stdout = ''
    let stderr = ''
    const budget = timeoutMs ?? opts.timeoutMs ?? 90_000
    const timer = setTimeout(() => {
      child.kill('SIGKILL')
      reject(new Error(`k8e-sandbox-cli ${args[0] ?? ''} timed out after ${Math.round(budget / 1000)}s (dial + RPC deadline)`))
    }, budget)
    timer.unref?.()
    child.stdout.on('data', (chunk: Buffer) => { stdout += chunk.toString() })
    child.stderr.on('data', (chunk: Buffer) => { stderr += chunk.toString() })
    child.on('error', (err) => { clearTimeout(timer); reject(err) })
    child.on('close', (code) => {
      clearTimeout(timer)
      resolve({ stdout, stderr, exitCode: code ?? -1 })
    })
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
export class CliK8eClient implements SandboxTransport {
  constructor(private readonly opts: CliK8eClientOptions = {}) {}

  /** Run one command in the sandbox and collect stdout/stderr/exit code. */
  async run(code: string, opts: RunOptions = {}): Promise<ExecResult> {
    const args = ['run', code]
    if (opts.lang) args.push('--lang', opts.lang)
    if (opts.timeout !== undefined) args.push('--timeout', String(opts.timeout))
    if (opts.sessionId) args.push('--session-id', opts.sessionId)
    if (opts.tenant) args.push('--tenant', opts.tenant)
    // Wall-clock budget = sandbox timeout + dial/RPC slack, so a dead gateway
    // fails fast instead of hanging forever on context.Background().
    const budgetMs = (opts.timeout !== undefined ? opts.timeout : 30) * 1000 + 20_000
    return parseJSON<ExecResult>(await runCli(args, this.opts, undefined, budgetMs), 'run')
  }

  /** Submit asynchronously; returns a run id to poll. */
  async runBackground(code: string, opts: RunOptions = {}): Promise<BackgroundResult> {
    const args = ['run', code, '--background']
    if (opts.lang) args.push('--lang', opts.lang)
    if (opts.sessionId) args.push('--session-id', opts.sessionId)
    if (opts.tenant) args.push('--tenant', opts.tenant)
    return parseJSON<BackgroundResult>(await runCli(args, this.opts, undefined, 20_000), 'run --background')
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

  // ── KIP-24 service exposure (CLI-backed) ─────────────────────────────────

  async exposeService(sessionId: string, port: number, host?: string): Promise<{ url: string }> {
    const args = ['expose', String(port), '--session-id', sessionId]
    if (host !== undefined && host !== '') args.push('--host', host)
    return parseJSON<{ url: string }>(await runCli(args, this.opts), 'expose')
  }

  async unexposeService(sessionId: string, port: number): Promise<{ ok: boolean }> {
    return parseJSON<{ ok: boolean }>(await runCli(['unexpose', String(port), '--session-id', sessionId], this.opts), 'unexpose')
  }

  async listExposed(sessionId: string): Promise<ExposedServiceInfo[]> {
    const out = await parseJSON<{ services: ExposedServiceInfo[] }>(await runCli(['exposed', '--session-id', sessionId], this.opts), 'exposed')
    return out.services ?? []
  }

  async updateAllowedHosts(sessionId: string, hosts: string[]): Promise<string[]> {
    const args = hosts.length > 0
      ? ['allow-hosts', hosts.join(','), '--session-id', sessionId]
      : ['allow-hosts', '--clear', '--session-id', sessionId]
    const out = await parseJSON<{ allowed_hosts: string[] }>(await runCli(args, this.opts), 'allow-hosts')
    return out.allowed_hosts ?? []
  }
}
