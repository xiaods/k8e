/**
 * Local structural contracts for the services this host-UI plugin consumes.
 * Mirrors DSH-better-sidebar's context-types.ts: the `@deepseek-ai/*` service
 * type packages publish broken dependency chains on npm, so a third-party
 * plugin declares the documented harness faces locally. Drift is contained to
 * this file.
 * @module @k8e-sandbox/dsh-k8e-sandbox-host-ui/context-types
 */

export type { Context } from '@deepseek-ai/cordis'
import type { K8eSandboxRuntime } from '@k8e-sandbox/dsh-k8e-sandbox'

/** The HTTP request face (structural subset of node's IncomingMessage). */
export interface SandboxHttpRequest {
  url?: string
  method?: string
  headers: Record<string, string | string[] | undefined>
  [Symbol.asyncIterator](): AsyncIterator<string | Uint8Array>
}

/** The HTTP response face (structural subset of node's ServerResponse). */
export interface SandboxHttpResponse {
  statusCode: number
  writeHead(status: number, headers?: Record<string, string>): void
  write(chunk: string | Uint8Array): boolean
  end(body?: string | Uint8Array): void
  on(event: 'close', listener: () => void): void
}

/** One webserver route (mirror of the host-webserver WebRoute). */
export interface SandboxWebRoute {
  kind: 'exact' | 'prefix'
  path: string
  handler: (req: SandboxHttpRequest, res: SandboxHttpResponse) => void | Promise<void>
}

/** One HTTP upgrade registration (mirror of WebUpgradeRoute). */
export interface SandboxWebUpgradeRoute {
  path: string
  handler: (req: SandboxHttpRequest, socket: SandboxUpgradeSocket, head: Uint8Array) => void | Promise<void>
}

/** The upgrade socket face (the destroy the fence uses). */
export interface SandboxUpgradeSocket {
  destroy(): void
}

/** The webServer service face this plugin uses. */
export interface SandboxWebServer {
  register(route: SandboxWebRoute): () => void
  registerUpgrade(route: SandboxWebUpgradeRoute): () => void
}

/**
 * One sandbox execution activity emitted by `K8eSubprocessRuntime.spawn` on the
 * `k8e-sandbox/exec` event (structural mirror of the subprocess package's
 * `K8eExecEvent`; restated here so this bridge need not depend on it).
 */
export type K8eExecEvent =
  | { phase: 'start'; id: string; command: string; cwd?: string; at: number }
  | { phase: 'output'; id: string; stream: 'stdout' | 'stderr'; data: string; at: number }
  | { phase: 'exit'; id: string; exitCode: number | null; signal: string | null; at: number }

declare module '@deepseek-ai/cordis' {
  interface Context {
    webServer: SandboxWebServer
    k8eSandbox: K8eSandboxRuntime
    effect(fn: () => void | (() => void), label?: string): void
  }
  interface Events {
    'k8e-sandbox/exec'(event: K8eExecEvent): void
  }
}
