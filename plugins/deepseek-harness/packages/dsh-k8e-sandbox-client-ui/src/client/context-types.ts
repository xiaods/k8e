/**
 * Local structural contracts for the client half (mirror of DSH-better-sidebar's
 * context-types.ts): the `@deepseek-ai/*` client service type packages are not
 * resolvable from a third-party workspace, so the faces this plugin touches are
 * restated structurally. Drift is contained to this file.
 */

export type { Context } from '@deepseek-ai/cordis'

/** The client slots service face (register + inject). */
export interface SandboxClientSlots {
  register(options: {
    name: string
    id?: string
    key?: string
    order?: number
    label?: string | (() => string)
    locale?: string
    inject?: (...args: any[]) => Record<string, unknown>
    children?: Record<string, unknown>
    [k: string]: unknown
  }, component: unknown): () => void
  inject(key: string, callback: () => () => void): () => void
}

/** The client locale service face (bind + getSnapshot/subscribe + register). */
export interface SandboxClientLocale {
  getSnapshot(): { active: string; revision?: number }
  subscribe(fn: () => void): () => void
  /** Register one namespace's dictionaries: `{ zh: {...}, en: {...} }`. */
  register(ns: string, dicts: Record<string, Record<string, string>>): () => void
  /** Namespace-bound translate; repeat binds return the same function. */
  bind(ns: string): (key: string, params?: Record<string, string>) => string
}

/**
 * Minimal structural contract of DSH-better-sidebar's `ctx.betterSidebar`
 * service (registerTab only). Declared locally so this plugin stays an
 * optional soft dependency: `ctx.get('betterSidebar')` returns `undefined`
 * when better-sidebar is not installed, and registration is skipped.
 */
export interface BetterSidebarService {
  registerTab(descriptor: {
    id: string
    title: string | (() => string)
    icon?: unknown
    order?: number
    hidden?: boolean
    single?: boolean
    component: (props: { visible: boolean; [k: string]: unknown }) => unknown
    [k: string]: unknown
  }): () => void
}

declare module '@deepseek-ai/cordis' {
  interface Context {
    slots: SandboxClientSlots
    locale: SandboxClientLocale
    betterSidebar?: BetterSidebarService
    effect(fn: () => void | (() => void), label?: string): void
  }
}
