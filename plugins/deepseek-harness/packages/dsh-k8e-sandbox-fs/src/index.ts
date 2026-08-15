/**
 * k8e-sandbox Service Provider for the filesystem capability seam. Paths and
 * contents live in the sandbox workspace; operations shell out to
 * `k8e-sandbox-cli` in Phase 1 (KIP-20).
 * @module @k8e/dsh-k8e-sandbox-fs
 */

import { Buffer } from 'node:buffer'
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
import type { K8eSandboxRuntime } from '@k8e/dsh-k8e-sandbox'

/** Stat facts returned by the single `stat -c` probe. */
interface StatFacts {
  type: FsInfo['type']
  size: number
  version: ReturnType<typeof FsVersion>
}

/** Remote filesystem backend sharing the session owned by `ctx.k8eSandbox`. */
export class K8eFileSystem extends FileSystem {
  static inject = ['k8eSandbox']

  private async runtime(): Promise<K8eSandboxRuntime> {
    return this.ctx.k8eSandbox
  }

  private async session(): Promise<string> {
    return (await this.runtime()).getSession()
  }

  private display(path: string, cwd?: string): string {
    return posix.resolve(cwd ?? this.ctx.k8eSandbox.cwd, path)
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
    const facts = await this.probe(this.processPath(target))
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
    const client = (await this.runtime()).getClient()
    const sid = await this.session()
    try {
      return await client.read(sid, this.processPath(target))
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
    const client = (await this.runtime()).getClient()
    const sid = await this.session()
    const parent = this.processPath(target)
    const files = await client.list(sid)
    const prefix = parent === '/' ? '/' : `${parent}/`
    const entries: FsDirEntry[] = []
    for (const file of files) {
      if (!file.path.startsWith(prefix)) continue
      const rest = file.path.slice(prefix.length)
      if (rest.length === 0 || rest.includes('/')) continue // only direct children
      const childPath = file.path
      const facts = await this.probe(childPath)
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
    if (expected?.kind === 'createIfAbsent') {
      const existing = await this.stat(target, signal)
      if (existing !== undefined) {
        throw new FsError(`cannot overwrite existing "${target.displayPath}" without reading it first`, 'FS_NOT_OBSERVED')
      }
    }
    const client = (await this.runtime()).getClient()
    const sid = await this.session()
    const before = await this.stat(target, signal).then((info) => info ?? null)
    await client.write(sid, this.processPath(target), content)
    const after = await this.probe(this.processPath(target))
    return {
      operation: before === null ? 'create' : 'update',
      version: after?.version ?? FsVersion('k8e:unknown'),
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
    const version = (await this.probe(this.processPath(target)))?.version ?? FsVersion('k8e:unknown')
    return { version, before, after }
  }
}

export default K8eFileSystem
