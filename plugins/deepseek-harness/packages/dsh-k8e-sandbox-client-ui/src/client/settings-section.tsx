/**
 * The K8E Sandbox settings page (registered into `settings.section`). Shows
 * the sandbox status (host /k8e-sandbox/api/status) and the user-level terminal
 * preferences; the "打开终端" action lazy-loads the terminal chunk.
 */

import { createElement, useEffect, useState } from 'react'
import type { ChangeEvent, CSSProperties, ReactNode } from 'react'
import { loadChunk } from './chunk-loader.ts'
import { loadPrefs, savePrefs, type TerminalPrefs } from './prefs.ts'

export interface K8eSettingsSectionProps {
  t: (key: string, params?: Record<string, string>) => string
}

interface Status {
  ok: boolean
  grpcAvailable: boolean
  cwd?: string
  endpoint?: string
  runtimeClass?: string
}

const box: CSSProperties = {
  background: 'var(--dsw-alias-bg-layer-1, #1f1f1f)',
  border: '1px solid var(--dsw-alias-border, #333)',
  borderRadius: '8px',
  padding: '14px 16px',
}

const labelStyle: CSSProperties = {
  display: 'block',
  fontSize: '12px',
  color: 'var(--dsw-alias-label-secondary, #999)',
  marginBottom: '6px',
}

const fieldStyle: CSSProperties = {
  width: '100%',
  boxSizing: 'border-box',
  padding: '8px 10px',
  borderRadius: '6px',
  border: '1px solid var(--dsw-alias-border, #333)',
  background: 'var(--dsw-alias-bg-input, transparent)',
  color: 'var(--dsw-alias-label-primary, #eee)',
  fontSize: '13px',
}

const buttonStyle: CSSProperties = {
  padding: '8px 14px',
  borderRadius: '6px',
  border: '1px solid var(--dsw-alias-border, #333)',
  background: 'var(--dsw-alias-bg-button, #2a2a2a)',
  color: 'var(--dsw-alias-label-primary, #eee)',
  cursor: 'pointer',
  fontSize: '13px',
}

const primaryButtonStyle: CSSProperties = {
  ...buttonStyle,
  background: 'var(--dsw-alias-accent, #2f6fed)',
  borderColor: 'var(--dsw-alias-accent, #2f6fed)',
  color: '#fff',
}

function Field(props: { label: string; children: ReactNode }): ReactNode {
  return createElement('div', { style: { marginBottom: '14px' } },
    createElement('label', { style: labelStyle }, props.label),
    props.children,
  )
}

export function K8eSettingsSection(props: K8eSettingsSectionProps): ReactNode {
  const { t } = props
  const [status, setStatus] = useState<Status | undefined>(undefined)
  const [prefs, setPrefs] = useState<TerminalPrefs>(() => loadPrefs())
  const [saved, setSaved] = useState(false)
  const [opening, setOpening] = useState(false)

  useEffect(() => {
    let cancelled = false
    void (async () => {
      try {
        const res = await fetch('/k8e-sandbox/api/status', { method: 'POST' })
        const body = (await res.json()) as Status
        if (!cancelled) setStatus(body)
      } catch {
        if (!cancelled) setStatus({ ok: false, grpcAvailable: false })
      }
    })()
    return () => { cancelled = true }
  }, [])

  const openTerminal = async (): Promise<void> => {
    if (opening) return
    setOpening(true)
    try {
      const chunk = await loadChunk('terminal')
      const { openTerminalPanel } = chunk as { openTerminalPanel: () => void }
      openTerminalPanel()
    } finally {
      setOpening(false)
    }
  }

  const onSave = (): void => {
    savePrefs(prefs)
    setSaved(true)
    window.setTimeout(() => setSaved(false), 1500)
  }

  const connected = status?.grpcAvailable === true

  return createElement('div', { style: { maxWidth: '560px' } },
    createElement('h3', { style: { margin: '0 0 14px', fontSize: '16px', color: 'var(--dsw-alias-label-primary, #eee)' } },
      t('section.title')),

    createElement('div', { style: box },
      createElement('div', { style: { fontSize: '13px', color: 'var(--dsw-alias-label-secondary, #999)', marginBottom: '8px' } },
        t('status.heading')),
      createElement('div', { style: { fontSize: '13px', color: 'var(--dsw-alias-label-primary, #eee)', marginBottom: '6px' } },
        connected
          ? createElement('span', { style: { color: '#4caf50' } }, '● ' + t('status.connected'))
          : createElement('span', { style: { color: '#e5a50a' } }, '● ' + t('status.noGrpc'))),
      status?.cwd !== undefined
        ? createElement('div', { style: { fontSize: '13px', color: 'var(--dsw-alias-label-secondary, #999)' } },
          t('status.cwd') + ': ' + status.cwd)
        : null,
      status?.endpoint !== undefined
        ? createElement('div', { style: { fontSize: '13px', color: 'var(--dsw-alias-label-secondary, #999)' } },
          'endpoint: ' + (status.endpoint || '—'))
        : null,
      status?.runtimeClass !== undefined
        ? createElement('div', { style: { fontSize: '13px', color: 'var(--dsw-alias-label-secondary, #999)' } },
          'runtimeClass: ' + status.runtimeClass)
        : null,
      createElement('div', { style: { fontSize: '12px', color: 'var(--dsw-alias-label-secondary, #999)', marginTop: '10px' } },
        t('hint.hostConfig')),
    ),

    createElement('div', { style: { ...box, marginTop: '16px' } },
      createElement('div', { style: { fontSize: '13px', color: 'var(--dsw-alias-label-secondary, #999)', marginBottom: '14px' } },
        t('prefs.heading')),

      Field({ label: t('prefs.rows'), children: createElement('input', {
        type: 'number', min: 1, max: 200, value: prefs.rows, style: fieldStyle,
        onChange: (e: ChangeEvent<HTMLInputElement>) => {
          const n = Number(e.target.value)
          setPrefs({ ...prefs, rows: Number.isFinite(n) ? n : prefs.rows })
        },
      }) }),

      Field({ label: t('prefs.cols'), children: createElement('input', {
        type: 'number', min: 1, max: 400, value: prefs.cols, style: fieldStyle,
        onChange: (e: ChangeEvent<HTMLInputElement>) => {
          const n = Number(e.target.value)
          setPrefs({ ...prefs, cols: Number.isFinite(n) ? n : prefs.cols })
        },
      }) }),

      Field({ label: t('prefs.autoOpen'), children: createElement('input', {
        type: 'checkbox',
        checked: prefs.autoOpenTerminal,
        onChange: (e: ChangeEvent<HTMLInputElement>) => {
          setPrefs({ ...prefs, autoOpenTerminal: e.target.checked })
        },
      }) }),

      createElement('div', { style: { display: 'flex', gap: '10px', alignItems: 'center', marginTop: '18px' } },
        createElement('button', { type: 'button', style: primaryButtonStyle, onClick: openTerminal, disabled: opening },
          opening ? '…' : t('actions.openTerminal')),
        createElement('button', { type: 'button', style: buttonStyle, onClick: onSave },
          saved ? t('actions.saved') : t('actions.save')),
      ),
    ),
  )
}
