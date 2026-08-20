/**
 * k8e-sandbox Service Provider for the subprocess capability seam. Phase 1
 * maps `spawn` to a single-shot `k8e-sandbox-cli run`; streaming stdio and
 * `spawnTerminal` arrive in Phase 2 (direct gRPC + KIP-19 PTY).
 * @module @k8e-sandbox/dsh-k8e-sandbox-subprocess
 */

import { posix } from 'node:path'
import { PassThrough } from 'node:stream'
import { Context } from '@deepseek-ai/cordis'
import { SubprocessRuntime } from '@deepseek-ai/dsh-subprocess'
import type {
  SubprocessHandle,
  SubprocessOutcome,
  SubprocessSpawnSpec,
  SubprocessTerminalHandle,
  SubprocessTerminalSignal,
  SubprocessTerminalSpawnSpec,
} from '@deepseek-ai/dsh-subprocess'
import type { GrpcK8eClient } from '@k8e-sandbox/dsh-k8e-sandbox-client/grpc'
import type { K8eSandboxRuntime } from '@k8e-sandbox/dsh-k8e-sandbox'
// Side-effect import pulls in the cordis Context augmentation (ctx.k8eSandbox).
import '@k8e-sandbox/dsh-k8e-sandbox'

/** Quote one opaque argument for the CLI's `/bin/sh -c` layer. */
function shellQuote(value: string): string {
  return `'${value.replaceAll('\'', `'"'"'`)}'`
}

let execIdCounter = 0
function nextExecId(): string {
  execIdCounter += 1
  return `exec-${process.pid}-${execIdCounter}`
}

/**
 * One sandbox execution activity, broadcast on the `k8e-sandbox/exec` event.
 * The host-ui bridge forwards it to the browser so the terminal panel can show
 * running commands live and auto-open on activity (KIP-20 M3).
 */
export type K8eExecEvent =
  | { phase: 'start'; id: string; command: string; cwd?: string; at: number }
  | { phase: 'output'; id: string; stream: 'stdout' | 'stderr'; data: string; at: number }
  | { phase: 'exit'; id: string; exitCode: number | null; signal: string | null; at: number }

declare module '@deepseek-ai/cordis' {
  interface Events {
    /** A sandbox execution advanced (started / emitted output / exited). @mode emit */
    'k8e-sandbox/exec'(event: K8eExecEvent): void
  }
}

/** k8e-sandbox command manager registered as `ctx.subprocess`. */
export class K8eSubprocessRuntime extends SubprocessRuntime {
  static inject = ['k8eSandbox']

  /**
   * The owning sandbox service with an actionable error when it is missing
   * (stale dsh process after a bundle upgrade, or the bundle not registered in
   * dsh.profile.bundles).
   */
  private owner(): K8eSandboxRuntime {
    const owner = this.ctx.k8eSandbox
    if (owner === undefined) {
      throw new Error('k8e-sandbox: ctx.k8eSandbox is not mounted — the dsh-k8e-sandbox bundle is not loaded; reinstall the bundle and restart dsh (k8e-sandbox-cli doctor --fix diagnoses this)')
    }
    return owner
  }

  override async resolveExecutable(
    command: string,
    env?: Readonly<Record<string, string>>,
    signal?: AbortSignal,
  ): Promise<string> {
    if (command.length === 0) throw new Error('k8e-sandbox subprocess: executable name must be non-empty')
    signal?.throwIfAborted()
    const client = this.owner().getClient()
    const sid = await this.owner().getSession()
    if (posix.isAbsolute(command)) {
      const result = await client.run(`test -f ${shellQuote(command)} -a -x ${shellQuote(command)}`, { sessionId: sid, timeout: 10 })
      if (result.exitCode !== 0) throw new Error(`k8e-sandbox subprocess: ${command} is not an executable file`)
      return command
    }
    if (command.includes('/')) {
      throw new Error(`k8e-sandbox subprocess: command ${JSON.stringify(command)} is a relative path; use an absolute path or a bare PATH name`)
    }
    const result = await client.run(`command -v -- ${shellQuote(command)}`, { sessionId: sid, timeout: 10 })
    const executable = result.stdout.trim()
    if (executable.length === 0 || executable.includes('\n') || !posix.isAbsolute(executable)) {
      throw new Error(`k8e-sandbox subprocess: executable ${JSON.stringify(command)} did not resolve to one absolute path`)
    }
    return executable
  }

