import { useEffect, useRef, useState } from 'preact/hooks'
import { api } from '../api'
import { t, ts } from '../i18n'
import { toast, toastError } from '../store'
import type { CatalogPlugin, EffectivePlugin, MarketplaceSource, PluginLevel, PluginSet } from '../types'
import { cls } from '../util'
import { IPlus, ITrash, Spinner } from './icons'

// Claude Code plugins: each level (global, group, personal, project) is a
// PluginSet edited here; the server installs the merged result into the
// containers before Claude starts.

const BUILTIN = 'anthropic-plugin-directory'

type FullSet = { enabledPlugins: Record<string, boolean>; extraKnownMarketplaces: Record<string, { source: MarketplaceSource }> }

const full = (s: PluginSet | undefined): FullSet => ({ enabledPlugins: { ...(s?.enabledPlugins || {}) }, extraKnownMarketplaces: { ...(s?.extraKnownMarketplaces || {}) } })

/** "owner/repo" → GitHub; an http(s) URL ending in .json → that file; other URLs → git. */
function parseSource(s: string): MarketplaceSource | null {
  s = s.trim()
  if (/^[A-Za-z0-9][\w.-]*\/[\w.-]+$/.test(s)) return { source: 'github', repo: s }
  if (/^https?:\/\/\S+\.json$/.test(s)) return { source: 'url', url: s }
  if (/^(https?|ssh|git):\/\/\S+$/.test(s) || /^[\w.-]+@[\w.-]+:\S+$/.test(s)) return { source: 'git', url: s }
  return null
}

const showSource = (src: MarketplaceSource) => src.repo || src.url || src.path || src.source

function splitID(id: string): [string, string] {
  const i = id.lastIndexOf('@')
  return i < 0 ? [id, ''] : [id.slice(0, i), id.slice(i + 1)]
}

function PluginName({ id }: { id: string }) {
  const [name, mkt] = splitID(id)
  return (
    <span class="grow plugin-id">
      <span class="b">{name}</span>
      {mkt !== BUILTIN && <span class="dim small mono"> @{mkt}</span>}
    </span>
  )
}

/** Search over Anthropic's plugin directory; Enter adds what was typed. */
function PluginPicker({ onPick }: { onPick: (id: string) => void }) {
  const [q, setQ] = useState('')
  const [open, setOpen] = useState(false)
  const [list, setList] = useState<CatalogPlugin[] | null>(null)
  const [err, setErr] = useState('')
  const [loading, setLoading] = useState(false)
  const seq = useRef(0)
  useEffect(() => {
    if (!open) return
    const n = ++seq.current
    const timer = setTimeout(async () => {
      setLoading(true)
      try {
        const r = await api<CatalogPlugin[]>('GET', `/api/plugins/catalog?q=${encodeURIComponent(q.trim())}`)
        if (n === seq.current) {
          setList(r)
          setErr('')
        }
      } catch (e) {
        if (n === seq.current) setErr((e as Error).message)
      } finally {
        if (n === seq.current) setLoading(false)
      }
    }, 200)
    return () => clearTimeout(timer)
  }, [q, open])
  const pick = (id: string) => {
    onPick(id)
    setQ('')
    setOpen(false)
  }
  const addTyped = () => {
    const v = q.trim()
    if (v) pick(v.includes('@') ? v : `${v}@${BUILTIN}`)
  }
  return (
    <div class="plugin-picker">
      <div class="row gap">
        <input
          class="input"
          value={q}
          placeholder={t('Search the plugin directory, or type name@marketplace')}
          onFocus={() => setOpen(true)}
          onBlur={() => setTimeout(() => setOpen(false), 150)}
          onInput={(e) => {
            setQ((e.target as HTMLInputElement).value)
            setOpen(true)
          }}
          onKeyDown={(e) => {
            if (e.key === 'Enter') {
              e.preventDefault()
              addTyped()
            } else if (e.key === 'Escape') setOpen(false)
          }}
        />
        <button class="btn" disabled={!q.trim()} onClick={addTyped}>
          <IPlus size={14} /> {t('Add')}
        </button>
      </div>
      {open && (
        <div class="plugin-results">
          {loading && !list && (
            <div class="dim small pad-s">
              <Spinner size={12} /> {t('Loading the plugin directory…')}
            </div>
          )}
          {err && <div class="warn small pad-s">{ts(err)}</div>}
          {list?.map((c) => (
            <button key={c.id} class="plugin-result" onMouseDown={(e) => e.preventDefault()} onClick={() => pick(c.id)}>
              <PluginName id={c.id} />
              {c.description && <span class="dim small">{c.description}</span>}
            </button>
          ))}
          {list && list.length === 0 && q.trim() && <div class="dim small pad-s">{t('No match. Press Enter to add “{q}” as typed.', { q: q.trim() })}</div>}
        </div>
      )}
    </div>
  )
}

