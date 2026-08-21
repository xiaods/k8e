/**
 * L1 user-level sandbox prefs (docs/ui-design.md §9): web-editable session
 * parameters persisted host-side so they survive across browsers. Deployment
 * config (endpoint/certDir/tenant) stays read-only in the profile row — this
 * store only ever holds L1 fields.
 *
 * Pure module (no cordis / no plugin wiring) so the validation and file
 * persistence are unit-testable without a dsh context.
 * @module @k8e-sandbox/dsh-k8e-sandbox-host-ui/prefs-store
 */

import { mkdir, readFile, rename, writeFile } from 'node:fs/promises'
import { dirname } from 'node:path'

/** RuntimeClass values the web UI may select (session pod isolation level). */
export const RUNTIME_ALLOWLIST = ['gvisor', 'kata', 'firecracker'] as const

export type RuntimeClass = (typeof RUNTIME_ALLOWLIST)[number]

/** L1 user prefs. `runtimeClass: undefined` = follow deployment config default. */
export interface SavedPrefs {
  runtimeClass: string | undefined
  rows: number
  cols: number
  autoOpenTerminal: boolean
}

export const DEFAULT_PREFS: SavedPrefs = {
  runtimeClass: undefined,
  rows: 24,
  cols: 80,
  autoOpenTerminal: true,
}

export interface PrefsError {
  code: 'validation' | 'io'
  message: string
}

export type PrefsResult = { ok: true; prefs: SavedPrefs } | { ok: false; error: PrefsError }

function clampInt(value: unknown, min: number, max: number, fallback: number): number {
  const n = typeof value === 'number' ? value : Number(value)
  if (!Number.isFinite(n)) return fallback
  return Math.min(max, Math.max(min, Math.round(n)))
}

/**
 * Validate untrusted client input into a complete `SavedPrefs`. Unknown keys
 * are ignored; missing fields fall back to defaults; out-of-range or
 * mistyped values fail validation (the UI must not silently persist garbage).
 */
export function validatePrefs(input: unknown): PrefsResult {
  if (input === null || typeof input !== 'object' || Array.isArray(input)) {
    return { ok: false, error: { code: 'validation', message: 'prefs must be an object' } }
  }
  const raw = input as Record<string, unknown>

  let runtimeClass: string | undefined = undefined
  if (raw.runtimeClass !== undefined && raw.runtimeClass !== null && raw.runtimeClass !== '') {
    if (typeof raw.runtimeClass !== 'string' || !(RUNTIME_ALLOWLIST as readonly string[]).includes(raw.runtimeClass)) {
      return {
        ok: false,
        error: { code: 'validation', message: `runtimeClass must be one of ${RUNTIME_ALLOWLIST.join(', ')}` },
      }
    }
    runtimeClass = raw.runtimeClass
  }

  const rows = clampInt(raw.rows, 1, 200, DEFAULT_PREFS.rows)
  const cols = clampInt(raw.cols, 1, 400, DEFAULT_PREFS.cols)
  if (raw.rows !== undefined && rows !== Number(raw.rows)) {
    return { ok: false, error: { code: 'validation', message: 'rows must be an integer in [1, 200]' } }
  }
  if (raw.cols !== undefined && cols !== Number(raw.cols)) {
    return { ok: false, error: { code: 'validation', message: 'cols must be an integer in [1, 400]' } }
  }

  let autoOpenTerminal = DEFAULT_PREFS.autoOpenTerminal
  if (raw.autoOpenTerminal !== undefined) {
    if (typeof raw.autoOpenTerminal !== 'boolean') {
      return { ok: false, error: { code: 'validation', message: 'autoOpenTerminal must be a boolean' } }
    }
    autoOpenTerminal = raw.autoOpenTerminal
  }

  return { ok: true, prefs: { runtimeClass, rows, cols, autoOpenTerminal } }
}

/** Read the prefs file; missing or corrupt file falls back to defaults. */
export async function loadPrefsFile(path: string): Promise<SavedPrefs> {
  let content: string
  try {
    content = await readFile(path, 'utf8')
  } catch {
    return { ...DEFAULT_PREFS }
  }
  try {
    const parsed = JSON.parse(content) as unknown
    const result = validatePrefs(parsed)
    return result.ok ? result.prefs : { ...DEFAULT_PREFS }
  } catch {
    return { ...DEFAULT_PREFS }
  }
}

/** Atomic write (tmp + rename) so a crash mid-write never corrupts the file. */
export async function savePrefsFile(path: string, prefs: SavedPrefs): Promise<void> {
  const tmp = `${path}.tmp`
  await mkdir(dirname(path), { recursive: true })
  await writeFile(tmp, JSON.stringify(prefs, null, 2) + '\n', 'utf8')
  await rename(tmp, path)
}

/**
 * Effective runtime for NEW sessions: L1 prefs override, else the deployment
 * config default. Encodes the precedence rule the owner (`K8eSandboxRuntime`)
 * applies via `setRuntimeClass`; kept here so it is unit-testable.
 */
export function effectiveRuntime(prefsRuntime: string | undefined, configDefault: string): {
  runtimeClass: string
  source: 'prefs' | 'config'
} {
  return prefsRuntime !== undefined
    ? { runtimeClass: prefsRuntime, source: 'prefs' }
    : { runtimeClass: configDefault, source: 'config' }
}
