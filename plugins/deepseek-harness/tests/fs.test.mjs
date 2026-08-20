// Fake-ctx runtime test for the filesystem provider: mount K8eFileSystem on a
// fake ctx with a fake owner + fake transport (an in-memory file map), then
// assert resolve/read/write/edit/list map correctly, listDir uses entry facts
// (no per-child stat), and host-absolute paths stay on the local disk.
import assert from 'node:assert/strict'
import { mkdtempSync, writeFileSync, readFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { K8eFileSystem } from '@k8e-sandbox/dsh-k8e-sandbox-fs'

function makeCliClient(files) {
  const calls = []
  return {
    calls,
    async read(sid, path) {
      calls.push(['read', sid, path])
      if (!files.has(path)) throw new Error(`no such file: ${path}`)
      return files.get(path)
    },
    async write(sid, path, content) {
      calls.push(['write', sid, path, content])
      files.set(path, content)
    },
    async list(sid) {
      calls.push(['list', sid])
      // Gateway entry facts: type/size arrive with the single ListFiles RPC.
      return [...files.keys()].map((path) => ({
        path,
        modified: 0,
        type: 'file',
        size: files.get(path).length,
      }))
    },
    async run(code, opts) {
      calls.push(['run', code, opts])
      // parse: stat -c '%f|%s|%Y' 'PATH' 2>/dev/null || printf 'missing'
      const m = /stat -c '[^']*' '([^']+)'/.exec(code)
      if (m) {
        const path = m[1]
        if (files.has(path)) {
          return { stdout: `8000|${files.get(path).length}|123`, stderr: '', exitCode: 0 }
        }
        return { stdout: 'missing', stderr: '', exitCode: 0 }
      }
      return { stdout: '', stderr: '', exitCode: 0 }
    },
  }
}

function makeCtx(owner) {
  const ctx = { k8eSandbox: owner, reflect: { provide() { return () => {} } }, effect() { return () => {} } }
  return ctx
}

/** Fresh sandbox owner whose transport is an in-memory file map. */
function makeSandboxOwner(files) {
  const client = makeCliClient(files)
  return { getClient: () => client, getSession: async () => 's1', cwd: '/workspace' }
}

// ---- pure path primitives ------------------------------------------
{
  const files = new Map()
  const owner = { getClient: () => makeCliClient(files), getSession: async () => 's1', cwd: '/workspace' }
  const fs = new K8eFileSystem(makeCtx(owner))

  const target = await fs.resolve('src/a.txt')
  assert.equal(target.displayPath, '/workspace/src/a.txt')
  assert.equal(fs.processPath(target), '/workspace/src/a.txt')
  assert.equal(fs.fileUrl(target), 'file:///workspace/src/a.txt')

  const parent = await fs.resolve('src')
  const child = await fs.resolve('src/a.txt')
  const outside = await fs.resolve('other/b.txt')
  assert.equal(fs.contains(parent, child), true)
  assert.equal(fs.contains(parent, outside), false)
}

// ---- readText / writeText (create + update) / editText / listDir ----
{
  const files = new Map([['/workspace/hello.txt', 'hello world']])
  const owner = makeSandboxOwner(files)
  const client = owner.getClient()
  const fs = new K8eFileSystem(makeCtx(owner))

  // readText
  assert.equal(await fs.readText(await fs.resolve('hello.txt')), 'hello world')

  // writeText create
  const createOutcome = await fs.writeText(await fs.resolve('new.txt'), 'brand new')
  assert.equal(createOutcome.operation, 'create')
  assert.equal(createOutcome.before, null)
  assert.equal(createOutcome.after, 'brand new')
  assert.equal(files.get('/workspace/new.txt'), 'brand new')

  // writeText update
  const updateOutcome = await fs.writeText(await fs.resolve('new.txt'), 'updated')
  assert.equal(updateOutcome.operation, 'update')
  assert.equal(updateOutcome.before, 'brand new')
  assert.equal(updateOutcome.after, 'updated')

  // writeText createIfAbsent rejects an existing file
  await assert.rejects(
    fs.writeText(await fs.resolve('new.txt'), 'again', { kind: 'createIfAbsent' }),
    /overwrite existing/,
  )

  // editText literal replace
  const editOutcome = await fs.editText(await fs.resolve('hello.txt'), {
    oldString: 'world',
    newString: 'there',
    replaceAll: false,
  })
  assert.equal(editOutcome.before, 'hello world')
  assert.equal(editOutcome.after, 'hello there')
  assert.equal(files.get('/workspace/hello.txt'), 'hello there')

  // listDir returns only direct-child files; entry facts (type/size) arrive in
  // the single ListFiles RPC — no per-child `run` stat (KIP-20 perf).
  files.set('/workspace/sub/a.txt', 'a')
  files.set('/workspace/root.txt', 'r')
  const runCallsBeforeList = client.calls.filter(([op]) => op === 'run').length
  const entries = await fs.listDir(await fs.resolve('.'))
  const runCallsAfterList = client.calls.filter(([op]) => op === 'run').length
  assert.equal(runCallsAfterList, runCallsBeforeList, 'listDir must not spawn per-child stat probes')
  const names = entries.map((e) => e.name).sort()
  assert.deepEqual(names, ['hello.txt', 'new.txt', 'root.txt'])
  const txt = entries.find((e) => e.name === 'root.txt')
  assert.equal(txt.type, 'file')
  assert.equal(txt.size, 1, 'size carried from ListFiles entry facts')
}

// ---- host-absolute paths stay on the local disk (preflight local) ----
{
  const hostDir = mkdtempSync(join(tmpdir(), 'k8e-fs-host-'))
  try {
    writeFileSync(join(hostDir, 'AGENTS.md'), '# local instructions\n', 'utf8')
    writeFileSync(join(hostDir, 'nested.txt'), 'n', 'utf8')
    const files = new Map()
    const owner = makeSandboxOwner(files)
    const client = owner.getClient()
    const fs = new K8eFileSystem(makeCtx(owner))

    // stat reads host metadata without touching the sandbox transport
    const info = await fs.stat(await fs.resolve(join(hostDir, 'AGENTS.md')))
    assert.equal(info.type, 'file')
    assert.equal(info.size, 21)

    // readText reads the local file
    assert.equal(await fs.readText(await fs.resolve(join(hostDir, 'AGENTS.md'))), '# local instructions\n')

    // listDir reads the local directory
    const entries = await fs.listDir(await fs.resolve(hostDir))
    assert.deepEqual(entries.map((e) => e.name).sort(), ['AGENTS.md', 'nested.txt'])
    assert.equal(entries.find((e) => e.name === 'AGENTS.md').type, 'file')

    // none of the host ops may touch the sandbox transport at all
    assert.equal(client.calls.length, 0, 'host-absolute paths must not hit the sandbox transport')

    // writeText on a host path writes locally
    const outcome = await fs.writeText(await fs.resolve(join(hostDir, 'new.md')), 'hello')
    assert.equal(outcome.operation, 'create')
    assert.equal(readFileSync(join(hostDir, 'new.md'), 'utf8'), 'hello')
  } finally {
    rmSync(hostDir, { recursive: true, force: true })
  }
}

console.log('✔ fs fake-ctx test passed (path primitives, read/write/edit/list, host-local preflight)')
