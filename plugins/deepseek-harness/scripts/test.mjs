// Test runner: bundles each tests/*.test.mjs entry (which imports the plugin
// source) with esbuild, resolving @k8e/* to src via alias and @deepseek-ai/*
// via the node_modules symlinks (see README), leaving @grpc/* external, then
// imports and runs each bundle. Usage: node scripts/test.mjs [tests/<file>]
import { readdir } from 'node:fs/promises'
import { writeFile } from 'node:fs/promises'
import { join, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'
import { build } from 'esbuild'

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..')
const testsDir = join(ROOT, 'tests')
const files = process.argv[2]
  ? [process.argv[2]]
  : (await readdir(testsDir)).filter((f) => f.endsWith('.test.mjs') && !f.startsWith('.bundle-')).sort()

for (const file of files) {
  const result = await build({
    entryPoints: [join(testsDir, file)],
    bundle: true,
    platform: 'node',
    format: 'esm',
    write: false,
    sourcemap: false,
    external: ['@grpc/grpc-js', '@grpc/proto-loader'],
    alias: {
      '@k8e/dsh-k8e-sandbox': join(ROOT, 'packages/dsh-k8e-sandbox/src/index.ts'),
      '@k8e/dsh-k8e-sandbox-client': join(ROOT, 'packages/dsh-k8e-sandbox-client/src/index.ts'),
      '@k8e/dsh-k8e-sandbox-client/grpc': join(ROOT, 'packages/dsh-k8e-sandbox-client/src/grpc.ts'),
      '@k8e/dsh-k8e-sandbox-fs': join(ROOT, 'packages/dsh-k8e-sandbox-fs/src/index.ts'),
      '@k8e/dsh-k8e-sandbox-subprocess': join(ROOT, 'packages/dsh-k8e-sandbox-subprocess/src/index.ts'),
    },
  })
  const outfile = join(testsDir, `.bundle-${file}`)
  await writeFile(outfile, result.outputFiles[0].text)
  await import(outfile)
}