  override spawn(spec: SubprocessSpawnSpec): SubprocessHandle {
    const command = spec.argv.map(shellQuote).join(' ')
    const id = nextExecId()
    const cwd = this.owner().cwd
    const emit = (event: K8eExecEvent): void => { this.ctx.emit('k8e-sandbox/exec', event) }
    emit({ phase: 'start', id, command, cwd, at: Date.now() })

    const stdout = new PassThrough()
    const stderr = new PassThrough()
    let grpcClient: GrpcK8eClient | undefined
    try {
      grpcClient = this.owner().getGrpcClient()
    } catch {
      grpcClient = undefined
    }
    let settled = false

    const done: Promise<SubprocessOutcome> = (async () => {
      if (grpcClient !== undefined) {
        // Phase 2: streaming exec via gRPC (sandboxd merges stderr into stdout).
        try {
          const sid = await this.owner().getSession()
          const result = grpcClient.execStream(sid, command)
          result.stdout.on('data', (chunk: Buffer) => {
            emit({ phase: 'output', id, stream: 'stdout', data: chunk.toString('utf8'), at: Date.now() })
          })
          // The stream's PassThrough is destroyed with the gRPC error when the
          // gateway rejects the exec (e.g. session gone); without a listener the
          // 'error' event is unhandled and crashes the whole dsh process. Surface
          // it as a normal failed exec instead (done rejects below).
          result.stdout.on('error', (error: Error) => {
            stdout.destroy(error)
            emit({ phase: 'exit', id, exitCode: 1, signal: null, at: Date.now() })
          })
          result.stdout.pipe(stdout)
          const outcome = await result.done
          emit({ phase: 'exit', id, exitCode: outcome.exitCode, signal: outcome.signal, at: Date.now() })
          return outcome
        } catch (cause) {
          const error = cause instanceof Error ? cause : new Error(String(cause))
          stdout.destroy(error)
          emit({ phase: 'exit', id, exitCode: 1, signal: null, at: Date.now() })
          return { exitCode: 1, signal: null }
        } finally {
          settled = true
        }
      }
      // Phase 1 fallback: single-shot CLI run.
      const client = this.owner().getClient()
      try {
        const sid = await this.owner().getSession()
        const result = await client.run(command, { sessionId: sid, timeout: Math.ceil(spec.graceMs / 1000) })
        stdout.end(result.stdout)
        stderr.end(result.stderr)
        if (result.stdout.length > 0) emit({ phase: 'output', id, stream: 'stdout', data: result.stdout, at: Date.now() })
        if (result.stderr.length > 0) emit({ phase: 'output', id, stream: 'stderr', data: result.stderr, at: Date.now() })
        emit({ phase: 'exit', id, exitCode: result.exitCode, signal: null, at: Date.now() })
        return { exitCode: result.exitCode, signal: null }
      } catch (cause) {
        const error = cause instanceof Error ? cause : new Error(String(cause))
        stdout.destroy(error)
        stderr.destroy(error)
        emit({ phase: 'exit', id, exitCode: 1, signal: null, at: Date.now() })
        return { exitCode: 1, signal: null }
      } finally {
        settled = true
      }
    })()

    return {
      pid: -1,
      stdin: undefined,
      stdout,
      stderr,
      collected: {},
      done,
      terminate() {
        if (settled) return
        stdout.destroy()
        stderr.destroy()
      },
      async waitForExit(signal?: AbortSignal): Promise<boolean> {
        if (signal !== undefined) {
          await Promise.race([done, new Promise((_, reject) => signal.addEventListener('abort', () => reject(signal.reason), { once: true }))])
          return !signal.aborted
        }
        await done
        return true
      },
    }
  }

  override async spawnTerminal(spec: SubprocessTerminalSpawnSpec): Promise<SubprocessTerminalHandle> {
    const runtime = this.owner()
    const grpcClient = runtime.getGrpcClient()
    const sessionId = await runtime.getSession()

    const created = await grpcClient.createTerminal({
      sessionId,
      argv: [...spec.argv],
      ...(spec.cwd !== undefined ? { workdir: spec.cwd } : {}),
      ...(spec.env !== undefined ? { env: spec.env } : {}),
      rows: spec.rows,
      cols: spec.cols,
    })

    const output = new PassThrough()
    let exitCode: number | null = null
    let termSignal: NodeJS.Signals | null = null
    let settled = false

    const done: Promise<SubprocessOutcome> = new Promise((resolve, reject) => {
      const stream = grpcClient.terminalStream(created.terminalId)
      stream.on('data', (frame: any) => {
        if (frame.data !== undefined) {
          output.write(frame.data)
        } else if (frame.exit !== undefined) {
          exitCode = frame.exit.exitCode ?? null
          termSignal = frame.exit.signal ? (frame.exit.signal as NodeJS.Signals) : null
          output.end()
          settled = true
          resolve({ exitCode, signal: termSignal })
        }
      })
      stream.on('error', (err: unknown) => {
        output.destroy(err instanceof Error ? err : new Error(String(err)))
        if (!settled) {
          settled = true
          reject(err instanceof Error ? err : new Error(String(err)))
        }
      })
      stream.on('end', () => {
        output.end()
        if (!settled) {
          settled = true
          resolve({ exitCode: exitCode ?? 0, signal: termSignal })
        }
      })
    })

    return {
      pid: created.pid,
      output,
      done,
      async write(data: string): Promise<void> {
        await grpcClient.terminalWrite(created.terminalId, new TextEncoder().encode(data))
      },
      async inspectForeground() {
        const fg = await grpcClient.terminalForeground(created.terminalId)
        if (fg.processGroupId < 0) return undefined
        return { processGroupId: fg.processGroupId, inputWaiting: fg.inputWaiting }
      },
      async signalForeground(signal: SubprocessTerminalSignal): Promise<number> {
        return grpcClient.terminalSignal(created.terminalId, signal)
      },
      async terminate(): Promise<void> {
        await grpcClient.terminalDestroy(created.terminalId, spec.graceMs)
      },
    }
  }
}

export default K8eSubprocessRuntime
