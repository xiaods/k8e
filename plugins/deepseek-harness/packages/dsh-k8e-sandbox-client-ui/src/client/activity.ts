/**
 * Core-bundle activity watcher (KIP-20 M3): subscribes to the sandbox activity
 * SSE and, when `autoOpenTerminal` is on, lazily loads the terminal chunk and
 * opens the panel on the first sandbox execution. Keeps the xterm dependency
 * out of the core bundle — only the SSE (a DOM EventSource) runs at startup.
 */

import { loadPrefs } from './prefs.ts'
import { loadChunk } from './chunk-loader.ts'

export function watchSandboxActivity(): () => void {
  // The SSE stays open regardless of the current preference so a user who
  // toggles autoOpenTerminal in settings mid-session is honoured without a
  // reload; the pref is re-read on every activity rather than captured here.
  const es = new EventSource('/k8e-sandbox/activity')

  const onExec = (event: Event): void => {
    let phase: unknown
    try {
      phase = (JSON.parse((event as MessageEvent).data as string) as { phase?: unknown }).phase
    } catch {
      return
    }
    if (phase !== 'start') return
    if (!loadPrefs().autoOpenTerminal) return
    void loadChunk('terminal')
      .then((chunk) => { (chunk as { openTerminalPanel: () => void }).openTerminalPanel() })
      .catch(() => undefined)
  }

  es.addEventListener('exec', onExec)

  return () => {
    es.removeEventListener('exec', onExec)
    es.close()
  }
}
