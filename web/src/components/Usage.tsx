// Plan usage (rate-limit windows of my agent accounts) and context fill,
// shown in the sidebar and next to the composer so they are always in view.
import { Fragment } from 'preact'
import type { Agent, Session, UsageWindow } from '../types'
import { clock, me, usage, userName } from '../store'
import { cls, money, tokens, until } from '../util'
import { lang, t } from '../i18n'
import { AgentBadge, Popover } from './ui'

const ORDER = ['five_hour', 'primary', 'seven_day', 'secondary', 'seven_day_opus', 'seven_day_sonnet', 'seven_day_overage_included']
const rank = (w: UsageWindow) => {
  const i = ORDER.indexOf(w.id)
  return i < 0 ? ORDER.length : i
}
const sortWindows = (ws: UsageWindow[]) => [...ws].sort((a, b) => rank(a) - rank(b) || a.id.localeCompare(b.id))

function spanName(mins: number): string {
  if (mins === 300) return t('5h')
  if (mins === 10080) return t('Week')
  if (mins % 1440 === 0) return t('{n}d', { n: mins / 1440 })
  if (mins % 60 === 0) return t('{n}h', { n: mins / 60 })
  return t('{n}m', { n: mins })
}

/** Short window name: "5h", "Week", "Opus"… */
function windowLabel(w: UsageWindow): string {
  switch (w.id) {
    case 'five_hour':
      return t('5h')
    case 'seven_day':
      return t('Week')
    // model-scoped weekly windows
    case 'seven_day_opus':
      return 'Opus'
    case 'seven_day_sonnet':
      return 'Sonnet'
    case 'seven_day_overage_included':
      return w.label || 'Fable'
  }
  const kind = w.id.split(':').pop()
  const base = w.mins ? spanName(w.mins) : kind === 'secondary' ? t('Week') : t('5h')
  return w.label ? `${w.label} ${base}` : base
}

/** A window that has reset since it was reported starts over. */
const live = (w: UsageWindow, now: number) => (w.resetsAt && w.resetsAt <= now ? { ...w, pct: 0, resetsAt: undefined } : w)
const level = (pct: number) => (pct >= 90 ? 'hi' : pct >= 70 ? 'mid' : '')
const pctText = (pct: number) => `${Math.round(pct)}%`

/** "15:00", or "Sun, Nov 30 15:00" when not today. */
function at(ms: number): string {
  const d = new Date(ms)
  const loc = lang.value === 'ja' ? 'ja-JP' : undefined
  const time = d.toLocaleTimeString(loc, { hour: '2-digit', minute: '2-digit' })
  if (d.toDateString() === new Date().toDateString()) return time
  return `${d.toLocaleDateString(loc, { month: 'short', day: 'numeric', weekday: 'short' })} ${time}`
}

function describe(w: UsageWindow): string {
  const vars = { label: windowLabel(w), pct: Math.round(w.pct) }
  return w.resetsAt ? t('{label}: {pct}% used · resets {when}', { ...vars, when: at(w.resetsAt) }) : t('{label}: {pct}% used', vars)
}

function Bar({ pct }: { pct: number }) {
  return (
    <span class={cls('ubar', level(pct))}>
      <i style={{ width: `${Math.min(100, Math.max(0, pct))}%` }} />
    </span>
  )
}

function Ring({ pct }: { pct: number }) {
  const r = 5.5
  const c = 2 * Math.PI * r
  return (
    <svg class="ring" width="14" height="14" viewBox="0 0 14 14" aria-hidden="true">
      <circle cx="7" cy="7" r={r} class="ring-bg" />
      <circle cx="7" cy="7" r={r} class="ring-fg" stroke-dasharray={`${(c * Math.min(100, pct)) / 100} ${c}`} transform="rotate(-90 7 7)" />
    </svg>
  )
}

const agents: Agent[] = ['claude', 'codex']

