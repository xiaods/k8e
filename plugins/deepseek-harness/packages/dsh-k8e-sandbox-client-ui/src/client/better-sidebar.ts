/**
 * Optional DSH-better-sidebar integration (KIP-20 M4): when better-sidebar is
 * installed (its client half provides `ctx.betterSidebar`), register a
 * `k8e-sandbox:terminal` tab that renders the same terminal panel as the
 * floating window. The terminal content lives in the lazy chunk, so this tab
 * costs the core bundle nothing until opened.
 */

import { createElement, useEffect, useState } from 'react'
import type { ReactNode } from 'react'
import type { Context } from './context-types.ts'
import { loadChunk } from './chunk-loader.ts'

/** Tab content: lazily load the terminal chunk on first mount. */
function K8eTerminalTab(): ReactNode {
  const [component, setComponent] = useState<((props: { rows?: number; cols?: number }) => ReactNode) | undefined>(undefined)

  useEffect(() => {
    let cancelled = false
    void loadChunk('terminal')
      .then((chunk) => {
        if (cancelled) return
        setComponent(() => (chunk as { SandboxTerminalTab: (props: { rows?: number; cols?: number }) => ReactNode }).SandboxTerminalTab)
      })
      .catch(() => undefined)
    return () => { cancelled = true }
  }, [])

  if (component === undefined) {
    return createElement('div', { style: { padding: '12px', color: '#999', fontSize: '13px' } }, 'Loading terminal…')
  }
  return createElement(component, {})
}

/** Register the terminal tab; a no-op when better-sidebar is absent. */
export function registerBetterSidebarTab(ctx: Context): void {
  const betterSidebar = ctx.get('betterSidebar')
  if (betterSidebar === undefined) return
  ctx.effect(() => betterSidebar.registerTab({
    id: 'k8e-sandbox:terminal',
    title: 'K8E Terminal',
    order: 45,
    single: true,
    component: () => createElement(K8eTerminalTab, {}),
  }), 'k8e-sandbox-ui: better-sidebar terminal tab')
}
