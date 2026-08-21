/**
 * User-level sandbox prefs (docs/ui-design.md §9). Two tiers:
 *
 * - **Host-backed (primary)**: `/k8e-sandbox/api/prefs` (read) and
 *   `/k8e-sandbox/api/prefs/set` (write) persist L1 prefs host-side
 *   (`~/.k8e/sandbox/ui-prefs.json`) so they survive across browsers.
 *   `runtimeClass` only lives here — it feeds session creation, not the
 *   terminal renderer.
 * - **localStorage (fallback cache)**: the settings page degrades to it when
 *   the host is unreachable, and the terminal panel reads it synchronously for
 *   rows/cols geometry (the lazy chunk stays sync).
 */

export interface TerminalPrefs {
  /** L1 runtime override for NEW sessions; undefined = follow deployment config. */
  runtimeClass: string | undefined
  rows: number
  cols: number
  autoOpenTerminal: boolean
}

const KEY = 'k8e-sandbox:terminal-prefs'

export const DEFAULTS: TerminalPrefs = { runtimeClass: undefined, rows: 24, cols: 80, autoOpenTerminal: true }

const RUNTIMES = ['gvisor', 'kata', 'firecracker'] as const

function clampInt(value: unknown, min: number, max: number, fallback: number): number {
  const n = typeof value === 'number' ? value : Number(value)
  if (!Number.isFinite(n)) return fallback
  return Math.min(max, Math.max(min, Math.round(n)))
}

/** Synchronous local cache read (terminal geometry + offline fallback). */
export function loadPrefs(): TerminalPrefs {
  try {
    const raw = localStorage.getItem(KEY)
    if (raw === null) return { ...DEFAULTS }
    const parsed = JSON.parse(raw) as Partial<TerminalPrefs>
    return {
      runtimeClass: typeof parsed.runtimeClass === 'string' && (RUNTIMES as readonly string[]).includes(parsed.runtimeClass)
        ? parsed.runtimeClass
        : undefined,
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

/** Synchronous local cache write. */
export function savePrefs(prefs: TerminalPrefs): void {
  try {
    localStorage.setItem(KEY, JSON.stringify(prefs))
  } catch {
    // Storage disabled (private mode, quota): the panel still works with defaults.
  }
}

/** Host prefs shape mirroring the /k8e-sandbox/api/prefs response. */
interface HostPrefsResponse {
  ok: boolean
  prefs?: TerminalPrefs
  effective?: {
    runtimeClass?: string
    runtimeClassSource?: 'prefs' | 'config'
    endpoint?: string
    endpointSource?: string
  }
  error?: { code?: string; message?: string }
}

async function fetchJson(url: string, body?: unknown): Promise<unknown> {
  const init: RequestInit = { method: 'POST' }
  if (body !== undefined) {
    init.headers = { 'content-type': 'application/json' }
    init.body = JSON.stringify(body)
  }
  const res = await fetch(url, init)
  const text = await res.text()
  let parsed: unknown
  try {
    parsed = JSON.parse(text)
  } catch {
    throw new Error(`k8e sandbox: bad response from ${url}: HTTP ${res.status}`)
  }
  return parsed
}

/**
 * Load prefs from the host (authoritative). On host failure, fall back to the
 * local cache so the UI still works offline — a stale local value is better
 * than a blank form.
 */
export async function loadHostPrefs(): Promise<TerminalPrefs> {
  try {
    const body = (await fetchJson('/k8e-sandbox/api/prefs')) as HostPrefsResponse
    if (body.ok !== true || body.prefs === undefined) throw new Error('prefs read rejected')
    const prefs: TerminalPrefs = {
      runtimeClass: body.prefs.runtimeClass,
      rows: clampInt(body.prefs.rows, 1, 200, DEFAULTS.rows),
      cols: clampInt(body.prefs.cols, 1, 400, DEFAULTS.cols),
      autoOpenTerminal: body.prefs.autoOpenTerminal,
    }
    savePrefs(prefs)
    return prefs
  } catch {
    return loadPrefs()
  }
}

/**
 * Save prefs host-side (primary). Always mirrors to the local cache first so
 * the terminal geometry updates immediately; rethrows when the host write
 * fails so the UI can surface a hint that the change is local-only.
 */
export async function saveHostPrefs(prefs: TerminalPrefs): Promise<void> {
  savePrefs(prefs)
  try {
    const body = (await fetchJson('/k8e-sandbox/api/prefs/set', { prefs })) as HostPrefsResponse
    if (body.ok !== true) throw new Error(body.error?.message ?? 'prefs write rejected')
  } catch (error) {
    throw error instanceof Error ? error : new Error(String(error))
  }
}
