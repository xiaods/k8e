#!/usr/bin/env node
/**
 * Release script for the @k8e-sandbox/* dsh plugin packages.
 *
 * Publishes the seven workspace packages to npmjs.com in dependency
 * topological order (a package is published only after every package it
 * depends on). Uses `npm publish` (pnpm 11.x ignores `--otp` and fails with
 * ERR_PNPM_OTP_NON_INTERACTIVE under 2FA); npm rewrites the in-workspace
 * `workspace:*` ranges to the actual published versions, so consumers
 * install real published packages.
 *
 * Usage:
 *   node scripts/release.mjs [--dry-run] [--version <v>]
 *
 *   --dry-run   Verify the publish payload without contacting the registry
 *               (npm publish --dry-run).
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
import { delimiter, dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { tmpdir } from 'node:os'

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const dryRun = process.argv.includes('--dry-run')
const otpArg = (() => {
  const i = process.argv.indexOf('--otp')
  return i >= 0 && process.argv[i + 1] ? process.argv[i + 1] : null
})()
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

/**
 * Rewrite in-workspace `workspace:*` ranges to the actual published versions
 * so the registry payload is installable (npm publish does NOT rewrite them,
 * unlike pnpm). Returns a restore callback.
 */
function rewriteWorkspaceDeps(pkg) {
  const path = join(root, 'packages', pkg.name, 'package.json')
  const original = readFileSync(path, 'utf8')
  let changed = false
  for (const section of ['dependencies', 'devDependencies', 'peerDependencies']) {
    const deps = pkg.data[section]
    if (!deps) continue
    for (const [dep, range] of Object.entries(deps)) {
      if (dep.startsWith('@k8e-sandbox/') && (range === 'workspace:*' || range === 'workspace:^')) {
        const target = packages.find((q) => q.data.name === dep)
        if (!target) throw new Error(`workspace dep ${dep} of ${pkg.data.name} not found`)
        deps[dep] = target.data.version
        changed = true
      }
    }
  }
  if (changed) {
    writeFileSync(path, JSON.stringify(pkg.data, null, 2) + '\n')
  }
  return () => writeFileSync(path, original)
}

function run(cmd, args, opts = {}) {
  console.log(`$ ${cmd} ${args.join(' ')}`)
  return execFileSync(cmd, args, { stdio: 'inherit', env: SAFE_ENV, ...opts })
}

/**
 * Resolve a binary to an absolute path from the ambient PATH, then rebuild a
 * SAFE_PATH that only contains the binary's own directory plus fixed system
 * directories. SonarCloud flags passing an untrusted PATH to exec* (S5310);
 * using absolute binaries with a locked-down PATH keeps the release script
 * from executing anything other than the installed npm/pnpm.
 */
function resolveBin(name) {
  for (const dir of (process.env.PATH ?? '').split(delimiter)) {
    const cand = join(dir, name)
    if (existsSync(cand)) return cand
  }
  throw new Error(`${name} not found on PATH`)
}

const NPM = resolveBin('npm')
const PUBLISH_BIN = NPM // npm publish: pnpm 11.x --otp is ignored under 2FA (ERR_PNPM_OTP_NON_INTERACTIVE)
const BIN_DIR = dirname(NPM)
const SAFE_PATH = [BIN_DIR, '/usr/local/bin', '/usr/bin', '/bin']
  .filter((d) => existsSync(d))
  .join(delimiter)
const SAFE_ENV = { ...process.env, PATH: SAFE_PATH }

/**
 * True when the exact version already exists on the registry (any dist-tag).
 * Checking the precise version — not `@latest` — makes partial-release retries
 * skip immutable prereleases/superseded versions too (Greptile).
 */
function registryHasVersion(pkgName, version) {
  try {
    const out = execFileSync(NPM, ['view', `${pkgName}@${version}`, 'version'], { encoding: 'utf8', env: SAFE_ENV }).trim()
    return out.split('\n').pop()?.trim() === version
  } catch {
    return false // E404 — not published yet
  }
}

