/**
 * User-level terminal preferences (rows/cols/auto-open). These are pure
 * browser concerns — persisted to localStorage, read by the settings section
 * and the terminal panel. Deploy-level config (endpoint/runtimeClass/certDir)
 * stays host-side in the `dsh-k8e-sandbox` owner row.
 */

export interface TerminalPrefs {
  rows: number
  cols: number
  autoOpenTerminal: boolean
}

const KEY = 'k8e-sandbox:terminal-prefs'

const DEFAULTS: TerminalPrefs = { rows: 24, cols: 80, autoOpenTerminal: true }

function clampInt(value: unknown, min: number, max: number, fallback: number): number {
  const n = typeof value === 'number' ? value : Number(value)
  if (!Number.isFinite(n)) return fallback
  return Math.min(max, Math.max(min, Math.round(n)))
}

export function loadPrefs(): TerminalPrefs {
  try {
    const raw = localStorage.getItem(KEY)
    if (raw === null) return { ...DEFAULTS }
    const parsed = JSON.parse(raw) as Partial<TerminalPrefs>
    return {
      rows: clampInt(parsed.rows, 1, 200, DEFAULTS.rows),
      cols: clampInt(parsed.cols, 1, 400, DEFAULTS.cols),
      autoOpenTerminal: typeof parsed.autoOpenTerminal === 'boolean'
        ? parsed.autoOpenTerminal
        : DEFAULTS.autoOpenTerminal,
    }
  } catch {
    return { ...DEFAULTS }
  }
}

export function savePrefs(prefs: TerminalPrefs): void {
  try {
    localStorage.setItem(KEY, JSON.stringify(prefs))
  } catch {
    // Storage disabled (private mode, quota): the panel still works with defaults.
  }
}
