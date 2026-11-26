// Minimal i18n: UI strings are written in English and used as keys; a
// dictionary per language translates them (missing entries fall back to
// English). Components re-render on language change because t() reads the
// `lang` signal.
import { signal } from '@preact/signals'
import { ja, jaPatterns } from './i18n-ja'

export type Lang = 'en' | 'ja'

function initial(): Lang {
  const saved = localStorage.getItem('collie.lang')
  if (saved === 'en' || saved === 'ja') return saved
  return (navigator.languages || [navigator.language]).some((l) => l?.toLowerCase().startsWith('ja')) ? 'ja' : 'en'
}

export const lang = signal<Lang>(initial())
document.documentElement.lang = lang.value

export function setLang(l: Lang) {
  lang.value = l
  localStorage.setItem('collie.lang', l)
  document.documentElement.lang = l
}

function fill(s: string, vars?: Record<string, string | number | undefined>): string {
  if (!vars) return s
  return s.replace(/\{(\w+)\}/g, (_, k) => (vars[k] === undefined ? '' : String(vars[k])))
}

/** Translate a UI string. `{name}` placeholders are filled from vars. */
export function t(s: string, vars?: Record<string, string | number | undefined>): string {
  return fill(lang.value === 'ja' ? (ja[s] ?? s) : s, vars)
}

/** Translate a message that came from the server (errors, notices). */
export function ts(msg: string): string {
  if (lang.value !== 'ja' || !msg) return msg
  const exact = ja[msg]
  if (exact) return exact
  for (const [re, out] of jaPatterns) {
    const m = re.exec(msg)
    if (m) return out.replace(/\$(\d)/g, (_, i) => m[Number(i)] ?? '')
  }
  return msg
}
