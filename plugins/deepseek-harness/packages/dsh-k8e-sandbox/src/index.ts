/**
 * Shared ownership of one k8e-sandbox session. Capability adapters await the
 * same session handle, so filesystem and process operations inhabit one remote
 * Linux world (mirrors the E2B POC's `ctx.e2b` owner).
 * @module @k8e-sandbox/dsh-k8e-sandbox
 */

import { Context, Service } from '@deepseek-ai/cordis'
import z from '@deepseek-ai/schemastery'
import { CliK8eClient } from '@k8e-sandbox/dsh-k8e-sandbox-client'
import { GrpcK8eClient } from '@k8e-sandbox/dsh-k8e-sandbox-client/grpc'

/** Configuration for the shared k8e-sandbox owner. */
export interface Config {
  /** gRPC gateway endpoint; omission uses the CLI's local auto-discovery. */
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
}

interface ResolvedConfig {
  cwd: string
  runtimeClass: string
  tenant?: string
  allowedHosts?: string[]
  pauseOnDispose: boolean
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
  })

  /** Validated remote working directory shared by provider adapters. */
  readonly cwd: string
  /** gRPC gateway endpoint (undefined = CLI local auto-discovery). */
  readonly endpoint: string | undefined
  /** mTLS material dir for the direct gRPC terminal client. */
  readonly certDir: string | undefined
  /** RuntimeClass for the session pod. */
  readonly runtimeClass: string

  private readonly config: ResolvedConfig
  private readonly client: CliK8eClient
  private readonly grpcClient: GrpcK8eClient | undefined
  private sessionId: string | undefined
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
    }
    this.cwd = this.config.cwd
    this.endpoint = config.endpoint
    this.certDir = config.certDir
    this.runtimeClass = this.config.runtimeClass
    this.client = new CliK8eClient({
      ...(process.env.K8E_SANDBOX_CLI_BIN !== undefined ? { bin: process.env.K8E_SANDBOX_CLI_BIN } : {}),
      ...(config.profile !== undefined ? { profile: config.profile } : {}),
      ...(config.endpoint !== undefined ? { endpoint: config.endpoint } : {}),
    })
    this.grpcClient = config.endpoint !== undefined
      ? new GrpcK8eClient({ endpoint: config.endpoint, ...(config.certDir !== undefined ? { certDir: config.certDir } : {}) })
      : undefined

    ctx.effect(() => async () => {
      this.disposed = true
      if (this.sessionId === undefined) return
      try {
        await this.client.destroySession(this.sessionId)
      } catch (_destroyFailure) {
        // Best-effort: the session pod is also reclaimed by the warm-pool GC.
      }
    }, 'k8e sandbox teardown')
  }

  /**
   * Return the live session id, creating the sandbox session on first use.
   * @throws when the service is disposing.
   */
  async getSession(): Promise<string> {
    if (this.disposed) throw new Error('k8e sandbox service is disposing')
    if (this.sessionId !== undefined) return this.sessionId
    const created = await this.client.createSession({
      runtimeClass: this.config.runtimeClass,
      ...(this.config.tenant !== undefined ? { tenant: this.config.tenant } : {}),
      ...(this.config.allowedHosts !== undefined ? { allowedHosts: this.config.allowedHosts } : {}),
    })
    if (this.disposed) {
      await this.client.destroySession(created.sessionId).catch(() => undefined)
      throw new Error('k8e sandbox service is disposing')
    }
    this.sessionId = created.sessionId
    return this.sessionId
  }

  /** Return the shared transport client. */
  getClient(): CliK8eClient {
    return this.client
  }

  /**
   * Return the direct gRPC client for the terminal primitive. Requires an
   * explicit `endpoint` (the gRPC client does no local auto-discovery).
   * @throws when no endpoint is configured.
   */
  getGrpcClient(): GrpcK8eClient {
    if (this.grpcClient === undefined) {
      throw new Error('k8e sandbox: gRPC terminal requires an explicit `endpoint` config')
    }
    return this.grpcClient
  }
}

export default K8eSandboxRuntime