/** Edits one level's plugins: GET {url} → {plugins, canEdit}, PUT {plugins}. */
export function PluginsEditor({ url, readOnlyNote, onSaved }: { url: string; readOnlyNote?: string; onSaved?: () => void }) {
  const [set, setSet] = useState<FullSet | null>(null)
  const [orig, setOrig] = useState('')
  const [canEdit, setCanEdit] = useState(false)
  const [busy, setBusy] = useState(false)
  const [mkName, setMkName] = useState('')
  const [mkSrc, setMkSrc] = useState('')
  useEffect(() => {
    setSet(null)
    api<{ plugins: PluginSet; canEdit: boolean }>('GET', url)
      .then((r) => {
        const s = full(r.plugins)
        setSet(s)
        setOrig(JSON.stringify(s))
        setCanEdit(r.canEdit)
      })
      .catch(toastError)
  }, [url])
  if (!set) return <Spinner />

  const update = (f: (s: FullSet) => void) => {
    const s = full(set)
    f(s)
    setSet(s)
  }
  const save = async () => {
    setBusy(true)
    try {
      await api('PUT', url, { plugins: set })
      setOrig(JSON.stringify(set))
      toast(t('Saved — applies the next time an agent starts'))
      onSaved?.()
    } catch (e) {
      toastError(e)
    } finally {
      setBusy(false)
    }
  }
  const addMarketplace = () => {
    const name = mkName.trim()
    const src = parseSource(mkSrc)
    if (!/^[A-Za-z0-9][\w.-]*$/.test(name) || name === BUILTIN) return toastError(new Error(t('Marketplace name: the name in its marketplace.json (a-z, 0-9, ., -, _)')))
    if (!src) return toastError(new Error(t('Source: owner/repo on GitHub, or a git / https URL')))
    update((s) => {
      s.extraKnownMarketplaces[name] = { source: src }
    })
    setMkName('')
    setMkSrc('')
  }
  const ids = Object.keys(set.enabledPlugins).sort()
  const mkts = Object.keys(set.extraKnownMarketplaces).sort()
  const dirty = JSON.stringify(set) !== orig

  return (
    <div class="col gap">
      <div>
        {ids.length === 0 && <div class="dim small">{t('No plugins here yet.')}</div>}
        {ids.map((id) => {
          const on = set.enabledPlugins[id]
          return (
            <div key={id} class={cls('row gap list-row', !on && 'muted')}>
              <PluginName id={id} />
              {canEdit ? (
                <>
                  <select
                    class="select tiny"
                    value={on ? 'on' : 'off'}
                    onChange={(e) =>
                      update((s) => {
                        s.enabledPlugins[id] = (e.target as HTMLSelectElement).value === 'on'
                      })
                    }
                  >
                    <option value="on">{t('Install')}</option>
                    <option value="off">{t('Turn off')}</option>
                  </select>
                  <button
                    class="icon-btn tiny"
                    aria-label={t('Remove')}
                    onClick={() =>
                      update((s) => {
                        delete s.enabledPlugins[id]
                      })
                    }
                  >
                    <ITrash size={13} />
                  </button>
                </>
              ) : (
                <span class="dim small">{on ? t('Install') : t('Turn off')}</span>
              )}
            </div>
          )
        })}
      </div>
      {canEdit && (
        <PluginPicker
          onPick={(id) =>
            update((s) => {
              s.enabledPlugins[id] = true
            })
          }
        />
      )}
      <p class="dim small">{t('“Turn off” stops a plugin turned on at a broader level from being installed here.')}</p>
      {(canEdit || mkts.length > 0) && (
        <details class="plugin-mkts" open={mkts.length > 0}>
          <summary class="small">{t('Other marketplaces ({n})', { n: mkts.length })}</summary>
          <p class="dim small">{t('Anthropic’s directory is always available. Add a marketplace to install plugins from it as name@marketplace.')}</p>
          {mkts.map((name) => (
            <div key={name} class="row gap list-row">
              <span class="b mono small">{name}</span>
              <span class="grow dim small mono plugin-id">{showSource(set.extraKnownMarketplaces[name].source)}</span>
              {canEdit && (
                <button
                  class="icon-btn tiny"
                  aria-label={t('Remove')}
                  onClick={() =>
                    update((s) => {
                      delete s.extraKnownMarketplaces[name]
                    })
                  }
                >
                  <ITrash size={13} />
                </button>
              )}
            </div>
          ))}
          {canEdit && (
            <div class="row gap wrap mkt-add">
              <input class="input mono" value={mkName} placeholder={t('name')} onInput={(e) => setMkName((e.target as HTMLInputElement).value)} />
              <input class="input mono grow" value={mkSrc} placeholder="owner/repo · https://…" onInput={(e) => setMkSrc((e.target as HTMLInputElement).value)} />
              <button class="btn" disabled={!mkName.trim() || !mkSrc.trim()} onClick={addMarketplace}>
                <IPlus size={14} /> {t('Add')}
              </button>
            </div>
          )}
        </details>
      )}
      {canEdit ? (
        <div class="modal-actions">
          <button class="btn primary" disabled={!dirty || busy} onClick={save}>
            {busy && <Spinner size={12} />} {t('Save')}
          </button>
        </div>
      ) : (
        readOnlyNote && <p class="dim small">{readOnlyNote}</p>
      )}
    </div>
  )
}

