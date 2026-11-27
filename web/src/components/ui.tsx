import { Fragment } from 'preact'
import type { ComponentChildren, JSX } from 'preact'
import { t } from '../i18n'
import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks'
import { toasts } from '../store'
import type { Agent, Status } from '../types'
import { cls } from '../util'
import { ICheck, IX } from './icons'

export interface MenuItem {
  label: string
  onClick: () => void
  checked?: boolean
  danger?: boolean
  icon?: JSX.Element
  hint?: string
}

/** Open state of a dropdown that closes on an outside tap or Escape. */
function useDropdown() {
  const [open, setOpen] = useState(false)
  const ref = useRef<HTMLDivElement>(null)
  useEffect(() => {
    if (!open) return
    const close = (e: Event) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false)
    }
    const esc = (e: KeyboardEvent) => e.key === 'Escape' && setOpen(false)
    document.addEventListener('pointerdown', close, true)
    document.addEventListener('keydown', esc)
    return () => {
      document.removeEventListener('pointerdown', close, true)
      document.removeEventListener('keydown', esc)
    }
  }, [open])
  return { open, setOpen, ref }
}

export function Menu({ trigger, items, up, align = 'left', label }: { trigger: ComponentChildren; items: MenuItem[]; up?: boolean; align?: 'left' | 'right'; label?: string }) {
  const { open, setOpen, ref } = useDropdown()
  return (
    <div class="menu-wrap" ref={ref}>
      <button class="menu-trigger" onClick={() => setOpen(!open)} aria-haspopup="menu" aria-expanded={open} aria-label={label}>
        {trigger}
      </button>
      {open && (
        <div class={cls('menu', up && 'up', align)} role="menu">
          {items.map((it, i) => (
            <button
              key={i}
              role="menuitem"
              class={cls('menu-item', it.danger && 'danger')}
              onClick={() => {
                setOpen(false)
                it.onClick()
              }}
            >
              <span class="mi-icon">{it.checked ? <ICheck size={14} /> : it.icon}</span>
              <span class="mi-label">{it.label}</span>
              {it.hint && <span class="mi-hint">{it.hint}</span>}
            </button>
          ))}
        </div>
      )}
    </div>
  )
}

/** A button that opens a panel of arbitrary content (like Menu, not a list). */
export function Popover({ trigger, children, up, align = 'left', label, title }: { trigger: ComponentChildren; children: ComponentChildren; up?: boolean; align?: 'left' | 'right'; label?: string; title?: string }) {
  const { open, setOpen, ref } = useDropdown()
  const panel = useRef<HTMLDivElement>(null)
  // Keep the panel on screen (a phone is narrower than it may be wide).
  useLayoutEffect(() => {
    const el = panel.current
    if (!open || !el) return
    el.style.transform = ''
    const r = el.getBoundingClientRect()
    const dx = r.left < 8 ? 8 - r.left : r.right > innerWidth - 8 ? innerWidth - 8 - r.right : 0
    if (dx) el.style.transform = `translateX(${dx}px)`
  }, [open])
  return (
    <div class="menu-wrap" ref={ref}>
      <button class="menu-trigger" onClick={() => setOpen(!open)} aria-haspopup="dialog" aria-expanded={open} aria-label={label} title={title}>
        {trigger}
      </button>
      {open && (
        <div ref={panel} class={cls('menu pop', up && 'up', align)} role="dialog" aria-label={label}>
          {children}
        </div>
      )}
    </div>
  )
}

export function Modal({ title, onClose, children, wide }: { title: string; onClose: () => void; children: ComponentChildren; wide?: boolean }) {
  useEffect(() => {
    const esc = (e: KeyboardEvent) => e.key === 'Escape' && onClose()
    document.addEventListener('keydown', esc)
    return () => document.removeEventListener('keydown', esc)
  }, [onClose])
  return (
    <div class="modal-bg" onPointerDown={(e) => e.target === e.currentTarget && onClose()}>
      <div class={cls('modal', wide && 'wide')} role="dialog" aria-label={title}>
        <div class="modal-head">
          <h2>{title}</h2>
          <button class="icon-btn" onClick={onClose} aria-label={t('Close')}>
            <IX size={18} />
          </button>
        </div>
        <div class="modal-body">{children}</div>
      </div>
    </div>
  )
}

