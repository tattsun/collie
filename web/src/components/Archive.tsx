// Archiving a session straight from a list: swipe a row left on a phone,
// or press the button that shows on a hovered (or keyboard-focused) row.
import type { ComponentChildren } from 'preact'
import { signal } from '@preact/signals'
import { useEffect, useRef, useState } from 'preact/hooks'
import type { Session } from '../types'
import { api } from '../api'
import { groupById, me, projectById, sessionById, sessions, toast, toastError } from '../store'
import { t } from '../i18n'
import { cls } from '../util'
import { IArchive, IUnarchive } from './icons'

/** Whether the server lets me archive s (its sessionAdmin): the person who
 *  started it, or an owner of its project's group. */
export function canArchive(s: Session): boolean {
  if (s.owner && s.owner === me.value?.id) return true
  const g = projectById.value.get(s.projectId)?.ownerGroup
  return !!g && groupById.value.get(g)?.myRole === 'owner'
}

function setLocal(id: string, archived: boolean) {
  sessions.value = sessions.value.map((x) => (x.id === id ? { ...x, archived } : x))
}

/** Archives a session (or brings it back) right away; the toast can undo it. */
export async function setArchived(s: Session, archived: boolean, undoable = true): Promise<boolean> {
  setLocal(s.id, archived)
  try {
    await api('PATCH', `/api/sessions/${s.id}`, { archived })
  } catch (e) {
    setLocal(s.id, !archived)
    toastError(e)
    return false
  }
  if (!undoable) return true
  // Archiving only tidies the list: an agent at work carries on.
  const busy = s.status === 'running' || s.status === 'starting' || !!s.tasks?.length
  const detail = !archived
    ? undefined
    : s.status === 'waiting'
      ? t('Archiving doesn’t stop its agent: it still waits for you.')
      : busy
        ? t('Archiving doesn’t stop its agent: it keeps working.')
        : undefined
  toast(archived ? t('Archived “{title}”', { title: s.title }) : t('Unarchived “{title}”', { title: s.title }), 'info', {
    detail,
    ms: 6000,
    action: { label: t('Undo'), run: () => void setArchived(s, !archived, false) },
  })
  return true
}

const ACT = 104 // width of the button a swipe uncovers
const FULL = 0.5 // swiping past this share of the row archives on release
const SLOP = 10
// In a browser tab, touches starting this close to a side of the screen
// are left to the browser: Safari's back and forward swipes start there.
// The Home Screen app has no such swipes, and a thumb often starts there.
const EDGE = 24
const standalone = () => matchMedia('(display-mode: standalone)').matches || (navigator as { standalone?: boolean }).standalone === true

/** The row swiped open, showing its button (one at a time). */
const openRow = signal<object | null>(null)
/** Until when a tap on a row only closes the row that was open. */
let swallowUntil = 0

/** Wraps a session's row (its link) with the archive actions. keep: the
 *  row stays in the list once archived (it shows archived sessions). */
