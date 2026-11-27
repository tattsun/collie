import { useEffect, useMemo, useRef, useState } from 'preact/hooks'
import type { Caps, Session } from '../types'
import { api } from '../api'
import { modes, toastError } from '../store'
import { cls, isTouch, shortMode } from '../util'
import { t, ts } from '../i18n'
import { IImage, ISend, IStop, IShield, Spinner } from './icons'
import { Menu } from './ui'
import { Thumbs, pastedImages, useAttachments } from './attach'
import { SessionMeter } from './Usage'

const draftKey = (sid: string) => `collie.draft.${sid}`

export function Composer({ session, caps }: { session: Session; caps: Caps }) {
  const [text, setText] = useState(() => localStorage.getItem(draftKey(session.id)) || '')
  const { atts, setAtts, add: addFiles, remove, uploading } = useAttachments()
  const [sending, setSending] = useState(false)
  const [pick, setPick] = useState(0)
  const ta = useRef<HTMLTextAreaElement>(null)
  const file = useRef<HTMLInputElement>(null)

  useEffect(() => {
    setText(localStorage.getItem(draftKey(session.id)) || '')
    setAtts([])
  }, [session.id])

  useEffect(() => {
    const t = setTimeout(() => {
      if (text) localStorage.setItem(draftKey(session.id), text)
      else localStorage.removeItem(draftKey(session.id))
    }, 300)
    return () => clearTimeout(t)
  }, [text, session.id])

  useEffect(() => {
    const el = ta.current
    if (!el) return
    el.style.height = 'auto'
    el.style.height = Math.min(el.scrollHeight, window.innerHeight * 0.4) + 'px'
  }, [text])

  const slash = useMemo(() => {
    const m = /^\/([\w:-]*)$/.exec(text)
    if (!m || !caps.commands?.length) return []
    const q = m[1].toLowerCase()
    return caps.commands.filter((c) => c.name.toLowerCase().includes(q)).slice(0, 8)
  }, [text, caps.commands])

  const running = session.status === 'running' || session.status === 'starting'
  const canSend = (text.trim().length > 0 || atts.length > 0) && !uploading && !sending

  const submit = async () => {
    if (!canSend) return
    const body = { text, images: atts.map((a) => a.id!) }
    const prev = { text, atts }
    setText('')
    setAtts([])
    setSending(true)
    try {
      await api('POST', `/api/sessions/${session.id}/messages`, body)
      localStorage.removeItem(draftKey(session.id))
    } catch (e) {
      toastError(e)
      setText(prev.text)
      setAtts(prev.atts)
    } finally {
      setSending(false)
      if (!isTouch) ta.current?.focus()
    }
  }

  const interrupt = () => api('POST', `/api/sessions/${session.id}/interrupt`).catch(toastError)

  const onKey = (e: KeyboardEvent) => {
    if (slash.length) {
      if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
        e.preventDefault()
        setPick((p) => (p + (e.key === 'ArrowDown' ? 1 : -1) + slash.length) % slash.length)
        return
      }
      if (e.key === 'Tab' || (e.key === 'Enter' && !e.shiftKey)) {
        e.preventDefault()
        setText('/' + slash[Math.min(pick, slash.length - 1)].name + ' ')
        setPick(0)
        return
      }
    }
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing && (!isTouch || e.metaKey || e.ctrlKey)) {
      e.preventDefault()
      submit()
    }
    if (e.key === 'Escape' && running) interrupt()
  }

  const modeList = modes.value[session.agent] || []
  const modeLabel = shortMode(session.mode)
  const setMode = (mode: string) => api('PATCH', `/api/sessions/${session.id}`, { mode }).catch(toastError)

  return (
    <div
      class="composer"
      onDragOver={(e) => e.preventDefault()}
      onDrop={(e) => {
        e.preventDefault()
        if (e.dataTransfer?.files) addFiles(e.dataTransfer.files)
      }}
    >
      {slash.length > 0 && (
        <div class="slash">
          {slash.map((c, i) => (
            <button
              key={c.name}
              class={cls('slash-item', i === pick && 'on')}
              onMouseDown={(e) => {
                e.preventDefault()
                setText('/' + c.name + ' ')
                ta.current?.focus()
              }}
            >
              <span class="mono">/{c.name}</span>
              {c.hint && <span class="dim mono small"> {c.hint}</span>}
              {c.description && <span class="dim small desc">{c.description}</span>}
            </button>
          ))}
        </div>
      )}
      <div class="composer-box">
        <Thumbs atts={atts} remove={remove} />
        <textarea
          ref={ta}
          rows={1}
          value={text}
          placeholder={session.status === 'waiting' ? t('Reply to decline with instructions…') : running ? t('Send a follow-up…') : t('Message {agent}…', { agent: session.agent === 'claude' ? 'Claude' : 'Codex' })}
          onInput={(e) => {
            setText((e.target as HTMLTextAreaElement).value)
            setPick(0)
          }}
          onKeyDown={onKey}
          onPaste={(e) => {
            const files = pastedImages(e)
            if (files.length) {
              e.preventDefault()
              addFiles(files)
            }
          }}
        />
        <div class="composer-bar">
          <button class="icon-btn" onClick={() => file.current?.click()} aria-label={t('Attach image')} title={t('Attach image')}>
            <IImage size={18} />
          </button>
          <input ref={file} type="file" accept="image/*" multiple hidden onChange={(e) => {
            const t = e.target as HTMLInputElement
            if (t.files) addFiles(t.files)
            t.value = ''
          }} />
          <Menu
            trigger={
              <span class={cls('mode-chip', session.mode)}>
                <IShield size={13} />
                {modeLabel}
              </span>
            }
            items={modeList.map((m) => ({ label: ts(m.label), checked: m.id === session.mode, onClick: () => setMode(m.id) }))}
            up
          />
          <span class="grow" />
          <SessionMeter s={session} up />
          {running && !canSend ? (
            <button class="send stop" onClick={interrupt} aria-label={t('Stop')} title={t('Stop (Esc)')}>
              <IStop size={14} />
            </button>
          ) : (
            <button class="send" disabled={!canSend} onClick={submit} aria-label={t('Send')} title={t('Send (Enter)')}>
              {sending ? <Spinner size={14} /> : <ISend size={17} />}
            </button>
          )}
        </div>
      </div>
    </div>
  )
}
