/**
 * Shared ownership of one k8e-sandbox session. Capability adapters await the
 * same session handle, so filesystem and process operations inhabit one remote
 * Linux world (mirrors the E2B POC's `ctx.e2b` owner).
 *
 * Transport: when an endpoint is configured (explicit, env, or the CLI
 * profiles.yaml KIP-17), a persistent `GrpcK8eClient` owns one connection and
 * every fs/exec op is a gRPC RPC on it — no per-op `k8e-sandbox-cli` spawn.
 * Without an endpoint the CLI-backed transport is used (local auto-discovery).
 * Session creation is single-flighted and failures are negatively cached so a
 * dead gateway cannot cause a getSession() storm.
 * @module @k8e-sandbox/dsh-k8e-sandbox
 */

import { Context, Service } from '@deepseek-ai/cordis'
import z from '@deepseek-ai/schemastery'
import { CliK8eClient, resolveSandboxTransport, type SandboxTransport } from '@k8e-sandbox/dsh-k8e-sandbox-client'
import { GrpcK8eClient } from '@k8e-sandbox/dsh-k8e-sandbox-client/grpc'

/** Configuration for the shared k8e-sandbox owner. */
export interface Config {
  /** gRPC gateway endpoint; unset resolves via env → ~/.k8e/sandbox/profiles.yaml (KIP-17). */
  endpoint?: string
  /** Named profile from ~/.k8e/sandbox/profiles.yaml (KIP-17). */
  profile?: string
  /** mTLS material dir for the direct gRPC terminal client. */
  certDir?: string
  /** Remote working directory shared by provider adapters. */
  cwd?: string
  /** RuntimeClass for the session pod. */
  runtimeClass?: string
  /** Explicit cross-process session reuse tenant; unset = one session per dsh session. */
  tenant?: string
  /** Egress allowlist (only applied when the owner creates the session). */
  allowedHosts?: string[]
  /** Dispose by pausing instead of destroying (PVC retained); Phase 1 destroy-only. */
  pauseOnDispose?: boolean
  /** How long a failed session creation stays negatively cached (ms). */
  sessionFailureTtlMs?: number
}

interface ResolvedConfig {
  cwd: string
  runtimeClass: string
  tenant?: string
  allowedHosts?: string[]
  pauseOnDispose: boolean
  sessionFailureTtlMs: number
}

interface SchemaResolvedConfig extends Config {
  cwd: string
  runtimeClass: string
  pauseOnDispose: boolean
}

declare module '@deepseek-ai/cordis' {
  interface Context {
    k8eSandbox: K8eSandboxRuntime
  }
}

/** Creates one lazily consumable k8e-sandbox session and destroys it on disposal. */
export class K8eSandboxRuntime extends Service {
  static Config: z<Config> = z.object({
    endpoint: z.string(),
    profile: z.string(),
    certDir: z.string(),
    cwd: z.string().default('/workspace'),
    runtimeClass: z.string().default('gvisor'),
    tenant: z.string(),
    allowedHosts: z.array(z.string()),
    pauseOnDispose: z.boolean().default(false),
    sessionFailureTtlMs: z.number().default(10_000),
  })

  /** Validated remote working directory shared by provider adapters. */
  readonly cwd: string
  /** gRPC gateway endpoint (undefined = CLI local auto-discovery). */
  readonly endpoint: string | undefined
  /** Where the endpoint came from: config | env | profile | undefined. */
  readonly endpointSource: 'config' | 'env' | 'profile' | undefined
  /** Selected profile name when the endpoint came from profiles.yaml (KIP-17). */
  readonly endpointProfile: string | undefined
  /** mTLS material dir for the direct gRPC terminal client. */
  readonly certDir: string | undefined
  /** RuntimeClass for the session pod. */
  readonly runtimeClass: string

  private readonly config: ResolvedConfig
  private readonly transport: SandboxTransport
  private readonly grpcClient: GrpcK8eClient | undefined
  private sessionId: string | undefined
  private sessionInFlight: Promise<string> | undefined
  private sessionUnavailableUntil = 0
  private disposed = false

