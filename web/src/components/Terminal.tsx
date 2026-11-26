import { useEffect, useRef, useState } from 'preact/hooks'
import type { TermInfo } from '../types'
import { api } from '../api'
import { terms, toastError } from '../store'
import { cls, isTouch } from '../util'
import { t } from '../i18n'
import { IPlus, IX, Spinner } from './icons'

type XTerm = import('@xterm/xterm').Terminal

// xterm.js is large; load it only when a terminal is actually opened.
let xtermPromise: Promise<[typeof import('@xterm/xterm'), typeof import('@xterm/addon-fit')]> | null = null
function loadXterm() {
  if (!xtermPromise) {
    xtermPromise = Promise.all([import('@xterm/xterm'), import('@xterm/addon-fit'), import('@xterm/xterm/css/xterm.css')]).then(
      ([a, b]) => [a, b] as [typeof import('@xterm/xterm'), typeof import('@xterm/addon-fit')],
    )
  }
  return xtermPromise
}

const KEYS: { label: string; seq: string }[] = [
  { label: 'Esc', seq: '\x1b' },
  { label: 'Tab', seq: '\t' },
  { label: '^C', seq: '\x03' },
  { label: '^D', seq: '\x04' },
  { label: '↑', seq: '\x1b[A' },
  { label: '↓', seq: '\x1b[B' },
  { label: '←', seq: '\x1b[D' },
  { label: '→', seq: '\x1b[C' },
  { label: '|', seq: '|' },
  { label: '~', seq: '~' },
  { label: '/', seq: '/' },
]

function TermPane({ info }: { info: TermInfo }) {
  const host = useRef<HTMLDivElement>(null)
  const wsRef = useRef<WebSocket | null>(null)
  const termRef = useRef<XTerm | null>(null)
  const [state, setState] = useState<'loading' | 'open' | 'closed'>('loading')
  const [ctrl, setCtrl] = useState(false)
  const ctrlRef = useRef(false)
  ctrlRef.current = ctrl

  useEffect(() => {
    let disposed = false
    let ro: ResizeObserver | null = null
    let retry: number | undefined
    let fitFn = () => {}
    loadXterm().then(([{ Terminal }, { FitAddon }]) => {
      if (disposed || !host.current) return
      const dark = matchMedia('(prefers-color-scheme: dark)').matches
      const term = new Terminal({
        fontFamily: 'ui-monospace, "SF Mono", "JetBrains Mono", Menlo, Consolas, monospace',
        fontSize: isTouch ? 12 : 13,
        cursorBlink: true,
        scrollback: 5000,
        allowProposedApi: true,
        theme: dark
          ? { background: '#0c0d0f', foreground: '#e6e6e6', cursor: '#e3a857', selectionBackground: '#3a3d45' }
          : { background: '#ffffff', foreground: '#1d1d1f', cursor: '#b8741a', selectionBackground: '#cfd6e4' },
      })
      const fit = new FitAddon()
      term.loadAddon(fit)
      term.open(host.current)
      termRef.current = term
      fitFn = () => {
        try {
          fit.fit()
        } catch {
          /* not visible */
        }
      }
      fitFn()

      const sendResize = () => {
        const ws = wsRef.current
        if (ws?.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ t: 'resize', c: term.cols, r: term.rows }))
      }
      term.onResize(sendResize)
      term.onData((d) => {
        const ws = wsRef.current
        if (ctrlRef.current && d.length === 1) {
          const c = d.toUpperCase().charCodeAt(0)
          if (c >= 64 && c <= 95) d = String.fromCharCode(c - 64)
          setCtrl(false)
        }
        if (ws?.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ t: 'in', d }))
      })

      const connect = () => {
        if (disposed) return
        const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
        const ws = new WebSocket(`${proto}//${location.host}/api/terms/${info.id}/ws`)
        ws.binaryType = 'arraybuffer'
        wsRef.current = ws
        let first = true
        ws.onopen = () => {
          setState('open')
          sendResize()
        }
        ws.onmessage = (ev) => {
          if (first) {
            term.reset()
            first = false
          }
          term.write(new Uint8Array(ev.data as ArrayBuffer))
        }
        ws.onclose = (ev) => {
          if (disposed || wsRef.current !== ws) return
          if (ev.code === 4000) {
            setState('closed')
            return
          }
          retry = window.setTimeout(connect, ev.code === 4001 ? 50 : 1000)
        }
      }
      connect()
      ro = new ResizeObserver(() => fitFn())
      ro.observe(host.current)
      if (!isTouch) term.focus()
    })
    const onVis = () => {
      if (!document.hidden && wsRef.current?.readyState !== WebSocket.OPEN) fitFn()
    }
    document.addEventListener('visibilitychange', onVis)
    return () => {
      disposed = true
      clearTimeout(retry)
      ro?.disconnect()
      document.removeEventListener('visibilitychange', onVis)
      wsRef.current?.close()
      termRef.current?.dispose()
    }
  }, [info.id])

  const sendKey = (seq: string) => {
    const ws = wsRef.current
    if (ws?.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ t: 'in', d: seq }))
    termRef.current?.focus()
  }

  return (
    <div class="term-pane">
      {state === 'loading' && (
        <div class="term-overlay">
          <Spinner />
        </div>
      )}
      {state === 'closed' && <div class="term-overlay dim">{t('Shell exited.')}</div>}
      <div class="term-host" ref={host} />
      {isTouch && (
        <div class="keybar">
          <button class={cls('key', ctrl && 'on')} onPointerDown={(e) => { e.preventDefault(); setCtrl(!ctrl) }}>
            Ctrl
          </button>
          {KEYS.map((k) => (
            <button key={k.label} class="key" onPointerDown={(e) => { e.preventDefault(); sendKey(k.seq) }}>
              {k.label}
            </button>
          ))}
        </div>
      )}
    </div>
  )
}

