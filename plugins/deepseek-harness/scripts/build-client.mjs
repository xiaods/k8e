// Builds the k8e-sandbox client half (KIP-20 M2) into the dsh client-bundle
// wire format, mirroring the shipped tsdown client preset but with esbuild:
//
//   lib/client.js            core bundle  — window.__ModuleLoader__.load({id, factory})
//   lib/client-terminal.js   lazy chunk   — globalThis.__dshChunks__["terminal"] = (require) => {...}
//
// Platform modules (react, react-dom/client, react/jsx-runtime) stay external
// and resolve from the shell module table; xterm + @xterm/addon-fit are inlined
// INTO the terminal chunk so they are only fetched on first terminal open.
//
// Usage: node scripts/build-client.mjs
import { mkdir, readFile, writeFile } from 'node:fs/promises'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { build } from 'esbuild'

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..')
const PKG = join(ROOT, 'packages', 'dsh-k8e-sandbox-client-ui')
const CLIENT_DIR = join(PKG, 'src', 'client')
const OUT_DIR = join(PKG, 'lib')

const CLIENT_ID = '@k8e-sandbox/dsh-k8e-sandbox-client-ui'

const PLATFORM_EXTERNALS = ['react', 'react/jsx-runtime', 'react-dom/client']

/**
 * Wrap esbuild's CJS output (which expects free `require`/`exports`/`module`
 * bindings) in the client-modules closure handoff. The factory returns
 * `module.exports`, which the loader memoizes as the bundle's exports.
 */
function handoff(body, open, close) {
  return [
    open,
    'var module = { exports: {} };',
    'var exports = module.exports;',
    'Object.defineProperty(exports, Symbol.toStringTag, { value: "Module" });',
    body.trim(),
    'return module.exports;',
    close,
    '',
  ].join('\n')
}

async function bundleCore() {
  const result = await build({
    entryPoints: [join(CLIENT_DIR, 'index.tsx')],
    bundle: true,
    write: false,
    platform: 'browser',
    format: 'cjs',
    target: 'es2020',
    external: PLATFORM_EXTERNALS,
    define: { 'process.env.NODE_ENV': '"production"' },
  })
  const body = result.outputFiles[0].text
  const out = handoff(
    body,
    `window.__ModuleLoader__.load({\n\tid: ${JSON.stringify(CLIENT_ID)},\n\tfactory: (require) => {`,
    '\t}\n});',
  )
  await writeFile(join(OUT_DIR, 'client.js'), out)
  return result.outputFiles[0].contents.length
}

async function bundleTerminalChunk() {
  const result = await build({
    entryPoints: [join(CLIENT_DIR, 'terminal.tsx')],
    bundle: true,
    write: false,
    platform: 'browser',
    format: 'cjs',
    target: 'es2020',
    minify: true,
    external: PLATFORM_EXTERNALS,
    define: { 'process.env.NODE_ENV': '"production"' },
  })
  const body = result.outputFiles[0].text
  const out = handoff(
    body,
    'globalThis.__dshChunks__["terminal"] = (require) => {',
    '};',
  )
  await writeFile(join(OUT_DIR, 'client-terminal.js'), out)
  return result.outputFiles[0].contents.length
}

await mkdir(OUT_DIR, { recursive: true })
// Host-side loader entry: the browser half lives in `./client`; the host half
// has no behavior, so this is a no-op apply matching dsh's client-package
// convention (importable via the `"."` export).
await writeFile(join(OUT_DIR, 'index.js'), [
  '/** Host loader entry — no host-side behavior; the browser half lives in `./client`. */',
  'export function apply() {}',
  '',
].join('\n'))
const coreBytes = await bundleCore()
const chunkBytes = await bundleTerminalChunk()

const [core, chunk] = await Promise.all([
  readFile(join(OUT_DIR, 'client.js'), 'utf8'),
  readFile(join(OUT_DIR, 'client-terminal.js'), 'utf8'),
])
console.log(`client core   lib/client.js          ${core.length} bytes (entry ${coreBytes} bytes)`)
console.log(`terminal chunk lib/client-terminal.js ${chunk.length} bytes (entry ${chunkBytes} bytes)`)
console.log(`bundle external: ${core.includes('__ModuleLoader__') && chunk.includes('__dshChunks__') ? 'ok' : 'MISSING HANDOFF'}`)
