export function ago(ms: number): string {
  const s = Math.max(0, (Date.now() - ms) / 1000)
  if (s < 45) return 'now'
  if (s < 3600) return `${Math.round(s / 60)}m`
  if (s < 86400) return `${Math.round(s / 3600)}h`
  if (s < 86400 * 7) return `${Math.round(s / 86400)}d`
  return new Date(ms).toLocaleDateString(undefined, { month: 'short', day: 'numeric' })
}

export function duration(ms?: number): string {
  if (!ms) return ''
  const s = ms / 1000
  if (s < 60) return `${s < 10 ? s.toFixed(1) : Math.round(s)}s`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m}m ${Math.round(s % 60)}s`
  return `${Math.floor(m / 60)}h ${m % 60}m`
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
export const shortMode = (m: string) => SHORT_MODE[m] || m
