// Schedule timing: the simple choices the form offers, as cron expressions
// (minute hour day-of-month month day-of-week) and back, and how to show
// them. Anything else is a custom expression the server checks.
import { lang, t } from './i18n'

export type Freq = 'daily' | 'weekdays' | 'weekly' | 'monthly' | 'hourly' | 'custom'

export interface Timing {
  freq: Freq
  /** HH:MM (daily, weekdays, weekly, monthly) */
  time: string
  /** 0 = Sunday (weekly) */
  days: number[]
  /** day of the month (monthly) */
  dom: number
  /** minute past the hour (hourly) */
  minute: number
  /** the expression itself (custom) */
  cron: string
}

export const defaultTiming = (): Timing => ({ freq: 'daily', time: '09:00', days: [1], dom: 1, minute: 0, cron: '0 9 * * *' })

const pad = (n: number) => String(n).padStart(2, '0')

export function toCron(x: Timing): string {
  const [h, m] = (x.time || '09:00').split(':').map((v) => parseInt(v, 10) || 0)
  switch (x.freq) {
    case 'daily':
      return `${m} ${h} * * *`
    case 'weekdays':
      return `${m} ${h} * * 1-5`
    case 'weekly':
      return `${m} ${h} * * ${[...new Set(x.days)].sort((a, b) => a - b).join(',') || '1'}`
    case 'monthly':
      return `${m} ${h} ${x.dom} * *`
    case 'hourly':
      return `${x.minute} * * * *`
  }
  return x.cron.trim()
}

const num = (s: string, lo: number, hi: number) => (/^\d{1,2}$/.test(s) && +s >= lo && +s <= hi ? +s : null)

/** Reads an expression back into the simple choices, or as custom. */
export function fromCron(expr: string): Timing {
  const base = { ...defaultTiming(), freq: 'custom' as Freq, cron: expr }
  const f = expr.trim().split(/\s+/)
  if (f.length !== 5) return base
  const [mi, ho, dom, mon, dow] = f
  const m = num(mi, 0, 59)
  if (m === null || mon !== '*') return base
  if (ho === '*' && dom === '*' && dow === '*') return { ...base, freq: 'hourly', minute: m }
  const h = num(ho, 0, 23)
  if (h === null) return base
  const time = `${pad(h)}:${pad(m)}`
  if (dom === '*' && dow === '*') return { ...base, freq: 'daily', time }
  if (dom === '*' && dow === '1-5') return { ...base, freq: 'weekdays', time }
  if (dom === '*' && /^[0-7](,[0-7])*$/.test(dow)) {
    const days = [...new Set(dow.split(',').map((d) => +d % 7))].sort((a, b) => a - b)
    return { ...base, freq: 'weekly', time, days }
  }
  const d = num(dom, 1, 31)
  if (d !== null && dow === '*') return { ...base, freq: 'monthly', time, dom: d }
  return base
}

const locale = () => (lang.value === 'ja' ? 'ja-JP' : 'en-US')

/** Short weekday name, 0 = Sunday. */
export function weekday(d: number): string {
  return new Intl.DateTimeFormat(locale(), { weekday: 'short', timeZone: 'UTC' }).format(new Date(Date.UTC(2023, 0, 1 + d)))
}

/** "Every day at 09:00", "Fridays at 18:00"… or the expression itself. */
export function describeCron(expr: string): string {
  const x = fromCron(expr)
  switch (x.freq) {
    case 'daily':
      return t('Every day at {time}', { time: x.time })
    case 'weekdays':
      return t('Weekdays at {time}', { time: x.time })
    case 'weekly':
      return t('Every {days} at {time}', { days: x.days.map(weekday).join(lang.value === 'ja' ? '・' : ', '), time: x.time })
    case 'monthly':
      return t('Day {day} of every month at {time}', { day: x.dom, time: x.time })
    case 'hourly':
      return t('Every hour at :{minute}', { minute: pad(x.minute) })
  }
  return expr
}

export const browserTZ = () => Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC'

/** Time zones to choose from (the browser's own first). */
export function timeZones(current: string): string[] {
  let all: string[] = []
  try {
    all = Intl.supportedValuesOf('timeZone')
  } catch {
    /* older browsers: just the ones below */
  }
  return [...new Set([current, browserTZ(), 'UTC', ...all].filter(Boolean))]
}

/** A moment, as a weekday, date and time in tz (named when it isn't the browser's). */
export function fmtWhen(ms: number, tz?: string): string {
  const opts: Intl.DateTimeFormatOptions = { weekday: 'short', month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit', hourCycle: 'h23' }
  if (tz && tz !== browserTZ()) {
    opts.timeZone = tz
    opts.timeZoneName = 'short'
  }
  try {
    return new Intl.DateTimeFormat(locale(), opts).format(new Date(ms))
  } catch {
    delete opts.timeZone
    delete opts.timeZoneName
    return new Intl.DateTimeFormat(locale(), opts).format(new Date(ms))
  }
}
