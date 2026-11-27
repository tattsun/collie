import { lazy, Suspense } from 'preact/compat'
import { useEffect, useMemo, useState } from 'preact/hooks'
import type { Project } from '../types'
import { api } from '../api'
import { go, groupById, groups, me, ownerLabel, projectById, projects, root, rootBase, route, sandbox, sessions, toastError } from '../store'
import { ago, cls, home } from '../util'
import { ChangesView } from './Changes'
import { AgentBadge, Segmented, StatusDot, Toggle } from './ui'
import { IBack, IBox, IClock, IDiff, IFolder, IHome, IList, IPlus, ISettings, ITerminal, ITrash, Spinner } from './icons'
import { ProjectSettings } from './ProjectSettings'
import { t, ts } from '../i18n'

const TerminalView = lazy(() => import('./Terminal').then((m) => ({ default: m.TerminalView })))

type Kind = 'empty' | 'clone' | 'existing'

export function NewProjectForm({ onDone }: { onDone?: (p: Project) => void }) {
  const [kind, setKind] = useState<Kind>('empty')
  const admin = !!me.value?.admin
  const myGroups = groups.value.filter((g) => g.myRole)
  const [group, setGroup] = useState(() => route.value.query.get('group') || '')
  const base = group ? `${rootBase.value}/groups/${groupById.value.get(group)?.name || ''}` : root.value
  const [name, setName] = useState('')
  const [url, setUrl] = useState('')
  const [path, setPath] = useState('')
  const [git, setGit] = useState(true)
  const [isolated, setIsolated] = useState(sandbox.value.available)
  const [busy, setBusy] = useState(false)
  const [dirs, setDirs] = useState<string[]>([])

  useEffect(() => {
    if (kind === 'existing') api<{ dirs: string[] }>('GET', '/api/dirs').then((d) => setDirs(d.dirs)).catch(() => {})
  }, [kind])

  const submit = async (e: Event) => {
    e.preventDefault()
    setBusy(true)
    try {
      const p = await api<Project>('POST', '/api/projects', { kind, name, url, path, git, group, runtime: isolated || !admin ? 'container' : 'host' })
      setName('')
      setUrl('')
      setPath('')
      onDone?.(p)
    } catch (err) {
      toastError(err)
    } finally {
      setBusy(false)
    }
  }

  return (
    <form class="card form" onSubmit={submit}>
      <Segmented<Kind>
        value={kind}
        onChange={setKind}
        options={[
          { value: 'empty', label: t('New folder') },
          { value: 'clone', label: t('Clone repo') },
          ...(admin ? [{ value: 'existing' as Kind, label: t('Existing folder') }] : []),
        ]}
      />
      {kind === 'clone' && (
        <label class="field">
          <span class="field-l">{t('Repository URL')}</span>
          <input class="input mono" value={url} placeholder="git@github.com:you/repo.git" autoCapitalize="off" autoCorrect="off" spellcheck={false} onInput={(e) => setUrl((e.target as HTMLInputElement).value)} />
          <span class="field-h">{admin ? t("Cloned with the server user's git credentials.") : t('https:// only; private GitHub repositories use the GH_TOKEN in your account environment.')}</span>
        </label>
      )}
      {kind === 'existing' ? (
        <label class="field">
          <span class="field-l">{t('Path on the server')}</span>
          <input class="input mono" value={path} list="dir-suggest" placeholder={t('{home}/my-app or /srv/code/app', { home: home(root.value) })} autoCapitalize="off" autoCorrect="off" spellcheck={false} onInput={(e) => setPath((e.target as HTMLInputElement).value)} />
          <datalist id="dir-suggest">
            {dirs.map((d) => (
              <option key={d} value={d} />
            ))}
          </datalist>
          {dirs.length > 0 && (
            <div class="suggest">
              {dirs.slice(0, 12).map((d) => (
                <button type="button" key={d} class="chip" onClick={() => setPath(d)}>
                  {d.split('/').pop()}
                </button>
              ))}
            </div>
          )}
        </label>
      ) : (
        <label class="field">
          <span class="field-l">{t('Name')} {kind === 'clone' && <span class="dim">{t('(optional)')}</span>}</span>
          <input class="input" value={name} placeholder={kind === 'clone' ? t('derived from the URL') : 'my-app'} autoCapitalize="off" autoCorrect="off" spellcheck={false} onInput={(e) => setName((e.target as HTMLInputElement).value)} />
          <span class="field-h mono">
            {home(base)}/{name || (kind === 'clone' ? '<repo>' : '<name>')}
          </span>
        </label>
      )}
      {kind === 'empty' && <Toggle checked={git} onChange={setGit} label={t('Initialize a git repository')} />}
      {myGroups.length > 0 && (
        <label class="field">
          <span class="field-l">{t('Owner')}</span>
          <select class="select" value={group} onChange={(e) => setGroup((e.target as HTMLSelectElement).value)}>
            <option value="">{t('Me (personal)')}</option>
            {myGroups.map((g) => (
              <option key={g.id} value={g.id}>
                {t('Group: {name}', { name: g.name })}
              </option>
            ))}
          </select>
          {group && <span class="field-h">{t('Shared with every member of the group.')}</span>}
        </label>
      )}
      {admin && <Toggle
        checked={isolated && sandbox.value.available}
        disabled={!sandbox.value.available}
        onChange={setIsolated}
        label={t('Run agents in an isolated container (recommended)')}
        hint={
          sandbox.value.available
            ? t('Its own sandbox with resource limits, only this folder visible, no LAN/tailnet access.')
            : t('Needs podman on the server.')
        }
      />}
      <div class="modal-actions">
        <button class="btn primary" type="submit" disabled={busy}>
          {busy && <Spinner size={12} />}
          {kind === 'clone' ? t('Clone') : kind === 'existing' ? t('Add project') : t('Create project')}
        </button>
      </div>
    </form>
  )
}

