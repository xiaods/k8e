/**
 * Wire shapes shared by the core (auto-open watcher) and the terminal chunk
 * (command log) for the sandbox activity SSE. Type-only — esbuild erases these,
 * so the core and chunk stay separate bundles.
 */

/** One sandbox execution activity, streamed by /k8e-sandbox/activity. */
export type K8eExecEvent =
  | { phase: 'start'; id: string; command: string; cwd?: string; at: number }
  | { phase: 'output'; id: string; stream: 'stdout' | 'stderr'; data: string; at: number }
  | { phase: 'exit'; id: string; exitCode: number | null; signal: string | null; at: number }

/** One accumulated command for the log replay (`history` SSE event). */
export interface ExecLogEntry {
  id: string
  command: string
  cwd: string | undefined
  startedAt: number
  stdout: string
  stderr: string
  exitCode: number | null
  signal: string | null
  settled: boolean
}