export function Archivable({ s, card, keep, children }: { s: Session; card?: boolean; keep?: boolean; children: ComponentChildren }) {
  const token = useRef({}).current
  const wrap = useRef<HTMLDivElement>(null)
  const g = useRef<{ id: number; x: number; y: number; base: number; h: boolean; cur: number } | null>(null)
  const quietUntil = useRef(0) // the click a swipe may still fire is not a tap
  const pending = useRef(false) // a double click archives once, not back and forth
  const [drag, setDrag] = useState<number | null>(null)
  const [leaving, setLeaving] = useState(false)
  const open = openRow.value === token

  useEffect(() => {
    if (!open) return
    // A touch elsewhere closes the row; on another row it does only that.
    const outside = (e: PointerEvent) => {
      if (wrap.current?.contains(e.target as Node)) return
      openRow.value = null
      if ((e.target as Element).closest?.('.sw')) swallowUntil = Date.now() + 600
    }
    document.addEventListener('pointerdown', outside, true)
    return () => document.removeEventListener('pointerdown', outside, true)
  }, [open])
  useEffect(
    () => () => {
      if (openRow.value === token) openRow.value = null
    },
    [],
  )

  if (!canArchive(s)) return <>{children}</>

  const width = () => wrap.current?.offsetWidth || 0
  // The next row's link, for the keyboard focus once this row is gone.
  const neighbour = () => {
    const el = wrap.current
    const rows = el?.parentElement ? [...el.parentElement.querySelectorAll<HTMLElement>(':scope > .sw')] : []
    const i = el ? rows.indexOf(el) : -1
    return (rows[i + 1] || rows[i - 1])?.querySelector<HTMLElement>('a') || null
  }
  const run = (keyboard: boolean) => {
    if (pending.current) return
    pending.current = true
    const next = keyboard ? neighbour() : null
    if (openRow.value === token) openRow.value = null
    const cur = sessionById.value.get(s.id) || s
    setArchived(cur, !cur.archived).then(() => {
      pending.current = false
      if (!wrap.current?.isConnected) next?.focus()
      else setLeaving(false)
    })
  }
  // From a swipe: the row slides away first, unless it stays in the list.
  const commit = () => {
    if (leaving || pending.current) return
    if (openRow.value === token) openRow.value = null
    if (keep) return run(false)
    setLeaving(true)
    setTimeout(() => run(false), 200)
  }

  const down = (e: PointerEvent) => {
    if (e.pointerType === 'mouse' || !e.isPrimary || leaving) return
    if ((e.clientX < EDGE || e.clientX > innerWidth - EDGE) && !standalone()) return
    g.current = { id: e.pointerId, x: e.clientX, y: e.clientY, base: open ? -ACT : 0, h: false, cur: 0 }
  }
  const move = (e: PointerEvent) => {
    const d = g.current
    if (!d || e.pointerId !== d.id) return
    const dx = e.clientX - d.x
    const dy = e.clientY - d.y
    if (!d.h) {
      if (Math.abs(dx) < SLOP && Math.abs(dy) < SLOP) return
      // A scroll, or a right swipe on a closed row: not ours.
      if (Math.abs(dy) >= Math.abs(dx) || (dx > 0 && !d.base)) {
        g.current = null
        return
      }
      d.h = true
      ;(e.currentTarget as Element).setPointerCapture?.(e.pointerId)
      if (openRow.value && openRow.value !== token) openRow.value = null
    }
    d.cur = Math.max(-width(), Math.min(0, d.base + dx))
    setDrag(d.cur)
  }
  const up = (e: PointerEvent) => {
    const d = g.current
    if (!d || e.pointerId !== d.id) return
    g.current = null
    if (!d.h) return
    quietUntil.current = Date.now() + 400
    setDrag(null)
    if (e.type === 'pointercancel') return
    if (d.cur < -width() * FULL) commit()
    else if (d.cur < -ACT / 2) openRow.value = token
    else if (open) openRow.value = null
  }
  const click = (e: MouseEvent) => {
    if ((e.target as Element).closest('.sw-btn')) return
    const now = Date.now()
    if (open || now < quietUntil.current || now < swallowUntil) {
      e.preventDefault()
      e.stopPropagation()
      swallowUntil = 0
      if (open) openRow.value = null
    }
  }

  const offset = drag ?? (open ? -ACT : 0)
  const armed = leaving || (drag !== null && drag < -width() * FULL)
  const label = s.archived ? t('Unarchive') : t('Archive')
  const Icon = s.archived ? IUnarchive : IArchive
  return (
    <div ref={wrap} class={cls('sw', card && 'card-sw', drag !== null && 'dragging', (leaving || offset < 0) && 'swiped')}>
      {/* the touch twin of .sw-btn, which is the one for keyboards and screen readers */}
      <div class={cls('sw-under', armed && 'armed')} aria-hidden="true">
        <button tabIndex={-1} class="sw-act" style={{ width: leaving ? '100%' : `${Math.max(ACT, -offset)}px` }} onClick={commit}>
          <Icon size={18} />
          <span>{label}</span>
        </button>
      </div>
      <div
        class="sw-top"
        style={leaving ? { transform: 'translateX(-100%)' } : offset ? { transform: `translateX(${offset}px)` } : undefined}
        onPointerDown={down}
        onPointerMove={move}
        onPointerUp={up}
        onPointerCancel={up}
        onClickCapture={click}
      >
        {children}
        <button
          class="sw-btn"
          title={label}
          aria-label={s.archived ? t('Unarchive “{title}”', { title: s.title }) : t('Archive “{title}”', { title: s.title })}
          onClick={(e) => run(e.detail === 0)}
        >
          <Icon size={15} />
        </button>
      </div>
    </div>
  )
}
