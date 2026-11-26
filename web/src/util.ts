import { lang, t } from './i18n'

const ja = () => lang.value === 'ja'

/** Compact span ("5m", "3h", "2d" / "5分", "3時間", "2日"), or '' past a week. */
function span(s: number): string {
  if (s < 3600) return ja() ? `${Math.max(1, Math.round(s / 60))}分` : `${Math.max(1, Math.round(s / 60))}m`
  if (s < 86400) return ja() ? `${Math.round(s / 3600)}時間` : `${Math.round(s / 3600)}h`
  if (s < 86400 * 7) return ja() ? `${Math.round(s / 86400)}日` : `${Math.round(s / 86400)}d`
  return ''
}

const date = (ms: number) => new Date(ms).toLocaleDateString(ja() ? 'ja-JP' : undefined, { month: 'short', day: 'numeric' })

/** How long ago: "now" / "5m" (en), "たった今" / "5分前" (ja), or a date. */
export function ago(ms: number): string {
  const s = Math.max(0, (Date.now() - ms) / 1000)
  if (s < 45) return ja() ? 'たった今' : 'now'
  const v = span(s)
  if (!v) return date(ms)
  return ja() ? v + '前' : v
}

/** Time left until ms (future): "6d" / "6日". */
export function until(ms: number): string {
  const s = Math.max(0, (ms - Date.now()) / 1000)
  if (s < 45) return ja() ? '1分' : '1m'
  return span(s) || date(ms)
}

export function duration(ms?: number): string {
  if (!ms) return ''
  const s = ms / 1000
  const sec = s < 10 ? s.toFixed(1) : String(Math.round(s))
  if (s < 60) return ja() ? `${sec}秒` : `${sec}s`
  const m = Math.floor(s / 60)
  if (m < 60) return ja() ? `${m}分${Math.round(s % 60)}秒` : `${m}m ${Math.round(s % 60)}s`
  return ja() ? `${Math.floor(m / 60)}時間${m % 60}分` : `${Math.floor(m / 60)}h ${m % 60}m`
}

export function money(usd?: number): string {
  if (!usd) return ''
  return usd < 0.01 ? '<$0.01' : `$${usd.toFixed(2)}`
}

export function tokens(n?: number): string {
  if (!n) return '0'
  if (n < 1000) return String(n)
  if (n < 1_000_000) return `${(n / 1000).toFixed(n < 10_000 ? 1 : 0)}k`
  return `${(n / 1_000_000).toFixed(1)}M`
}

/** Path relative to base when it is inside it. */
export function rel(p: string | undefined, base: string | undefined): string {
  if (!p) return ''
  if (base && p.startsWith(base + '/')) return p.slice(base.length + 1)
  if (base && p === base) return '.'
  return p
}

export function home(p: string): string {
  return p.replace(/^\/home\/[^/]+/, '~').replace(/^\/root(?=\/|$)/, '~')
}

export function cls(...xs: (string | false | null | undefined)[]): string {
  return xs.filter(Boolean).join(' ')
}

export async function copyText(text: string): Promise<boolean> {
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text)
      return true
    }
  } catch {
    /* fall through */
  }
  // Plain-http fallback (e.g. a tailnet IP without TLS).
  const ta = document.createElement('textarea')
  ta.value = text
  ta.style.cssText = 'position:fixed;top:-1000px;opacity:0'
  document.body.appendChild(ta)
  ta.select()
  let ok = false
  try {
    ok = document.execCommand('copy')
  } catch {
    ok = false
  }
  ta.remove()
  return ok
}

export const isTouch = matchMedia('(pointer: coarse)').matches

export function firstLine(s: string | undefined, n = 120): string {
  if (!s) return ''
  const line = s.trim().split('\n', 1)[0]
  return line.length > n ? line.slice(0, n - 1) + '…' : line
}

const SHORT_MODE: Record<string, string> = {
  default: 'Ask first',
  acceptEdits: 'Accept edits',
  plan: 'Plan',
  auto: 'Auto',
  bypassPermissions: 'Bypass',
  dontAsk: "Don't ask",
  untrusted: 'Untrusted',
  'read-only': 'Read only',
  full: 'Full access',
}
export const shortMode = (m: string) => (SHORT_MODE[m] ? t(SHORT_MODE[m]) : m)
