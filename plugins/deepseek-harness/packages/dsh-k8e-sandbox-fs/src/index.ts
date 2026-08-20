/**
 * k8e-sandbox Service Provider for the filesystem capability seam.
 *
 * Two path worlds, one provider:
 * - Workspace paths (relative, or under the sandbox root e.g. `/workspace`)
 *   live in the sandbox pod and go through the shared transport (persistent
 *   gRPC when an endpoint is resolved — no per-op CLI spawn).
 * - Host-absolute paths outside the sandbox root (e.g. `/Users/...` used by
 *   agent-instructions / skill discovery preflight) stay on the local disk:
 *   AGENTS.md / skills discovery must not pay a pod round trip before the
 *   first model request (KIP-20 perf follow-up).
 *
 * listDir uses the gateway's ListFiles entry facts (type/size) when present —
 * no per-child stat — and falls back to a single exec probe per child only
 * when the gateway predates entry facts.
 * @module @k8e-sandbox/dsh-k8e-sandbox-fs
 */

import { Buffer } from 'node:buffer'
import { readdir, readFile, stat, writeFile } from 'node:fs/promises'
import { posix } from 'node:path'
import { Context } from '@deepseek-ai/cordis'
import { FileSystem, FsError, FsTargetKey, FsVersion } from '@deepseek-ai/dsh-fs'
import type {
  FsDirEntry,
  FsEditOutcome,
  FsEditRequest,
  FsInfo,
  FsPathInfo,
  FsTarget,
  FsWriteIntent,
  FsWriteOutcome,
} from '@deepseek-ai/dsh-fs'
import type { K8eSandboxRuntime } from '@k8e-sandbox/dsh-k8e-sandbox'

/** Stat facts returned by the single `stat -c` probe. */
interface StatFacts {
  type: FsInfo['type']
  size: number
  version: ReturnType<typeof FsVersion>
}

function hostType(info: { isFile(): boolean; isDirectory(): boolean; isSymbolicLink(): boolean }): FsInfo['type'] {
  return info.isDirectory() ? 'directory' : info.isFile() ? 'file' : 'other'
}

/** Remote filesystem backend sharing the session owned by `ctx.k8eSandbox`. */
export class K8eFileSystem extends FileSystem {
  static inject = ['k8eSandbox']

  /**
   * The owning sandbox service. A missing `ctx.k8eSandbox` means the owner
   * did not mount before this seam was used (stale dsh process after a bundle
   * upgrade, or the bundle is not in dsh.profile.bundles) — surface an
   * actionable error instead of a bare TypeError.
   */
  private owner(): K8eSandboxRuntime {
    const owner = this.ctx.k8eSandbox
    if (owner === undefined) {
      throw new Error('k8e-sandbox: ctx.k8eSandbox is not mounted — the dsh-k8e-sandbox bundle is not loaded; reinstall the bundle and restart dsh (k8e-sandbox-cli doctor --fix diagnoses this)')
    }
    return owner
  }

  private async runtime(): Promise<K8eSandboxRuntime> {
    return this.owner()
  }

  private async session(): Promise<string> {
    return (await this.runtime()).getSession()
  }

  private display(path: string, cwd?: string): string {
    return posix.resolve(cwd ?? this.owner().cwd, path)
  }

  /**
   * True when the path lives in the sandbox workspace. Relative paths resolve
   * under the sandbox root; absolute paths must be under it. Host-absolute
   * paths (preflight: AGENTS.md / skills discovery) stay on the local disk.
   */
  private isSandboxPath(path: string): boolean {
    if (!posix.isAbsolute(path)) return true
    const root = this.owner().cwd
    return path === root || path.startsWith(`${root}/`)
  }

  private shellQuote(value: string): string {
    return `'${value.replaceAll('\'', `'"'"'`)}'`
  }

  override async resolve(path: string, opts?: { cwd?: string; signal?: AbortSignal }): Promise<FsTarget> {
    if (path.trim().length === 0) throw new FsError('file_path must be a non-empty string', 'FS_NOT_FOUND')
    const displayPath = this.display(path, opts?.cwd)
    // Phase 1: no realpath round-trip; the canonical path is the resolved path.
    return { targetKey: FsTargetKey(displayPath), displayPath }
  }

  override processPath(target: FsTarget): string {
    return String(target.targetKey)
  }

  override fileUrl(target: FsTarget): string {
    const path = this.processPath(target)
    return `file://${path.split('/').map((segment) => encodeURIComponent(segment)).join('/')}`
  }

  override contains(parent: FsTarget, child: FsTarget): boolean {
    const relative = posix.relative(this.processPath(parent), this.processPath(child))
    return relative === '' || (relative !== '..' && !relative.startsWith('../') && !posix.isAbsolute(relative))
  }

