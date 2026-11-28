// What agents show the user: images, screen recordings and HTML pages
// (show_media), and screenshots seen in tool results.
//
// HTML pages (and SVG) come from the agent and are untrusted. They are only
// ever loaded into a sandboxed iframe without allow-same-origin, and the
// server sends them with a CSP sandbox of its own (so they get an opaque
// origin even when opened directly): no access to Collie's cookies, storage
// or API, no forms, popups or navigation of the app, and nothing loaded
// from the network. Scripts are off unless the user turns them on for the
// page being viewed; even then they stay sandboxed and offline.
import { signal } from '@preact/signals'
import { useEffect, useRef, useState } from 'preact/hooks'
import type { Item, Media } from '../types'
import { cls } from '../util'
import { closeOverlayBack, openOverlay } from '../store'
import { t } from '../i18n'
import { IBack, IChevron, ICode, IExpand, IExternal, IX } from './icons'

const url = (id: string, js = false) => `/api/uploads/${encodeURIComponent(id)}${js ? '?js=1' : ''}`

type View = 'image' | 'video' | 'html'
const viewOf = (m: Media): View => (m.mime.startsWith('video/') ? 'video' : m.mime.startsWith('text/html') ? 'html' : 'image')

/** Images given by upload name (attachments, screenshots in tool results). */
export const imagesAsMedia = (ids: string[]): Media[] => ids.map((id) => ({ id, mime: 'image/' + (id.split('.').pop() || 'png') }))

const viewer = signal<{ list: Media[]; index: number } | null>(null)

export function openViewer(list: Media[], index: number) {
  viewer.value = { list, index }
  // A history entry of its own: a back swipe closes the viewer, not the screen under it.
  openOverlay(() => (viewer.value = null))
}

/** An untrusted HTML page. Never add allow-same-origin: with scripts it would let the page lift its own sandbox. */
function PageFrame({ m, js = false, class: c, lazy }: { m: Media; js?: boolean; class?: string; lazy?: boolean }) {
  return (
    <iframe
      // A fresh element per mode: a changed sandbox attribute only applies to later navigations.
      key={m.id + (js ? ':js' : '')}
      class={c}
      sandbox={js ? 'allow-scripts' : ''}
      src={url(m.id, js)}
      referrerpolicy="no-referrer"
      loading={lazy ? 'lazy' : undefined}
      title={m.caption || m.name || 'HTML'}
    />
  )
}

function Tile({ m, onOpen }: { m: Media; onOpen: () => void }) {
  const v = viewOf(m)
  return (
    <figure class={cls('media-tile', v)}>
      {v === 'image' && (
        <button class="media-img" onClick={onOpen} aria-label={t('Open')}>
          <img src={url(m.id)} loading="lazy" alt={m.caption || m.name || ''} />
        </button>
      )}
      {v === 'video' && <video src={url(m.id)} controls playsInline preload="metadata" />}
      {v === 'html' && (
        <div class="media-page">
          <PageFrame m={m} lazy />
          <button class="media-page-open" onClick={onOpen}>
            <span class="pill">
              <ICode size={13} /> {t('Open preview')}
            </span>
          </button>
        </div>
      )}
      <figcaption>
        <span class="media-cap">
          {m.caption && <span>{m.caption}</span>}
          {m.name && <span class="dim mono">{m.name}</span>}
        </span>
        <button class="icon-btn tiny" onClick={onOpen} title={t('Full screen')} aria-label={t('Full screen')}>
          <IExpand size={13} />
        </button>
      </figcaption>
    </figure>
  )
}

/** Files the agent showed with show_media. */
export function MediaCard({ item }: { item: Item }) {
  const list = item.media || []
  return (
    <div class="media-card">
      {item.text && <div class="media-title">{item.text}</div>}
      <div class={cls('media-grid', list.length === 1 && 'one')}>
        {list.map((m, i) => (
          <Tile key={m.id} m={m} onOpen={() => openViewer(list, i)} />
        ))}
      </div>
    </div>
  )
}