export function ProjectsPage() {
  const r = route.value
  const [showNew, setShowNew] = useState(r.query.get('new') === '1' || projects.value.length === 0)
  useEffect(() => {
    if (r.query.get('new') === '1') setShowNew(true)
  }, [r.query.get('new')])
  return (
    <div class="page page-pad narrow">
      <div class="page-head">
        <button class="icon-btn back" onClick={() => go('#/')} aria-label={t('Back')}>
          <IBack size={20} />
        </button>
        <h1>{t('Projects')}</h1>
        <span class="grow" />
        {!showNew && (
          <button class="btn primary small" onClick={() => setShowNew(true)}>
            <IPlus size={15} /> {t('New project')}
          </button>
        )}
      </div>
      {showNew && <NewProjectForm onDone={(p) => go(`#/new?project=${p.id}`)} />}
      <div class="plist">
        {projects.value.map((p) => (
          <a key={p.id} class="card prow-card" href={`#/p/${p.id}`}>
            <IFolder size={18} />
            <div class="grow">
              <div class="b">{p.name}</div>
              <div class="dim small mono">{home(p.path)}</div>
              {p.status === 'error' && <div class="err small">{ts(p.error || '')}</div>}
            </div>
            {p.runtime === 'container' && (
              <span class="chip" title={t('Isolated container')}>
                <IBox size={12} />
              </span>
            )}
            {p.status === 'cloning' ? <Spinner /> : p.git ? <span class="chip">git</span> : null}
          </a>
        ))}
      </div>
    </div>
  )
}

