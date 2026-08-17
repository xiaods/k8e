#!/usr/bin/env node
/**
 * Release script for the @k8e-sandbox/* dsh plugin packages.
 *
 * Publishes the seven workspace packages to npmjs.com in dependency
 * topological order (a package is published only after every package it
 * depends on). `pnpm publish` rewrites `workspace:*` dependency ranges to the
 * actual published versions, so consumers install real published packages.
 *
 * Usage:
 *   node scripts/release.mjs [--dry-run] [--version <v>]
 *
 *   --dry-run   Build each package and verify the publish payload without
 *               contacting the registry (pnpm publish --dry-run).
 *   --version   Bump every package to <v> (e.g. 0.2.0) before publishing;
 *               without it the current package.json versions are used.
 *
 * Environment:
 *   NPM_TOKEN / npm login — the publishing identity must have access to the
 *   `@k8e-sandbox` npm org. If NPM_TOKEN is set it is used for auth;
 *   otherwise the ambient `npm whoami` session is required. Every package
 *   carries `publishConfig.access = "public"`, so scoped packages publish
 *   as public without extra flags.
 */
import { execFileSync } from 'node:child_process'
import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { tmpdir } from 'node:os'

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const dryRun = process.argv.includes('--dry-run')
const versionArg = (() => {
  const i = process.argv.indexOf('--version')
  return i >= 0 && process.argv[i + 1] ? process.argv[i + 1] : null
})()

/** All workspace packages with their @k8e-sandbox/* dependencies. */
function loadPackages() {
  const dirs = []
  for (const name of ['dsh-k8e-sandbox-client', 'dsh-k8e-sandbox-client-ui', 'dsh-k8e-sandbox', 'dsh-k8e-sandbox-fs', 'dsh-k8e-sandbox-subprocess', 'dsh-k8e-sandbox-host-ui', 'dsh-k8e-sandbox-bundle']) {
    const p = join(root, 'packages', name, 'package.json')
    if (!existsSync(p)) throw new Error(`missing package.json: ${p}`)
    const data = JSON.parse(readFileSync(p, 'utf8'))
    if (!data.name?.startsWith('@k8e-sandbox/')) {
      throw new Error(`unexpected package name ${data.name} (expected @k8e-sandbox/*)`)
    }
    dirs.push({ name, data })
  }
  return dirs
}

/** Topological order: dependencies first. Throws on cycles. */
function topoSort(packages) {
  const byName = new Map(packages.map((p) => [p.data.name, p]))
  const order = []
  const state = new Map() // 0 = visiting, 1 = done
  const visit = (p) => {
    const mark = state.get(p.data.name)
    if (mark === 1) return
    if (mark === 0) throw new Error(`dependency cycle at ${p.data.name}`)
    state.set(p.data.name, 0)
    const deps = new Set([
      ...Object.keys(p.data.dependencies ?? {}),
      ...Object.keys(p.data.devDependencies ?? {}),
      ...Object.keys(p.data.peerDependencies ?? {}),
    ].filter((k) => k.startsWith('@k8e-sandbox/')))
    for (const dep of deps) {
      const depPkg = byName.get(dep)
      if (!depPkg) throw new Error(`${p.data.name} depends on unknown workspace package ${dep}`)
      visit(depPkg)
    }
    state.set(p.data.name, 1)
    order.push(p)
  }
  for (const p of packages) visit(p)
  return order
}

/** Bump every package version to the requested value (keeps them in lockstep). */
function bumpVersion(packages, version) {
  if (!/^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$/.test(version)) {
    throw new Error(`invalid version "${version}" (expected semver like 0.2.0)`)
  }
  for (const { name, data } of packages) {
    const p = join(root, 'packages', name, 'package.json')
    data.version = version
    writeFileSync(p, JSON.stringify(data, null, 2) + '\n')
    console.log(`  bumped ${data.name} -> ${version}`)
  }
}

function run(cmd, args, opts = {}) {
  console.log(`$ ${cmd} ${args.join(' ')}`)
  return execFileSync(cmd, args, { stdio: 'inherit', ...opts })
}

const packages = loadPackages()
const order = topoSort(packages)

if (versionArg) {
  console.log(`\nBumping all packages to ${versionArg}:\n`)
  bumpVersion(packages, versionArg)
}

if (!dryRun) {
  // Fail fast with a clear message instead of a confusing 401 per package.
  const token = process.env.NPM_TOKEN
  const userNpmrc = join(process.env.HOME ?? '', '.npmrc')
  const env = { ...process.env }
  if (token) {
    // Minimal temp userconfig so the token is never written into the repo.
    const tmp = mkdtempSync(join(tmpdir(), 'k8e-release-'))
    const rc = join(tmp, '.npmrc')
    writeFileSync(rc, `//registry.npmjs.org/:_authToken=${token}\n`)
    env.NPM_CONFIG_USERCONFIG = rc
    try {
      const who = execFileSync('npm', ['whoami'], { encoding: 'utf8', env }).trim()
      console.log(`\nAuthenticated as ${who} on the npm registry (NPM_TOKEN).`)
    } finally {
      rmSync(tmp, { recursive: true, force: true })
    }
  } else if (existsSync(userNpmrc)) {
    const who = execFileSync('npm', ['whoami'], { encoding: 'utf8', env }).trim()
    console.log(`\nAuthenticated as ${who} on the npm registry.`)
  } else {
    console.error('\nNot logged in to npm. Run `npm login` or set NPM_TOKEN first.')
    process.exit(1)
  }
}

console.log(`\nPublishing ${order.length} packages in topological order${dryRun ? ' (dry run)' : ''}:\n`)
for (const { name, data } of order) {
  console.log(`── ${data.name}@${data.version}`)
  const args = ['publish', '--no-git-checks']
  if (dryRun) args.push('--dry-run')
  run('pnpm', args, { cwd: join(root, 'packages', name) })
}

console.log(`\n${dryRun ? 'Dry run' : 'Release'} complete: ${order.map((p) => p.data.name).join(', ')}`)