export function TerminalView({ owner, projectId, sessionId }: { owner: string; projectId?: string; sessionId?: string }) {
  const mine = terms.value.filter((t) => t.owner === owner)
  const [active, setActive] = useState<string | null>(null)
  const [creating, setCreating] = useState(false)
  const autoCreated = useRef(false)

  const create = async () => {
    setCreating(true)
    try {
      const t = await api<TermInfo>('POST', '/api/terms', { projectId, sessionId, cols: 100, rows: 30 })
      if (!terms.value.some((x) => x.id === t.id)) terms.value = [...terms.value, t]
      setActive(t.id)
    } catch (e) {
      toastError(e)
    } finally {
      setCreating(false)
    }
  }

  useEffect(() => {
    if (mine.length === 0 && !autoCreated.current) {
      autoCreated.current = true
      create()
    }
  }, [owner])

  const cur = mine.find((t) => t.id === active) || mine[mine.length - 1]

  return (
    <div class="terminal-view">
      <div class="term-tabs">
        {mine.map((tm, i) => (
          <div key={tm.id} class={cls('term-tab', cur?.id === tm.id && 'on')}>
            <button onClick={() => setActive(tm.id)}>{t('Shell {n}', { n: i + 1 })}</button>
            <button class="x" aria-label={t('Close shell')} onClick={() => api('DELETE', `/api/terms/${tm.id}`).catch(toastError)}>
              <IX size={11} />
            </button>
          </div>
        ))}
        <button class="term-add" onClick={create} disabled={creating} aria-label={t('New shell')}>
          {creating ? <Spinner size={12} /> : <IPlus size={14} />}
        </button>
      </div>
      {cur ? (
        <TermPane key={cur.id} info={cur} />
      ) : (
        <div class="center dim pad">{creating ? <Spinner /> : t('No shell open.')}</div>
      )}
    </div>
  )
}