  // ── Local (host) implementation for preflight paths ────────────────────────

  private async localStat(path: string): Promise<StatFacts | undefined> {
    try {
      const info = await stat(path)
      return {
        type: hostType(info),
        size: info.size,
        version: FsVersion(`host:${Math.trunc(info.mtimeMs)}:${info.size}`),
      }
    } catch {
      return undefined
    }
  }

  private async localListDir(path: string): Promise<FsDirEntry[]> {
    const entries: FsDirEntry[] = []
    const dirents = await readdir(path, { withFileTypes: true })
    for (const dirent of dirents) {
      const childPath = posix.join(path, dirent.name)
      const type: FsInfo['type'] = dirent.isDirectory() ? 'directory' : dirent.isFile() ? 'file' : 'other'
      let size: number | undefined
      let version: ReturnType<typeof FsVersion> | undefined
      if (type === 'file') {
        try {
          const info = await stat(childPath)
          size = info.size
          version = FsVersion(`host:${Math.trunc(info.mtimeMs)}:${info.size}`)
        } catch {
          // vanished between readdir and stat → skip entry facts
        }
      }
      entries.push({
        name: dirent.name,
        type,
        target: { targetKey: FsTargetKey(childPath), displayPath: childPath },
        ...(size !== undefined ? { size } : {}),
        ...(version !== undefined ? { version } : {}),
      })
    }
    return entries.sort((left, right) => left.name.localeCompare(right.name))
  }

  private async localReadText(path: string): Promise<string> {
    return readFile(path, 'utf8')
  }

  private async localWriteText(path: string, content: string): Promise<void> {
    await writeFile(path, content, 'utf8')
  }

  // ── Sandbox (remote) implementation ───────────────────────────────────────

  private async probe(path: string): Promise<StatFacts | undefined> {
    const client = (await this.runtime()).getClient()
    const sid = await this.session()
    const code = `stat -c '%f|%s|%Y' ${this.shellQuote(path)} 2>/dev/null || printf 'missing'`
    const result = await client.run(code, { sessionId: sid, timeout: 10 })
    if (result.exitCode !== 0) throw new FsError(`cannot stat "${path}": ${result.stderr}`, 'FS_IO_ERROR')
    const raw = result.stdout.trim()
    if (raw === 'missing') return undefined
    const parts = raw.split('|')
    if (parts.length !== 3) return undefined
    const typeHex = Number.parseInt(parts[0] ?? '', 16)
    const size = Number.parseInt(parts[1] ?? '0', 10)
    const type: FsInfo['type'] = typeHex === 0x4000 ? 'directory' : typeHex === 0x8000 ? 'file' : 'other'
    const version = FsVersion(`k8e:${parts[2] ?? '0'}:${parts[1] ?? '0'}`)
    return { type, size: Number.isFinite(size) ? size : 0, version }
  }

  override async stat(target: FsTarget, signal?: AbortSignal): Promise<FsInfo | undefined> {
    const path = this.processPath(target)
    if (!this.isSandboxPath(path)) {
      const facts = await this.localStat(path)
      if (facts === undefined) return undefined
      return { version: facts.version, type: facts.type, ...(facts.type === 'file' ? { size: facts.size } : {}) }
    }
    const facts = await this.probe(path)
    if (facts === undefined) return undefined
    return { version: facts.version, type: facts.type, ...(facts.type === 'file' ? { size: facts.size } : {}) }
  }

  override async lstat(path: string, opts?: { cwd?: string }, signal?: AbortSignal): Promise<FsPathInfo | undefined> {
    const displayPath = this.display(path, opts?.cwd)
    const info = await this.stat({ targetKey: FsTargetKey(displayPath), displayPath }, signal)
    if (info === undefined) return undefined
    return info
  }

  override async readText(target: FsTarget, signal?: AbortSignal): Promise<string> {
    const path = this.processPath(target)
    try {
      if (!this.isSandboxPath(path)) return await this.localReadText(path)
      const client = (await this.runtime()).getClient()
      const sid = await this.session()
      return await client.read(sid, path)
    } catch (cause) {
      throw new FsError(`cannot read "${target.displayPath}": ${String(cause)}`, 'FS_IO_ERROR', { cause })
    }
  }

  override async streamText(target: FsTarget, signal?: AbortSignal): Promise<AsyncIterable<string>> {
    const content = await this.readText(target, signal)
    return {
      async *[Symbol.asyncIterator](): AsyncGenerator<string> {
        if (content.length > 0) yield content
      },
    }
  }