// Labels stay in English here and are translated where rendered.
const LEVEL: Record<PluginLevel, string> = {
  global: 'Global',
  group: 'Group',
  personal: 'Personal',
  project: 'Project',
  repo: 'Repository',
}

const PROBLEM_LEVEL: Record<string, string> = { global: 'Global', group: 'Group', personal: 'Personal', project: 'Project', repository: 'Repository' }

/** "<level> plugins: <message>" from the server, translated part by part. */
function problem(x: string) {
  const m = /^(\w+) plugins: ([\s\S]*)$/.exec(x)
  return m && PROBLEM_LEVEL[m[1]] ? `${t(PROBLEM_LEVEL[m[1]])}: ${ts(m[2])}` : ts(x)
}

/** The merged plugins a user's agents get in a project, with where each comes from. */
export function EffectivePlugins({ projectId, reload }: { projectId: string; reload?: number }) {
  const [d, setD] = useState<{ plugins: EffectivePlugin[]; problems: string[] } | null>(null)
  useEffect(() => {
    api<{ plugins: EffectivePlugin[]; problems: string[] }>('GET', `/api/projects/${projectId}/plugins/effective`)
      .then(setD)
      .catch(() => setD({ plugins: [], problems: [] }))
  }, [projectId, reload])
  if (!d) return <Spinner />
  return (
    <div>
      {d.plugins.length === 0 && <div class="dim small">{t('No plugins.')}</div>}
      {d.plugins.map((p) => (
        <div key={p.id} class={cls('row gap list-row', !p.enabled && 'muted')}>
          <PluginName id={p.id} />
          {!p.enabled && <span class="dim small">{t('off')}</span>}
          <span class="chip">{t(LEVEL[p.from] || p.from)}</span>
        </div>
      ))}
      {d.problems.map((x, i) => (
        <div key={i} class="warn small">
          {problem(x)}
        </div>
      ))}
    </div>
  )
}