  constructor(ctx: Context, config: Config) {
    super(ctx, 'k8eSandbox')
    // Schemastery fills defaulted fields before construction; the type does not encode that step.
    const resolved = config as SchemaResolvedConfig
    this.config = {
      cwd: resolved.cwd,
      runtimeClass: resolved.runtimeClass,
      ...(config.tenant !== undefined ? { tenant: config.tenant } : {}),
      ...(config.allowedHosts !== undefined ? { allowedHosts: config.allowedHosts } : {}),
      pauseOnDispose: resolved.pauseOnDispose,
      sessionFailureTtlMs: resolved.sessionFailureTtlMs ?? 10_000,
    }
    this.cwd = this.config.cwd
    this.runtimeClass = this.config.runtimeClass

    // Resolve the gateway: explicit config → env → CLI profiles.yaml (KIP-17).
    const transportCfg = resolveSandboxTransport({
      ...(config.endpoint !== undefined ? { endpoint: config.endpoint } : {}),
      ...(config.certDir !== undefined ? { certDir: config.certDir } : {}),
      ...(config.profile !== undefined ? { profile: config.profile } : {}),
    })
    this.endpoint = transportCfg?.endpoint
    this.endpointSource = transportCfg?.source
    this.endpointProfile = transportCfg?.profile
    this.certDir = transportCfg?.certDir

    if (transportCfg !== undefined) {
      // Persistent gRPC connection: fs/exec ops become RPCs, no per-op CLI spawn.
      this.grpcClient = new GrpcK8eClient({
        endpoint: transportCfg.endpoint,
        ...(transportCfg.certDir !== undefined ? { certDir: transportCfg.certDir } : {}),
      })
      this.transport = this.grpcClient
    } else {
      this.grpcClient = undefined
      this.transport = new CliK8eClient({
        ...(process.env.K8E_SANDBOX_CLI_BIN !== undefined ? { bin: process.env.K8E_SANDBOX_CLI_BIN } : {}),
        ...(config.profile !== undefined ? { profile: config.profile } : {}),
        ...(config.endpoint !== undefined ? { endpoint: config.endpoint } : {}),
      })
    }

    ctx.effect(() => async () => {
      this.disposed = true
      if (this.sessionId === undefined) return
      try {
        await this.transport.destroySession(this.sessionId)
      } catch (_destroyFailure) {
        // Best-effort: the session pod is also reclaimed by the warm-pool GC.
      }
      this.transport.close?.()
    }, 'k8e sandbox teardown')
  }

  /**
   * Return the live session id, creating the sandbox session on first use.
   * Concurrent callers share one in-flight creation; a failed creation is
   * negatively cached for `sessionFailureTtlMs` so a dead gateway fails fast
   * on subsequent probes instead of re-dialing create every time.
   * @throws when the service is disposing or the gateway is unreachable.
   */
  async getSession(): Promise<string> {
    if (this.disposed) throw new Error('k8e sandbox service is disposing')
    if (this.sessionId !== undefined) return this.sessionId
    const now = Date.now()
    if (this.sessionUnavailableUntil > now) {
      const retryIn = Math.ceil((this.sessionUnavailableUntil - now) / 1000)
      throw new Error(`k8e sandbox: session unavailable (gateway unreachable), retry in ${retryIn}s`)
    }
    if (this.sessionInFlight !== undefined) return this.sessionInFlight
    this.sessionInFlight = this.createSessionOnce().finally(() => {
      this.sessionInFlight = undefined
    })
    return this.sessionInFlight
  }

  private async createSessionOnce(): Promise<string> {
    try {
      const created = await this.transport.createSession({
        runtimeClass: this.config.runtimeClass,
        ...(this.config.tenant !== undefined ? { tenant: this.config.tenant } : {}),
        ...(this.config.allowedHosts !== undefined ? { allowedHosts: this.config.allowedHosts } : {}),
      })
      if (this.disposed) {
        await this.transport.destroySession(created.sessionId).catch(() => undefined)
        throw new Error('k8e sandbox service is disposing')
      }
      this.sessionId = created.sessionId
      return this.sessionId
    } catch (err) {
      this.sessionUnavailableUntil = Date.now() + this.config.sessionFailureTtlMs
      throw err
    }
  }

  /** Return the shared transport client (persistent gRPC when an endpoint is resolved). */
  getClient(): SandboxTransport {
    return this.transport
  }

  /**
   * Return the direct gRPC client for the terminal primitive. Requires a
   * resolved endpoint (explicit, env, or profile).
   * @throws when no endpoint is configured.
   */
  getGrpcClient(): GrpcK8eClient {
    if (this.grpcClient === undefined) {
      throw new Error('k8e sandbox: gRPC terminal requires an endpoint (config, K8E_SANDBOX_ENDPOINT, or profiles.yaml)')
    }
    return this.grpcClient
  }
}

export default K8eSandboxRuntime
