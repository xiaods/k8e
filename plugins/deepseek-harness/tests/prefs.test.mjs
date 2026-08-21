// Unit tests for the host-side L1 prefs store (docs/ui-design.md §9): input
// validation, atomic file persistence, and effective-runtime precedence.
import assert from 'node:assert/strict'
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { readFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import {
  DEFAULT_PREFS,
  effectiveRuntime,
  loadPrefsFile,
  RUNTIME_ALLOWLIST,
  savePrefsFile,
  validatePrefs,
} from '../packages/dsh-k8e-sandbox-host-ui/src/prefs-store.ts'

// ---- validatePrefs (untrusted client input) ----------------------------
{
  // full valid object round-trips
  const full = validatePrefs({ runtimeClass: 'kata', rows: 40, cols: 120, autoOpenTerminal: false })
  assert.equal(full.ok, true)
  assert.deepEqual(full.ok ? full.prefs : null, {
    runtimeClass: 'kata', rows: 40, cols: 120, autoOpenTerminal: false,
  })

  // partial input fills defaults (runtimeClass omitted = follow config)
  const partial = validatePrefs({ rows: 30 })
  assert.equal(partial.ok, true)
  assert.deepEqual(partial.ok ? partial.prefs : null, {
    runtimeClass: undefined, rows: 30, cols: 80, autoOpenTerminal: true,
  })

  // empty runtimeClass string means "default", not an error
  const emptyRuntime = validatePrefs({ runtimeClass: '' })
  assert.equal(emptyRuntime.ok, true)
  assert.equal(emptyRuntime.ok ? emptyRuntime.prefs.runtimeClass : 'x', undefined)

  // unknown keys are ignored
  const extra = validatePrefs({ rows: 10, endpoint: 'evil:50051', certDir: '/tmp/x' })
  assert.equal(extra.ok, true)

  // non-object input rejects
  assert.equal(validatePrefs(null).ok, false)
  assert.equal(validatePrefs('gvisor').ok, false)
  assert.equal(validatePrefs([1, 2]).ok, false)

  // out-of-allowlist runtime rejects
  const badRuntime = validatePrefs({ runtimeClass: 'docker' })
  assert.equal(badRuntime.ok, false)
  assert.equal(badRuntime.ok ? '' : badRuntime.error.code, 'validation')

  // out-of-range rows / cols reject
  assert.equal(validatePrefs({ rows: 0 }).ok, false)
  assert.equal(validatePrefs({ rows: 201 }).ok, false)
  assert.equal(validatePrefs({ cols: 401 }).ok, false)
  assert.equal(validatePrefs({ rows: 1.5 }).ok, false)

  // non-boolean autoOpen rejects
  assert.equal(validatePrefs({ autoOpenTerminal: 'yes' }).ok, false)

  // allowlist is exactly the three runtimes the UI offers
  assert.deepEqual(RUNTIME_ALLOWLIST, ['gvisor', 'kata', 'firecracker'])
}

// ---- savePrefsFile / loadPrefsFile (persistence) ------------------------
{
  const dir = mkdtempSync(join(tmpdir(), 'k8e-prefs-'))
  const path = join(dir, 'nested', 'ui-prefs.json')
  try {
    // missing file → defaults
    assert.deepEqual(await loadPrefsFile(path), DEFAULT_PREFS)

    // write + read round-trip
    const saved = { runtimeClass: 'firecracker', rows: 50, cols: 160, autoOpenTerminal: false }
    await savePrefsFile(path, saved)
    assert.deepEqual(await loadPrefsFile(path), saved)

    // parent dirs were created
    const raw = await readFile(path, 'utf8')
    assert.ok(raw.includes('"runtimeClass": "firecracker"'))

    // corrupt file → defaults (never crashes the settings page)
    writeFileSync(path, '{ not json', 'utf8')
    assert.deepEqual(await loadPrefsFile(path), DEFAULT_PREFS)

    // valid JSON but invalid values → defaults
    writeFileSync(path, JSON.stringify({ runtimeClass: 'evil', rows: 999 }), 'utf8')
    assert.deepEqual(await loadPrefsFile(path), DEFAULT_PREFS)

    // atomic write leaves no tmp file behind
    await savePrefsFile(path, { runtimeClass: 'kata', rows: 24, cols: 80, autoOpenTerminal: true })
    await assert.rejects(readFile(path + '.tmp', 'utf8'))
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
}

// ---- effectiveRuntime (precedence: L1 prefs override → config default) ---
{
  assert.deepEqual(effectiveRuntime('kata', 'gvisor'), { runtimeClass: 'kata', source: 'prefs' })
  assert.deepEqual(effectiveRuntime(undefined, 'gvisor'), { runtimeClass: 'gvisor', source: 'config' })
  assert.deepEqual(effectiveRuntime(undefined, 'kata'), { runtimeClass: 'kata', source: 'config' })
}

console.log('✔ prefs-store test passed (validatePrefs, file persistence, effectiveRuntime)')