/** Sidebar: my accounts' plan usage, one row per window. */
export function UsagePanel() {
  const all = usage.value
  const now = clock.value
  const shown = agents.filter((a) => all[a]?.windows?.length)
  if (!shown.length) return null
  return (
    <div class="usage-panel" aria-label={t('Usage')}>
      {shown.map((a) =>
        sortWindows(all[a].windows).map((raw, i) => {
          const w = live(raw, now)
          const tip = `${describe(w)}\n${t('Updated {time}', { time: at(all[a].updatedAt) })}`
          return (
            <Fragment key={a + w.id}>
              <span class="u-agent">{i === 0 && <AgentBadge agent={a} small />}</span>
              <span class="u-label" title={tip}>
                {windowLabel(w)}
              </span>
              <span class="u-bar" title={tip}>
                <Bar pct={w.pct} />
              </span>
              <span class={cls('u-pct', level(w.pct))}>{pctText(w.pct)}</span>
              <span class="u-reset" title={w.resetsAt ? at(w.resetsAt) : undefined}>
                {w.resetsAt ? t('in {time}', { time: until(w.resetsAt) }) : ''}
              </span>
            </Fragment>
          )
        }),
      )}
    </div>
  )
}

/** Session: context fill and the account's tightest window, details on tap. */
export function SessionMeter({ s, up, align = 'right' }: { s: Session; up?: boolean; align?: 'left' | 'right' }) {
  const now = clock.value
  // The agent runs on its owner's account: only they see that plan.
  const mine = !s.owner || s.owner === me.value?.id
  const u = mine ? usage.value[s.agent] : undefined
  const ws = u ? sortWindows(u.windows || []).map((w) => live(w, now)) : []
  const top = ws.reduce<UsageWindow | null>((m, w) => (!m || w.pct > m.pct ? w : m), null)
  const ctx = s.ctxMax ? Math.min(100, ((s.ctxUsed || 0) / s.ctxMax) * 100) : null
  if (ctx === null && !top && !s.costUsd) return null
  const ctxText = ctx !== null ? t('Context {used} / {max}', { used: tokens(s.ctxUsed), max: tokens(s.ctxMax) }) : ''
  const title = [ctxText, top && describe(top)].filter(Boolean).join('\n')
  return (
    <Popover
      up={up}
      align={align}
      label={t('Usage')}
      title={title}
      trigger={
        <span class="meter">
          {ctx !== null && (
            <span class={cls('m-ctx', level(ctx))}>
              <Ring pct={ctx} />
              {pctText(ctx)}
            </span>
          )}
          {top && (
            <span class={cls('m-plan', level(top.pct))}>
              {windowLabel(top)} {pctText(top.pct)}
            </span>
          )}
        </span>
      }
    >
      <div class="meter-pop">
        {(ctx !== null || ws.length > 0) && (
          <div class="mp-grid">
            {ctx !== null && (
              <>
                <span class="mp-l">{t('Context')}</span>
                <Bar pct={ctx} />
                <span class={cls('mp-pct', level(ctx))}>{pctText(ctx)}</span>
                <span class="mp-r">
                  {tokens(s.ctxUsed)} / {tokens(s.ctxMax)}
                </span>
              </>
            )}
            {ws.length > 0 && <div class="mp-h">{t('{agent} plan usage', { agent: s.agent === 'claude' ? 'Claude' : 'Codex' })}</div>}
            {ws.map((w) => (
              <Fragment key={w.id}>
                <span class="mp-l">{windowLabel(w)}</span>
                <Bar pct={w.pct} />
                <span class={cls('mp-pct', level(w.pct))}>{pctText(w.pct)}</span>
                <span class="mp-r" title={w.resetsAt ? at(w.resetsAt) : undefined}>
                  {w.resetsAt ? t('resets in {time}', { time: until(w.resetsAt) }) : ''}
                </span>
              </Fragment>
            ))}
          </div>
        )}
        {u && ws.length > 0 && <div class="mp-note">{t('Updated {time}', { time: at(u.updatedAt) })}</div>}
        {!mine && <div class="mp-note">{t('Runs on {name}’s account', { name: userName(s.owner) })}</div>}
        {!!s.costUsd && (
          <div class="mp-cost">
            <span>{t('Cost of this session')}</span>
            <span>{money(s.costUsd)}</span>
          </div>
        )}
      </div>
    </Popover>
  )
}
