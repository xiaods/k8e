/**
 * k8e-sandbox model-surface tools for DeepSeek Harness (KIP-20 Phase 2):
 * sandbox-only lifecycle verbs exposed on `ctx.tools` — session
 * create/status/destroy, foreground exec, background exec + poll. The
 * fs/subprocess seams cover file and process primitives; these tools expose
 * the k8e lifecycle verbs those seams cannot express (the "third corner" of
 * the seam model).
 * @module @k8e-sandbox/dsh-k8e-sandbox-tool
 */

import type { Context } from '@deepseek-ai/cordis'
import { Service } from '@deepseek-ai/cordis'
import type { ContentBlock } from '@deepseek-ai/dsh-llm'
import { defineTool } from '@deepseek-ai/dsh-tools'
import type { ParameterSchemaSpec } from '@deepseek-ai/dsh-tools'
import type { K8eSandboxRuntime } from '@k8e-sandbox/dsh-k8e-sandbox'

/** Strict object output schema (lossless canonical JSON). */
function resultSchema<const S extends ParameterSchemaSpec>(properties: S) {
  return { type: 'object', additionalProperties: false, properties } as const
}

/** Output fields shared by foreground exec and background poll results. */
const execResultProps = {
  stdout: { type: 'string' },
  stderr: { type: 'string' },
  exitCode: { type: 'number' },
  durationMs: { type: 'number' },
  truncated: { type: 'boolean' },
} as const

/** Pure Native rendering of one validated canonical value. */
function renderValue(_args: unknown, value: unknown): ContentBlock[] {
  return [{ type: 'text', text: JSON.stringify(value) }]
}

/**
 * k8e-sandbox tools provider registered as `ctx.k8eSandboxTools`. It shares
 * the session owned by `ctx.k8eSandbox` (KIP-20): the owner lazily creates
 * the session on first use, and every tool here operates on it.
 */
export class K8eSandboxTools extends Service {
  static readonly inject = ['k8eSandbox', 'tools']

  constructor(ctx: Context, private readonly owner: K8eSandboxRuntime) {
    super(ctx, 'k8eSandboxTools')
    const runtime = this.owner

    ctx.tools.register(defineTool({
      name: 'k8e_sandbox_session_status',
      description: 'Show the current k8e-sandbox session: creates one on first use (shared with fs/subprocess) and reports availability, session id, tenant and pod reachability.',
      parameters: {},
      output: {
        schema: resultSchema({
          available: { type: 'boolean' },
          sessionId: { type: 'string' },
          tenantId: { type: 'string' },
          error: { type: 'string' },
        }),
        render: renderValue,
      },
      async execute() {
        const client = runtime.getClient()
        return client.status()
      },
    }))

    ctx.tools.register(defineTool({
      name: 'k8e_sandbox_session_destroy',
      description: 'Destroy the current k8e-sandbox session (releases the pod; PVC/workspace semantics per session config). Idempotent.',
      parameters: {},
      output: { schema: resultSchema({ destroyed: { type: 'boolean' } }), render: renderValue },
      async execute() {
        const client = runtime.getClient()
        const st = await client.status()
        if (st.sessionId && st.sessionId !== '') {
          await client.destroySession(st.sessionId)
        }
        return { destroyed: true }
      },
    }))

    ctx.tools.register(defineTool({
      name: 'k8e_sandbox_exec',
      description: 'Run a command in the k8e sandbox session and return its output, exit code and duration. Use for quick foreground checks; use k8e_sandbox_run_background for long tasks.',
      parameters: {
        code: { type: 'string', required: true, description: 'Command or code to run' },
        lang: { type: 'string', description: 'Language hint: bash (default), python, node, ts' },
        timeout: { type: 'number', description: 'Timeout in seconds' },
      },
      output: {
        schema: resultSchema({
          ...execResultProps,
        }),
        render: renderValue,
      },
      async execute(args) {
        const client = runtime.getClient()
        return client.run(args.code, {
          ...(args.lang !== undefined ? { lang: args.lang } : {}),
          ...(args.timeout !== undefined ? { timeout: args.timeout } : {}),
          sessionId: await runtime.getSession(),
        })
      },
    }))

    ctx.tools.register(defineTool({
      name: 'k8e_sandbox_run_background',
      description: 'Submit a command to run asynchronously in the k8e sandbox and return its run id immediately; poll with k8e_sandbox_poll. Use for long-running or streaming workloads.',
      parameters: {
        code: { type: 'string', required: true, description: 'Command or code to run in the background' },
        lang: { type: 'string', description: 'Language hint: bash (default), python, node, ts' },
      },
      output: {
        schema: resultSchema({
          runId: { type: 'string' },
          sessionId: { type: 'string' },
          status: { type: 'string' },
        }),
        render: renderValue,
      },
      async execute(args) {
        const client = runtime.getClient()
        const sessionId = await runtime.getSession()
        const res = await client.runBackground(args.code, {
          ...(args.lang !== undefined ? { lang: args.lang } : {}),
          sessionId,
        })
        return { runId: res.runId, sessionId, status: 'submitted' }
      },
    }))

    ctx.tools.register(defineTool({
      name: 'k8e_sandbox_poll',
      description: 'Poll a background run (from k8e_sandbox_run_background) and return its status, output and exit code once it finishes.',
      parameters: {
        runId: { type: 'string', required: true, description: 'Run id returned by k8e_sandbox_run_background' },
      },
      output: {
        schema: resultSchema({
          runId: { type: 'string' },
          status: { type: 'string' },
          ...execResultProps,
        }),
        render: renderValue,
      },
      async execute(args) {
        const client = runtime.getClient()
        return client.poll(args.runId)
      },
    }))
  }
}

export default K8eSandboxTools
