/**
 * The sandbox terminal panel (lazy chunk): xterm.js rendering the remote PTY
 * bridged by the host over /k8e-sandbox/ws/terminal, plus a read-only command
 * log fed by the /k8e-sandbox/activity SSE (running commands live). Loaded on
 * first open via the chunk loader, so xterm never ships in the core bundle.
 * xterm + @xterm/addon-fit are INLINED here; react/react-dom come from the
 * module table.
 */

import { createElement, useEffect, useRef, useState } from 'react'
import type { CSSProperties, Dispatch, ReactNode, SetStateAction } from 'react'
import { createRoot } from 'react-dom/client'
import { Terminal } from 'xterm'
import { FitAddon } from '@xterm/addon-fit'
import { loadPrefs } from './prefs.ts'
import type { ExecLogEntry, K8eExecEvent } from './exec-types.ts'

export interface TerminalViewProps {
  rows?: number
  cols?: number
}

export function TerminalView(props: TerminalViewProps): ReturnType<typeof createElement> {
  const hostRef = useRef<HTMLDivElement | null>(null)

  useEffect(() => {
    const host = hostRef.current
    if (host === null) return
    let socket: WebSocket | undefined
    let term: Terminal | undefined
    let fit: FitAddon | undefined

    term = new Terminal({
      rows: props.rows ?? 24,
      cols: props.cols ?? 80,
      fontSize: 13,
      cursorBlink: true,
    })
    fit = new FitAddon()
    term.loadAddon(fit)
    term.open(host)
    fit.fit()

    const onResize = (): void => { fit?.fit() }

    const query = new URLSearchParams({ rows: String(term.rows), cols: String(term.cols) })
    socket = new WebSocket(`/k8e-sandbox/ws/terminal?${query.toString()}`)
    socket.binaryType = 'arraybuffer'
    socket.onmessage = (event) => {
      term?.write(typeof event.data === 'string' ? event.data : new Uint8Array(event.data as ArrayBuffer))
    }
    socket.onclose = () => {
      term?.write('\r\n[terminal closed]\r\n')
    }
    // Buffer input/resize during the brief CONNECTING window and flush once the
    // socket opens, so early keystrokes are not silently dropped.
    const pending: string[] = []
    const send = (data: string): void => {
      if (socket?.readyState === WebSocket.OPEN) socket.send(data)
      else if (socket?.readyState === WebSocket.CONNECTING) pending.push(data)
    }
    socket.onopen = () => {
      for (const chunk of pending.splice(0)) {
        if (socket?.readyState === WebSocket.OPEN) socket.send(chunk)
      }
    }
    term.onData((data) => { send(data) })
    term.onResize((size) => {
      send(JSON.stringify({ type: 'resize', cols: size.cols, rows: size.rows }))
    })
    window.addEventListener('resize', onResize)

    return () => {
      window.removeEventListener('resize', onResize)
      if (socket?.readyState === WebSocket.OPEN) {
        socket.send(JSON.stringify({ type: 'close' }))
      }
      socket?.close()
      term?.dispose()
    }
  }, [props.rows, props.cols])

  return createElement('div', {
    ref: hostRef,
    style: { width: '100%', height: '100%', background: '#1e1e1e' },
  })
}

type SetEntries = Dispatch<SetStateAction<ExecLogEntry[]>>

function applyExecEvent(ev: K8eExecEvent, setEntries: SetEntries): void {
  if (ev.phase === 'start') {
    setEntries((prev) => [...prev, {
      id: ev.id,
      command: ev.command,
      cwd: ev.cwd,
      startedAt: ev.at,
      stdout: '',
      stderr: '',
      exitCode: null,
      signal: null,
      settled: false,
    }])
  } else if (ev.phase === 'output') {
    setEntries((prev) => prev.map((entry) => {
      if (entry.id !== ev.id) return entry
      return ev.stream === 'stderr'
        ? { ...entry, stderr: entry.stderr + ev.data }
        : { ...entry, stdout: entry.stdout + ev.data }
    }))
  } else {
    setEntries((prev) => prev.map((entry) => (
      entry.id === ev.id ? { ...entry, exitCode: ev.exitCode, signal: ev.signal, settled: true } : entry
    )))
  }
}

const logStyle: CSSProperties = {
  flex: '0 0 auto',
  maxHeight: '45%',
  overflowY: 'auto',
  padding: '8px 10px',
  background: '#161616',
  borderBottom: '1px solid #2a2a2a',
  fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace',
  fontSize: '12px',
  lineHeight: '1.5',
}

const cmdStyle: CSSProperties = { color: '#7fd4c1', whiteSpace: 'pre-wrap', wordBreak: 'break-all' }
const outStyle: CSSProperties = { color: '#d6d6d6', whiteSpace: 'pre-wrap', wordBreak: 'break-all' }
const errStyle: CSSProperties = { color: '#e5979a', whiteSpace: 'pre-wrap', wordBreak: 'break-all' }
const exitStyle: CSSProperties = { color: '#888', marginBottom: '8px' }