/** Small thumbnails that open in the viewer. */
export function Thumbstrip({ ids, class: c }: { ids: string[]; class?: string }) {
  const list = imagesAsMedia(ids)
  return (
    <div class={cls('shots', c)}>
      {list.map((m, i) => (
        <button key={m.id} class="shot" onClick={() => openViewer(list, i)} aria-label={t('Open')}>
          <img src={url(m.id)} loading="lazy" alt="" />
        </button>
      ))}
    </div>
  )
}

/** Full-screen viewer; mounted once at the app's root. */
export function Viewer() {
  const v = viewer.value
  const [js, setJs] = useState(false)
  const touch = useRef<{ x: number; y: number } | null>(null)
  const cur = v ? v.list[v.index] : null
  const n = v ? v.list.length : 0

  const close = () => closeOverlayBack()
  const go = (d: number) => {
    const s = viewer.value
    if (s && s.list.length > 1) viewer.value = { ...s, index: (s.index + d + s.list.length) % s.list.length }
  }

  // Scripts are allowed for one page at a time, never carried over.
  useEffect(() => setJs(false), [cur?.id])
  useEffect(() => {
    if (!v) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') close()
      else if (e.key === 'ArrowLeft') go(-1)
      else if (e.key === 'ArrowRight') go(1)
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [!!v])

  if (!v || !cur) return null
  const view = viewOf(cur)
  return (
    <div class={cls('viewer', view)} role="dialog" aria-modal="true" aria-label={cur.caption || cur.name || t('Preview')}>
      <div class="viewer-bar">
        {n > 1 && <span class="viewer-count">{`${v.index + 1} / ${n}`}</span>}
        <span class="viewer-name">
          {cur.caption && <span>{cur.caption}</span>}
          {cur.name && <span class="dim mono">{cur.name}</span>}
        </span>
        {view === 'html' && (
          <button class={cls('btn small', js ? 'danger' : 'ghost')} onClick={() => setJs(!js)} title={t('Scripts run sandboxed and offline, without access to Collie.')}>
            <ICode size={13} /> {js ? t('Stop scripts') : t('Run scripts')}
          </button>
        )}
        {/* Never with scripts: a page of its own could navigate away with what it read. */}
        <a class="icon-btn" href={url(cur.id)} target="_blank" rel="noopener noreferrer" title={t('Open in a new tab')} aria-label={t('Open in a new tab')}>
          <IExternal size={17} />
        </a>
        <button class="icon-btn" onClick={close} title={t('Close')} aria-label={t('Close')}>
          <IX size={18} />
        </button>
      </div>
      <div
        class="viewer-stage"
        onClick={(e) => e.target === e.currentTarget && close()}
        onTouchStart={(e) => (touch.current = { x: e.touches[0].clientX, y: e.touches[0].clientY })}
        onTouchEnd={(e) => {
          const s = touch.current
          touch.current = null
          if (!s || view === 'html') return
          const dx = e.changedTouches[0].clientX - s.x
          const dy = e.changedTouches[0].clientY - s.y
          if (Math.abs(dx) > 60 && Math.abs(dx) > 2 * Math.abs(dy)) go(dx < 0 ? 1 : -1)
        }}
      >
        {view === 'image' && <img key={cur.id} src={url(cur.id)} alt={cur.caption || cur.name || ''} />}
        {view === 'video' && <video key={cur.id} src={url(cur.id)} controls autoPlay playsInline />}
        {view === 'html' && <PageFrame m={cur} js={js} class="viewer-page" />}
        {n > 1 && (
          <>
            <button class="viewer-nav prev" onClick={() => go(-1)} aria-label={t('Previous')}>
              <IBack size={22} />
            </button>
            <button class="viewer-nav next" onClick={() => go(1)} aria-label={t('Next')}>
              <IChevron size={22} />
            </button>
          </>
        )}
      </div>
      {view === 'html' && (
        <div class={cls('viewer-note', js && 'on')}>
          {js ? t('Scripts are running — sandboxed, offline and without access to Collie.') : t('Scripts are off and nothing is loaded from the network.')}
        </div>
      )}
    </div>
  )
}