export function Toasts() {
  return (
    <div class="toasts" aria-live="polite">
      {toasts.value.map((t) => (
        <div key={t.id} class={cls('toast', t.kind)}>
          {t.text}
        </div>
      ))}
    </div>
  )
}

const statusLabel: Record<string, string> = {
  starting: 'Starting',
  running: 'Working',
  waiting: 'Needs you',
  idle: 'Ready',
  stopped: 'Ready',
  error: 'Error',
}

export function StatusDot({ status, unread }: { status: Status; unread?: boolean }) {
  return <span class={cls('dot', status, unread && 'unread')} title={statusLabel[status] ? t(statusLabel[status]) : status} />
}

export function StatusPill({ status }: { status: Status }) {
  return (
    <span class={cls('pill', status)}>
      <span class={cls('dot', status)} />
      {statusLabel[status] ? t(statusLabel[status]) : status}
    </span>
  )
}

export function AgentBadge({ agent, small }: { agent: Agent; small?: boolean }) {
  return <span class={cls('agent', agent, small && 'small')}>{agent === 'claude' ? 'Claude' : 'Codex'}</span>
}

export function Segmented<T extends string>({ value, options, onChange }: { value: T; options: { value: T; label: ComponentChildren; disabled?: boolean }[]; onChange: (v: T) => void }) {
  return (
    <div class="seg" role="radiogroup">
      {options.map((o) => (
        <button
          key={o.value}
          role="radio"
          aria-checked={o.value === value}
          class={cls('seg-btn', o.value === value && 'on')}
          disabled={o.disabled}
          onClick={() => onChange(o.value)}
        >
          {o.label}
        </button>
      ))}
    </div>
  )
}

export function Toggle({ checked, onChange, label, hint, disabled }: { checked: boolean; onChange: (v: boolean) => void; label: string; hint?: string; disabled?: boolean }) {
  return (
    <label class={cls('toggle', disabled && 'disabled')}>
      <input type="checkbox" checked={checked} disabled={disabled} onChange={(e) => onChange((e.target as HTMLInputElement).checked)} />
      <span class="track">
        <span class="knob" />
      </span>
      <span class="toggle-text">
        <span>{label}</span>
        {hint && <span class="dim small">{hint}</span>}
      </span>
    </label>
  )
}

const UNIT: Record<string, number> = { B: 1, kB: 1e3, KB: 1e3, MB: 1e6, GB: 1e9, TB: 1e12, KiB: 2 ** 10, MiB: 2 ** 20, GiB: 2 ** 30, TiB: 2 ** 40 }

/** Parse podman's "173MB" into bytes. */
function bytes(s: string | undefined): number | null {
  const m = /^([\d.]+)\s*([kKMGT]?i?B)$/.exec((s || '').trim())
  return m && UNIT[m[2]] ? parseFloat(m[1]) * UNIT[m[2]] : null
}

/** Bytes in the units limits are configured with (4g = 4 GiB). */
function size(b: number): string {
  if (b < 2 ** 30) return `${Math.max(1, Math.round(b / 2 ** 20))}MB`
  const g = b / 2 ** 30
  return `${g < 10 ? Math.round(g * 10) / 10 : Math.round(g)}GB`
}

/** "Memory 165MB of 4GB · CPU 1.7% · 21 processes" from podman stats. */
export function ContainerUsage({ mem, cpu, pids, limited }: { mem?: string; cpu?: string; pids?: string; limited?: boolean }) {
  const [u, l] = (mem || '').split('/')
  const used = bytes(u)
  const limit = limited !== false ? bytes(l) : null
  const usedText = used !== null ? size(used) : (u || '').trim()
  const memText = limit ? t('Memory {used} of {limit}', { used: usedText, limit: size(limit) }) : t('Memory {used}', { used: usedText })
  const parts = [memText, cpu && t('CPU {pct}', { pct: cpu }), pids && t('{n} procs', { n: pids })].filter(Boolean)
  return <span class="dim">{parts.join(' · ')}</span>
}

/** Fill a translated sentence's {key} placeholders with elements (code, links…). */
export function rich(s: string, parts: Record<string, ComponentChildren>) {
  return s.split(/\{(\w+)\}/).map((x, i) => <Fragment key={i}>{i % 2 ? parts[x] : x}</Fragment>)
}
