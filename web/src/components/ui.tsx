import { Fragment } from 'preact'
import type { ComponentChildren, JSX } from 'preact'
import { t } from '../i18n'
import { useEffect, useRef, useState } from 'preact/hooks'
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

export function Menu({ trigger, items, up, align = 'left', label }: { trigger: ComponentChildren; items: MenuItem[]; up?: boolean; align?: 'left' | 'right'; label?: string }) {
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

/** Fill a translated sentence's {key} placeholders with elements (code, links…). */
export function rich(s: string, parts: Record<string, ComponentChildren>) {
  return s.split(/\{(\w+)\}/).map((x, i) => <Fragment key={i}>{i % 2 ? parts[x] : x}</Fragment>)
}