function ExecLogRow(props: { entry: ExecLogEntry }): ReactNode {
  const { entry } = props
  const exitLabel = entry.exitCode === null ? '[exited]' : `[exit ${entry.exitCode}]`
  return createElement('div', { style: { marginBottom: '4px' } },
    createElement('div', { style: cmdStyle }, '$ ' + entry.command),
    entry.stdout.length > 0 ? createElement('div', { style: outStyle }, entry.stdout) : null,
    entry.stderr.length > 0 ? createElement('div', { style: errStyle }, entry.stderr) : null,
    entry.settled
      ? createElement('div', { style: exitStyle }, exitLabel)
      : createElement('div', { style: exitStyle }, '…'),
  )
}

function CommandLog(): ReactNode {
  const [entries, setEntries] = useState<ExecLogEntry[]>([])
  const listRef = useRef<HTMLDivElement | null>(null)

  useEffect(() => {
    const es = new EventSource('/k8e-sandbox/activity')
    es.addEventListener('history', (event) => {
      try {
        const parsed = JSON.parse((event as MessageEvent).data as string) as { entries: ExecLogEntry[] }
        if (Array.isArray(parsed.entries)) setEntries(parsed.entries)
      } catch {
        // Ignore malformed history; live events still arrive.
      }
    })
    es.addEventListener('exec', (event) => {
      try {
        applyExecEvent(JSON.parse((event as MessageEvent).data as string) as K8eExecEvent, setEntries)
      } catch {
        // Ignore malformed frames.
      }
    })
    return () => { es.close() }
  }, [])

  useEffect(() => {
    const el = listRef.current
    if (el !== null) el.scrollTop = el.scrollHeight
  }, [entries])

  if (entries.length === 0) {
    return createElement('div', { style: logStyle },
      createElement('div', { style: { color: '#666' } }, 'No sandbox commands yet.'),
    )
  }

  return createElement('div', { ref: listRef, style: logStyle },
    entries.map((entry) => createElement(ExecLogRow, { key: entry.id, entry })),
  )
}

const headerStyle: CSSProperties = {
  flex: '0 0 auto',
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'space-between',
  padding: '6px 10px',
  background: '#222',
  color: '#eee',
  fontSize: '13px',
  borderBottom: '1px solid #2a2a2a',
}

const closeButtonStyle: CSSProperties = {
  border: 'none',
  background: 'transparent',
  color: '#999',
  fontSize: '18px',
  lineHeight: '1',
  cursor: 'pointer',
}

/** The terminal content without floating chrome: command log + interactive PTY. */
export function SandboxTerminalTab(props: { rows?: number; cols?: number }): ReactNode {
  return createElement('div', { style: { display: 'flex', flexDirection: 'column', width: '100%', height: '100%' } },
    createElement(CommandLog, {}),
    createElement('div', { style: { flex: 1, minHeight: '120px', background: '#1e1e1e' } },
      createElement(TerminalView, { rows: props.rows, cols: props.cols }),
    ),
  )
}

/** One live sandbox terminal panel: header + command log + interactive PTY. */
function SandboxTerminalPanel(props: { rows?: number; cols?: number; onClose: () => void }): ReactNode {
  return createElement('div', { style: { display: 'flex', flexDirection: 'column', width: '100%', height: '100%' } },
    createElement('div', { style: headerStyle },
      createElement('span', null, 'K8E Sandbox Terminal'),
      createElement('button', { type: 'button', style: closeButtonStyle, onClick: props.onClose, 'aria-label': 'close' }, '×'),
    ),
    createElement(SandboxTerminalTab, { rows: props.rows, cols: props.cols }),
  )
}

let activePanel: { host: HTMLDivElement; close: () => void } | undefined

/** Open (or focus) the terminal panel; idempotent across repeated activity. */
export function openTerminalPanel(): void {
  if (activePanel !== undefined) {
    activePanel.host.style.boxShadow = '0 8px 34px rgba(0,0,0,.65)'
    return
  }
  const prefs = loadPrefs()
  const host = document.createElement('div')
  host.dataset.k8eSandboxTerminal = ''
  Object.assign(host.style, {
    position: 'fixed',
    right: '16px',
    bottom: '16px',
    width: 'min(680px, calc(100vw - 32px))',
    height: 'min(520px, calc(100vh - 32px))',
    zIndex: '2147483000',
    borderRadius: '10px',
    overflow: 'hidden',
    boxShadow: '0 8px 30px rgba(0,0,0,.4)',
  } as CSSStyleDeclaration)
  document.body.appendChild(host)

  const root = createRoot(host)
  const close = (): void => {
    activePanel = undefined
    root.unmount()
    host.remove()
  }
  activePanel = { host, close }
  root.render(createElement(SandboxTerminalPanel, { rows: prefs.rows, cols: prefs.cols, onClose: close }))
}
