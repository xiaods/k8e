/**
 * k8e-sandbox Service Provider for the subprocess capability seam. Phase 1
 * maps `spawn` to a single-shot `k8e-sandbox-cli run`; streaming stdio and
 * `spawnTerminal` arrive in Phase 2 (direct gRPC + KIP-19 PTY).
 * @module @k8e/dsh-k8e-sandbox-subprocess
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
  SubprocessTerminalSpawnSpec,
} from '@deepseek-ai/dsh-subprocess'

/** Quote one opaque argument for the CLI's `/bin/sh -c` layer. */
function shellQuote(value: string): string {
  return `'${value.replaceAll('\'', `'"'"'`)}'`
}

/** k8e-sandbox command manager registered as `ctx.subprocess`. */
export class K8eSubprocessRuntime extends SubprocessRuntime {
  static inject = ['k8eSandbox']

  override async resolveExecutable(
    command: string,
    env?: Readonly<Record<string, string>>,
    signal?: AbortSignal,
  ): Promise<string> {
    if (command.length === 0) throw new Error('k8e-sandbox subprocess: executable name must be non-empty')
    signal?.throwIfAborted()
    const client = this.ctx.k8eSandbox.getClient()
    const sid = await this.ctx.k8eSandbox.getSession()
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
    // Phase 1: argv is re-quoted into a single shell command; the CLI executes
    // it and returns only after completion, so the handle is already settled.
    const command = spec.argv.map(shellQuote).join(' ')
    const client = this.ctx.k8eSandbox.getClient()
    const stdout = new PassThrough()
    const stderr = new PassThrough()
    let settled = false

    const done: Promise<SubprocessOutcome> = (async () => {
      try {
        const sid = await this.ctx.k8eSandbox.getSession()
        const result = await client.run(command, { sessionId: sid, timeout: Math.ceil(spec.graceMs / 1000) })
        stdout.end(result.stdout)
        stderr.end(result.stderr)
        return { exitCode: result.exitCode, signal: null }
      } catch (cause) {
        const error = cause instanceof Error ? cause : new Error(String(cause))
        stdout.destroy(error)
        stderr.destroy(error)
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
    // Phase 2 (depends on KIP-19 sandbox PTY primitive).
    throw new Error('k8e-sandbox subprocess: spawnTerminal is not implemented in Phase 1')
  }
}

export default K8eSubprocessRuntime