  override async readBytes(target: FsTarget, signal: AbortSignal | undefined, maxBytes: number): Promise<Uint8Array> {
    const content = await this.readText(target, signal)
    const bytes = new TextEncoder().encode(content)
    if (bytes.byteLength > maxBytes) {
      throw new FsError(`cannot read "${target.displayPath}": ${bytes.byteLength} bytes exceeds the ${maxBytes}-byte limit`, 'FS_TOO_LARGE')
    }
    return bytes
  }

  override async listDir(target: FsTarget, signal?: AbortSignal): Promise<FsDirEntry[]> {
    const path = this.processPath(target)
    if (!this.isSandboxPath(path)) return this.localListDir(path)

    const client = (await this.runtime()).getClient()
    const sid = await this.session()
    const prefix = path === '/' ? '/' : `${path}/`
    const files = await client.list(sid)
    const entries: FsDirEntry[] = []
    for (const file of files) {
      if (!file.path.startsWith(prefix)) continue
      const rest = file.path.slice(prefix.length)
      if (rest.length === 0 || rest.includes('/')) continue // only direct children
      const childPath = file.path
      // Entry facts (type/size) come from the single ListFiles RPC when the
      // gateway reports them; only legacy gateways pay a per-child probe.
      // dsh's FsInfo has no 'symlink' kind, so symlinks surface as 'other'.
      const facts: StatFacts | undefined = file.type !== undefined
        ? { type: file.type === 'symlink' ? 'other' : file.type, size: file.size ?? 0, version: FsVersion(`k8e:${file.modified}:${file.size ?? 0}`) }
        : await this.probe(childPath)
      entries.push({
        name: rest,
        type: facts?.type ?? 'other',
        target: { targetKey: FsTargetKey(childPath), displayPath: childPath },
        ...(facts?.type === 'file' ? { size: facts.size } : {}),
        ...(facts !== undefined ? { version: facts.version } : {}),
      })
    }
    return entries.sort((left, right) => left.name.localeCompare(right.name))
  }

  override async writeText(
    target: FsTarget,
    content: string,
    expected?: FsWriteIntent,
    signal?: AbortSignal,
  ): Promise<FsWriteOutcome> {
    const path = this.processPath(target)
    const local = !this.isSandboxPath(path)
    if (expected?.kind === 'createIfAbsent') {
      const existing = await this.stat(target, signal)
      if (existing !== undefined) {
        throw new FsError(`cannot overwrite existing "${target.displayPath}" without reading it first`, 'FS_NOT_OBSERVED')
      }
    }
    const existing = await this.stat(target, signal)
    const before = existing === undefined ? null : await this.readText(target, signal)
    if (local) {
      await this.localWriteText(path, content)
      const facts = await this.localStat(path)
      return {
        operation: existing === undefined ? 'create' : 'update',
        version: facts?.version ?? FsVersion('host:unknown'),
        before,
        after: content,
      }
    }
    const client = (await this.runtime()).getClient()
    const sid = await this.session()
    await client.write(sid, path, content)
    const facts = await this.probe(path)
    return {
      operation: existing === undefined ? 'create' : 'update',
      version: facts?.version ?? FsVersion('k8e:unknown'),
      before,
      after: content,
    }
  }

  override async editText(
    target: FsTarget,
    edit: FsEditRequest,
    expected?: { version: ReturnType<typeof FsVersion> },
    signal?: AbortSignal,
  ): Promise<FsEditOutcome> {
    const before = await this.readText(target, signal)
    const oldString = edit.oldString
    if (oldString.length === 0) {
      throw new FsError(`cannot edit "${target.displayPath}": old_string must be non-empty`, 'FS_EDIT_NOT_FOUND')
    }
    let matches = 0
    let offset = 0
    while (true) {
      const found = before.indexOf(oldString, offset)
      if (found < 0) break
      matches += 1
      offset = found + oldString.length
    }
    if (matches === 0) throw new FsError(`cannot edit "${target.displayPath}": old_string was not found`, 'FS_EDIT_NOT_FOUND')
    if (!edit.replaceAll && matches !== 1) {
      throw new FsError(`cannot edit "${target.displayPath}": old_string matched ${matches} times`, 'FS_AMBIGUOUS_EDIT')
    }
    const after = edit.replaceAll ? before.split(oldString).join(edit.newString) : before.replace(oldString, edit.newString)
    await this.writeText(target, after, undefined, signal)
    const path = this.processPath(target)
    const version = this.isSandboxPath(path)
      ? (await this.probe(path))?.version ?? FsVersion('k8e:unknown')
      : (await this.localStat(path))?.version ?? FsVersion('host:unknown')
    return { version, before, after }
  }
}

export default K8eFileSystem
