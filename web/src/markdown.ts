import { t as tr } from './i18n'
import { Marked, type Tokens } from 'marked'

const esc = (s: string) =>
  s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;')

// Links from agents may only go to web pages, mail, this page's anchors or
// Collie's own paths. Browsers drop control characters and whitespace inside
// a URL ("java\tscript:"), so any of them refuses the link.
function safeHref(href: string): string | null {
  const h = href.trim()
  if (/[\x00-\x20\x7f]/.test(h)) return null
  if (/^(https?:|mailto:)/i.test(h) || h.startsWith('#')) return h
  let u: URL
  try {
    u = new URL(h, location.origin)
  } catch {
    return null
  }
  if (u.origin !== location.origin || (u.protocol !== 'https:' && u.protocol !== 'http:')) return null
  return h
}

const md = new Marked({ gfm: true, breaks: true })
md.use({
  renderer: {
    // Raw HTML from the model is shown as text, never interpreted.
    html(t: Tokens.HTML | Tokens.Tag) {
      return esc(t.text)
    },
    link(t: Tokens.Link) {
      const text = this.parser.parseInline(t.tokens)
      const href = safeHref(t.href)
      if (!href) return text
      const title = t.title ? ` title="${esc(t.title)}"` : ''
      return `<a href="${esc(href)}"${title} target="_blank" rel="noopener noreferrer">${text}</a>`
    },
    image(t: Tokens.Image) {
      const href = safeHref(t.href)
      if (!href || !/^https?:/i.test(href)) return esc(t.text)
      return `<a href="${esc(href)}" target="_blank" rel="noopener noreferrer">${esc(t.text || href)}</a>`
    },
    code(t: Tokens.Code) {
      const lang = (t.lang || '').split(/\s/)[0]
      return `<div class="code"><div class="code-bar"><span>${esc(lang)}</span><button class="copy" data-copy>${tr('Copy')}</button></div><pre><code>${esc(t.text)}</code></pre></div>`
    },
  },
})

const cache = new Map<string, string>()

export function renderMarkdown(src: string): string {
  const hit = cache.get(src)
  if (hit !== undefined) return hit
  const out = md.parse(src, { async: false }) as string
  if (src.length < 20000) {
    if (cache.size > 400) cache.clear()
    cache.set(src, out)
  }
  return out
}
