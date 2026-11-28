import { useEffect, useRef, useState } from 'preact/hooks'
import type { TermInfo } from '../types'
import { api, saveClaudeToken } from '../api'
import { terms, toast, toastError } from '../store'
import { cls, copyText, isTouch } from '../util'
import { t } from '../i18n'
import { IPlus, IX, Spinner } from './icons'
import { Modal } from './ui'

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

// Apple keyboards copy and paste with Cmd, so there Ctrl+V and Ctrl+C stay
// the shell's ^V and ^C.
const isApple = /Mac|iPhone|iPad|iPod/.test(navigator.platform)

// xterm.js sends Ctrl+V to the shell as ^V (bash's quoted-insert, so nothing
// seems to happen) and Ctrl+C as ^C even over a selection. Elsewhere than on
// Apple devices, make them work as in Windows Terminal: Ctrl+V and
// Ctrl+Shift+V paste, and Ctrl+C and Ctrl+Shift+C copy the selection; with
// nothing selected, Ctrl+C is still ^C. Both use the browser's own paste and
// copy events, which need no clipboard permission and work over plain HTTP.
// Returns false for keys xterm.js must leave alone.
function clipboardKey(term: XTerm, ev: KeyboardEvent): boolean {
  if (isApple || !ev.ctrlKey || ev.altKey || ev.metaKey) return true
  // keyCode, as xterm.js and the browser's shortcuts go by it on any layout
  if (ev.keyCode === 86) return false // V: the browser pastes, xterm.js sends the paste event's text
  if (ev.keyCode === 67 && ev.type === 'keydown' && term.hasSelection()) {
    ev.preventDefault() // Ctrl+Shift+C would open the browser's DevTools
    const sel = term.getSelection()
    // xterm.js fills the copy event with its selection
    if (!document.execCommand('copy')) copyText(sel).then(() => term.focus())
    term.clearSelection() // the next Ctrl+C interrupts again
    return false
  }
  return true
}

// The terminal's text (scrollback included) as plain lines. Rows the
// terminal soft-wrapped are joined back, so a long token stays one string.
function bufferText(term: XTerm): string {
  const buf = term.buffer.active
  const out: string[] = []
  for (let i = 0; i < buf.length; i++) {
    const line = buf.getLine(i)
    if (!line) continue
    const s = line.translateToString(!buf.getLine(i + 1)?.isWrapped)
    if (line.isWrapped && out.length) out[out.length - 1] += s
    else out.push(s)
  }
  while (out.length && !out[out.length - 1].trim()) out.pop()
  return out.join('\n')
}

// `claude setup-token` prints the token alone in a text block that Ink
// hard-wraps at the terminal width, with a blank line after it. Find the
// latest one and join its rows back; picking it by hand on a phone easily
// drops a character.
const TOKEN_START = 'sk-ant-oat01-'
const TOKEN_ROW = /^[A-Za-z0-9_-]+$/
export function findClaudeToken(text: string): string | null {
  const rows = text.split('\n')
  for (let i = rows.length - 1; i >= 0; i--) {
    const at = rows[i].lastIndexOf(TOKEN_START)
    if (at < 0) continue
    const rest = rows[i].slice(at).trimEnd()
    if (!TOKEN_ROW.test(rest)) continue // something else follows it on the row: not the token output
    let tok = rest
    for (let j = i + 1; j < rows.length && TOKEN_ROW.test(rows[j].trimEnd()); j++) tok += rows[j].trimEnd()
    return tok.length >= TOKEN_START.length + 40 ? tok : null
  }
  return null
}

// Touch browsers cannot select text inside xterm, so show it in a native
// text box where long-press selection and Copy work.
function TextView({ text, onClose }: { text: string; onClose: () => void }) {
  const ta = useRef<HTMLTextAreaElement>(null)
  const token = findClaudeToken(text)
  const [saving, setSaving] = useState(false)
  useEffect(() => {
    if (ta.current) ta.current.scrollTop = ta.current.scrollHeight
  }, [])
  const save = async () => {
    if (!token) return
    setSaving(true)
    try {
      await saveClaudeToken(token)
      toast(t('Saved — used by your agents from their next start'))
      onClose()
    } catch (e) {
      toastError(e)
    } finally {
      setSaving(false)
    }
  }
  return (
    <Modal title={t('Terminal text')} onClose={onClose} wide>
      {token && (
        <div class="card col gap token-found">
          <div class="small">
            {t('Claude token found:')} <code>{token.slice(0, TOKEN_START.length + 6)}…{token.slice(-6)}</code> <span class="dim">({t('{n} characters', { n: token.length })})</span>
          </div>
          <div class="row gap">
            <button class="btn primary" disabled={saving} onClick={save}>
              {saving ? <Spinner size={12} /> : t('Save as my Claude token')}
            </button>
            <button class="btn" onClick={() => copyText(token).then((ok) => toast(ok ? t('Copied') : t('Copy failed')))}>
              {t('Copy token')}
            </button>
          </div>
        </div>
      )}
      <p class="dim small">{t('Long-press to select and copy.')}</p>
      <textarea ref={ta} class="input mono term-text" readOnly value={text} />
      <div class="modal-actions">
        <button class="btn" onClick={() => copyText(text).then((ok) => toast(ok ? t('Copied') : t('Copy failed')))}>
          {t('Copy all')}
        </button>
        <button class="btn primary" onClick={onClose}>
          {t('Close')}
        </button>
      </div>
    </Modal>
  )
}

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
      term.attachCustomKeyEventHandler((ev) => clipboardKey(term, ev))
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
        const ws = new WebSocket(`${proto}//${location.host}/api/terms/${encodeURIComponent(info.id)}/ws`)
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

  // Touch browsers offer no paste menu on the terminal's hidden input, so
  // read the clipboard here (iOS shows its own Paste bubble). Without
  // clipboard access (plain HTTP, denied, empty), ask for the text in a
  // prompt, where the system paste menu works.
  const paste = async () => {
    let text = ''
    try {
      text = await navigator.clipboard.readText()
    } catch {
      /* fall back to the prompt */
    }
    if (!text) text = prompt(t('Paste the text to send to the shell:')) ?? ''
    if (text) termRef.current?.paste(text)
    termRef.current?.focus()
  }

  const [textView, setTextView] = useState<string | null>(null)

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
          {/* click (not pointerdown): clipboard reads need a completed tap */}
          <button class="key" onPointerDown={(e) => e.preventDefault()} onClick={paste}>
            {t('Paste')}
          </button>
          <button
            class="key"
            onClick={() => {
              const tm = termRef.current
              if (!tm) return
              tm.blur() // close the on-screen keyboard
              setTextView(bufferText(tm))
            }}
          >
            {t('Select')}
          </button>
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
      {textView !== null && <TextView text={textView} onClose={() => setTextView(null)} />}
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
            <button class="x" aria-label={t('Close shell')} onClick={() => api('DELETE', `/api/terms/${encodeURIComponent(tm.id)}`).catch(toastError)}>
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