export function ProjectPage({ id }: { id: string }) {
  const p = projectById.value.get(id)
  const r = route.value
  const tab = r.tab || 'sessions'
  const list = useMemo(() => sessions.value.filter((s) => s.projectId === id).sort((a, b) => b.updatedAt - a.updatedAt), [sessions.value, id])
  if (!p) {
    return (
      <div class="page center-page dim">
        <p>{t('Project not found.')}</p>
        <button class="btn" onClick={() => go('#/projects')}>
          {t('All projects')}
        </button>
      </div>
    )
  }
  const remove = async () => {
    if (!confirm(t('Remove project “{name}” from Collie?\n\nIts {n} session(s) are deleted. Files in {path} are NOT touched.', { name: p.name, n: list.length, path: p.path }))) return
    try {
      await api('DELETE', `/api/projects/${id}`)
      go('#/projects')
    } catch (e) {
      toastError(e)
    }
  }
  return (
    <div class="session">
      <header class="sess-head">
        <button class="icon-btn back" onClick={() => go(p.home ? '#/' : '#/projects')} aria-label={t('Back')}>
          <IBack size={20} />
        </button>
        {p.home ? (
          <div class="sess-title-wrap">
            <div class="sess-title static">
              <IHome size={16} /> {t('Home')}
            </div>
            <div class="sess-sub">
              <span class="sub-item">{t('Talk about anything · coordinate your projects')}</span>
            </div>
          </div>
        ) : (
        <div class="sess-title-wrap">
          <div class="sess-title static">{p.name}</div>
          <div class="sess-sub">
            <span class={cls('sub-item', p.runtime === 'container' && 'iso')}>
              {p.runtime === 'container' ? (
                <>
                  <IBox size={11} /> {t('isolated')}
                </>
              ) : (
                t('host')
              )}
            </span>
            {ownerLabel(p) && <span class="sub-item">{ownerLabel(p)}</span>}
            <span class="sub-item mono">{home(p.path)}</span>
            {p.status === 'cloning' && (
              <span class="sub-item">
                <Spinner size={10} /> {t('cloning')}
              </span>
            )}
          </div>
        </div>
        )}
        <button class="btn primary small" onClick={() => go(`#/new?project=${id}`)} disabled={p.status === 'cloning'}>
          <IPlus size={15} /> {p.home ? t('New chat') : t('Session')}
        </button>
      </header>
      <nav class="tabs">
        <a href={`#/p/${id}`} class={cls('tab', tab === 'sessions' && 'on')}>
          <IList size={14} /> {t('Sessions')}
        </a>
        {!p.home && (
          <a href={`#/p/${id}/changes`} class={cls('tab', tab === 'changes' && 'on')}>
            <IDiff size={14} /> {t('Changes')}
          </a>
        )}
        <a href={`#/p/${id}/term`} class={cls('tab', tab === 'term' && 'on')}>
          <ITerminal size={14} /> {t('Terminal')}
        </a>
        {!p.home && (
          <a href={`#/p/${id}/settings`} class={cls('tab', tab === 'settings' && 'on')}>
            <ISettings size={14} /> {t('Settings')}
          </a>
        )}
      </nav>
      {tab === 'sessions' && (
        <div class="tab-body page-pad">
          {p.status === 'error' && <div class="error-box">{ts(p.error || '')}</div>}
          {p.home && list.length === 0 && (
            <div class="card home-intro">
              <p>{t('Your Home agent is for anything that is not tied to one project:')}</p>
              <ul>
                <li>{t('everyday questions and consultations')}</li>
                <li>{t('“What happened in my projects this week?” — it reads your sessions and code')}</li>
                <li>{t('“Have sample2 fix its tests” — it starts or instructs sessions in your projects')}</li>
              </ul>
              <p class="dim small">{t('It reads your projects but does not edit them; the sessions it starts do, and their approvals still come to you.')}</p>
            </div>
          )}
          {!p.home && list.length === 0 && <div class="dim pad">{t('No sessions in this project yet.')}</div>}
          <div class="plist">
            {list.map((s) => (
              <a key={s.id} href={`#/s/${s.id}`} class={cls('card prow-card', s.archived && 'archived')}>
                <StatusDot status={s.status} unread={s.unread} />
                <div class="grow minw0">
                  <div class="b ellipsis">{s.title}</div>
                  <div class="dim small ellipsis">{s.preview || s.branch}</div>
                </div>
                {s.schedule && (
                  <span class="dim" title={t('Started by a schedule')}>
                    <IClock size={13} />
                  </span>
                )}
                <AgentBadge agent={s.agent} small />
                <span class="dim small">{ago(s.updatedAt)}</span>
              </a>
            ))}
          </div>
          {!p.home && (
            <div class="danger-zone">
              <button class="btn ghost danger small" onClick={remove}>
                <ITrash size={14} /> {t('Remove project')}
              </button>
            </div>
          )}
        </div>
      )}
      {tab === 'changes' && !p.home && (
        <div class="tab-body">
          <ChangesView url={`/api/projects/${id}/changes`} />
        </div>
      )}
      {tab === 'term' && (
        <Suspense fallback={<div class="center pad"><Spinner /></div>}>
          <TerminalView owner={id} projectId={id} />
        </Suspense>
      )}
      {tab === 'settings' && !p.home && (
        <div class="tab-body">
          <ProjectSettings p={p} />
        </div>
      )}
    </div>
  )
}
