// Fake-ctx runtime test for the filesystem provider: mount K8eFileSystem on a
// fake ctx with a fake owner + fake CliK8eClient (an in-memory file map), then
// assert resolve/read/write/edit/list map correctly.
import assert from 'node:assert/strict'
import { K8eFileSystem } from '@k8e/dsh-k8e-sandbox-fs'

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
      return [...files.keys()].map((path) => ({ path, modified: 0 }))
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
  const client = makeCliClient(files)
  const owner = { getClient: () => client, getSession: async () => 's1', cwd: '/workspace' }
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

  // listDir returns only direct-child files (k8e ListFiles lists files, not dirs)
  files.set('/workspace/sub/a.txt', 'a')
  files.set('/workspace/root.txt', 'r')
  const entries = await fs.listDir(await fs.resolve('.'))
  const names = entries.map((e) => e.name).sort()
  assert.deepEqual(names, ['hello.txt', 'new.txt', 'root.txt'])
  const txt = entries.find((e) => e.name === 'root.txt')
  assert.equal(txt.type, 'file')
  assert.equal(txt.size, 1, 'size carried from the stat probe')
}

console.log('✔ fs fake-ctx test passed (path primitives, read/write/edit/list)')
