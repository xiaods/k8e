/**
 * Lazy chunk loader (mirror of DSH-better-sidebar's chunk-loader): the heavy
 * xterm stack lives in `lib/client-terminal.js`, fetched on first terminal open
 * so startup parses only the core bundle.
 */

export type ChunkName = 'terminal'

interface ChunkModuleSystem {
  import(specifier: string): Promise<unknown>
}

function moduleSystem(): ChunkModuleSystem | undefined {
  return (globalThis as { __DSH_MODULES__?: ChunkModuleSystem }).__DSH_MODULES__
}

interface ChunkRegistry {
  [name: string]: ((require: (spec: string) => unknown) => Record<string, unknown>) | undefined
}

function chunkRegistry(): ChunkRegistry {
  const g = globalThis as { __dshChunks__?: ChunkRegistry }
  g.__dshChunks__ ??= {}
  return g.__dshChunks__
}

const CHUNK_URL = (name: ChunkName): string => `/k8e-sandbox/bundle/${name}.js`

const cache = new Map<ChunkName, Promise<Record<string, unknown>>>()

function scriptLoader(src: string): Promise<void> {
  return new Promise((resolve, reject) => {
    const el = document.createElement('script')
    el.async = true
    el.src = src
    el.addEventListener('load', () => { el.remove(); resolve() }, { once: true })
    el.addEventListener('error', () => { el.remove(); reject(new Error(`chunk script ${src} failed to load`)) }, { once: true })
    document.head.append(el)
  })
}

export function loadChunk(name: ChunkName): Promise<Record<string, unknown>> {
  const cached = cache.get(name)
  if (cached !== undefined) return cached
  const task = (async (): Promise<Record<string, unknown>> => {
    const modules = moduleSystem()
    if (modules === undefined) throw new Error(`chunk "${name}": client module system unavailable`)
    await scriptLoader(CHUNK_URL(name))
    const factory = chunkRegistry()[name]
    if (typeof factory !== 'function') throw new Error(`chunk "${name}" script did not register its factory`)
    const externals = new Map<string, unknown>()
    await Promise.all(['react', 'react/jsx-runtime', 'react-dom/client'].map(async (spec) => {
      try { externals.set(spec, await modules.import(spec)) } catch { externals.set(spec, undefined) }
    }))
    const require = (spec: string): unknown => {
      if (!externals.has(spec)) throw new Error(`chunk require('${spec}') missed the module table`)
      return externals.get(spec)
    }
    return factory(require)
  })()
  cache.set(name, task)
  void task.catch(() => { cache.delete(name) })
  return task
}

export function resetChunks(): void {
  cache.clear()
}