const packages = loadPackages()
const order = topoSort(packages)

// Credentials are established BEFORE any version mutation, and the token
// userconfig (when used) lives for the whole publish loop — deleting it after
// `npm whoami` would leave `pnpm publish` unauthenticated (Greptile).
let cleanupUserconfig = null
if (!dryRun) {
  const token = process.env.NPM_TOKEN
  const userNpmrc = join(process.env.HOME ?? '', '.npmrc')
  if (token) {
    // Minimal temp userconfig so the token is never written into the repo.
    const tmp = mkdtempSync(join(tmpdir(), 'k8e-release-'))
    const rc = join(tmp, '.npmrc')
    writeFileSync(rc, `//registry.npmjs.org/:_authToken=${token}\n`)
    SAFE_ENV.NPM_CONFIG_USERCONFIG = rc
    cleanupUserconfig = () => rmSync(tmp, { recursive: true, force: true })
  } else if (!existsSync(userNpmrc)) {
    console.error('\nNot logged in to npm. Run `npm login` or set NPM_TOKEN first.')
    process.exit(1)
  }
  try {
    const who = execFileSync(NPM, ['whoami'], { encoding: 'utf8', env: SAFE_ENV }).trim()
    console.log(`\nAuthenticated as ${who} on the npm registry.`)
  } catch {
    cleanupUserconfig?.()
    console.error('\nNot logged in to npm. Run `npm login` or set NPM_TOKEN first.')
    process.exit(1)
  }
}

// Version bump happens only for a real release (never on --dry-run), after
// auth succeeded, and is rolled back if publishing fails (Greptile). The
// whole post-auth flow — bump AND publish — sits inside one try/finally so a
// throw anywhere (including version validation) still removes the temporary
// token userconfig (Greptile security review).
const originalVersions = new Map()
const published = []
try {
  if (!dryRun && versionArg) {
    console.log(`\nBumping all packages to ${versionArg}:\n`)
    for (const { name, data } of packages) {
      originalVersions.set(data.name, data.version)
    }
    bumpVersion(packages, versionArg)
  }

  console.log(`\nPublishing ${order.length} packages in topological order${dryRun ? ' (dry run)' : ''}:\n`)
  for (const { name, data } of order) {
    // Resume support: a package whose version is already on the registry is
    // skipped instead of colliding with the immutable prior publication
    // (partial-failure retry after a rollback).
    if (!dryRun && registryHasVersion(data.name, data.version)) {
      console.log(`── ${data.name}@${data.version} already published, skipping`)
      published.push(data.name)
      continue
    }
    // npm publish does not rewrite workspace:* ranges; swap them for the
    // real published versions just for this publish, then restore in a
    // finally so a failed npm command never leaves rewritten ranges behind.
    const restoreDeps = rewriteWorkspaceDeps({ name, data })
    try {
      console.log(`── ${data.name}@${data.version}`)
      const args = dryRun ? ['pack', '--dry-run'] : ['publish', '--no-git-checks']
      if (!dryRun && otpArg) args.push('--otp', otpArg)
      run(PUBLISH_BIN, args, { cwd: join(root, 'packages', name) })
    } finally {
      restoreDeps()
    }
    published.push(data.name)
  }
} catch (err) {
  // Roll back the local version bump so a failed release leaves no mutation.
  for (const { name, data } of packages) {
    if (originalVersions.has(data.name)) {
      data.version = originalVersions.get(data.name)
      writeFileSync(join(root, 'packages', name, 'package.json'), JSON.stringify(data, null, 2) + '\n')
    }
  }
  const done = published.length ? `\nAlready published (immutable on npm, skipped on retry): ${published.join(', ')}` : ''
  console.error(`\nRelease failed at ${order[published.length]?.data.name ?? '?'} — package.json versions restored.${done}`)
  throw err
} finally {
  cleanupUserconfig?.()
}

console.log(`\n${dryRun ? 'Dry run' : 'Release'} complete: ${order.map((p) => p.data.name).join(', ')}`)
