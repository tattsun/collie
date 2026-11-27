import { signal } from '@preact/signals'
import { memo } from 'preact/compat'
import { useMemo } from 'preact/hooks'
import type { Session } from '../types'
import { attention, conn, go, groupById, homeProject, me, notifyEnabled, projectById, projects, route, sessionById, sessions, setNotify, userName, version } from '../store'
import { ago, cls, home } from '../util'
import { IBell, IBox, IFolder, IHome, IPlus, IBranch, IUsers, Spinner } from './icons'
import { AgentBadge, Menu, StatusDot } from './ui'
import { signOut } from '../authflow'
import { ImageIndicator } from './ProjectSettings'
import { UsagePanel } from './Usage'
import { t, ts } from '../i18n'

export const listTab = signal<'sessions' | 'projects'>((localStorage.getItem('collie.tab') as any) || 'sessions')
export const projectFilter = signal<string>(localStorage.getItem('collie.filter') || '')
const showArchived = signal(false)

// Busy: its agent is working, in a turn or in the background.
const busy = (s: Session) => s.status === 'running' || s.status === 'starting' || !!s.tasks?.length
const rank = (s: Session) => (s.status === 'waiting' ? 0 : s.status === 'error' ? 1 : busy(s) ? 2 : 3)

const SessionRow = memo(function SessionRow({ s, active, project }: { s: Session; active: boolean; project?: string }) {
  return (
    <a href={`#/s/${s.id}`} class={cls('srow', active && 'active', s.unread && 'unread', s.archived && 'archived')}>
      <StatusDot status={s.status} unread={s.unread} bg={!!s.tasks?.length} />
      <div class="srow-main">
        <div class="srow-top">
          <span class="srow-title">{s.title}</span>
          <span class="srow-time">{ago(s.updatedAt)}</span>
        </div>
        <div class="srow-meta">
          <AgentBadge agent={s.agent} small />
          {project && <span class="srow-proj">{project}</span>}
          {s.owner && s.owner !== me.value?.id && (
            <span class="srow-by">
              <IUsers size={11} /> {userName(s.owner)}
            </span>
          )}
          {s.worktree && s.branch && (
            <span class="srow-branch">
              <IBranch size={11} />
              {s.branch.replace(/^collie\//, '')}
            </span>
          )}
          {!!s.tasks?.length && (
            <span class="srow-bg" title={s.tasks.map((x) => x.title).join('\n')}>
              {t('{n} in background', { n: s.tasks.length })}
            </span>
          )}
        </div>
        {s.status === 'waiting' ? (
          <div class="srow-preview warn">{t('Needs your input')}</div>
        ) : s.preview ? (
          <div class="srow-preview">{s.preview}</div>
        ) : null}
      </div>
    </a>
  )
})

function SessionList() {
  const r = route.value
  const pmap = projectById.value
  const filter = projectFilter.value
  const { needs, rest, archivedCount } = useMemo(() => {
    let list = sessions.value
    if (filter) list = list.filter((s) => s.projectId === filter)
    const archivedCount = list.filter((s) => s.archived).length
    if (!showArchived.value) list = list.filter((s) => !s.archived)
    const sorted = [...list].sort((a, b) => rank(a) - rank(b) || b.updatedAt - a.updatedAt)
    return {
      needs: sorted.filter((s) => s.status === 'waiting' || s.status === 'error'),
      rest: sorted.filter((s) => s.status !== 'waiting' && s.status !== 'error'),
      archivedCount,
    }
  }, [sessions.value, filter, showArchived.value])
  const activeId = r.name === 'session' ? r.id : undefined

  return (
    <div class="slist">
      {projects.value.length > 1 && (
        <select
          class="select filter"
          value={filter}
          onChange={(e) => {
            projectFilter.value = (e.target as HTMLSelectElement).value
            localStorage.setItem('collie.filter', projectFilter.value)
          }}
        >
          <option value="">{t('All projects')}</option>
          {projects.value.map((p) => (
            <option key={p.id} value={p.id}>
              {p.name}
            </option>
          ))}
        </select>
      )}
      {needs.length > 0 && <div class="group-h warn">{t('Needs you · {n}', { n: needs.length })}</div>}
      {needs.map((s) => (
        <SessionRow key={s.id} s={s} active={s.id === activeId} project={pmap.get(s.projectId)?.name} />
      ))}
      {needs.length > 0 && rest.length > 0 && <div class="group-h">{t('Sessions')}</div>}
      {rest.map((s) => (
        <SessionRow key={s.id} s={s} active={s.id === activeId} project={pmap.get(s.projectId)?.name} />
      ))}
      {needs.length + rest.length === 0 && (
        <div class="empty dim">
          {projects.value.length === 0 ? t('Create a project to get started.') : t('No sessions yet.')}
          <button class="btn primary" onClick={() => go(projects.value.length ? '#/new' : '#/projects?new=1')}>
            <IPlus size={15} /> {projects.value.length ? t('New session') : t('New project')}
          </button>
        </div>
      )}
      {archivedCount > 0 && (
        <button class="btn ghost small archived-toggle" onClick={() => (showArchived.value = !showArchived.value)}>
          {showArchived.value ? t('Hide archived ({n})', { n: archivedCount }) : t('Show archived ({n})', { n: archivedCount })}
        </button>
      )}
    </div>
  )
}

function ProjectList() {
  const r = route.value
  const sections = useMemo(() => {
    // Labels are translated at render time (this memo doesn't depend on the language).
    const out: { key: string; label: string; items: typeof projects.value }[] = []
    const mine = projects.value.filter((p) => !p.ownerGroup)
    if (mine.length) out.push({ key: 'me', label: '', items: mine })
    const byGroup = new Map<string, typeof projects.value>()
    for (const p of projects.value) {
      if (!p.ownerGroup) continue
      if (!byGroup.has(p.ownerGroup)) byGroup.set(p.ownerGroup, [])
      byGroup.get(p.ownerGroup)!.push(p)
    }
    for (const [gid, items] of byGroup) out.push({ key: gid, label: groupById.value.get(gid)?.name || '', items })
    return out
  }, [projects.value, groupById.value])
  const counts = useMemo(() => {
    const m = new Map<string, { n: number; busy: number }>()
    for (const s of sessions.value) {
      if (s.archived) continue
      const c = m.get(s.projectId) || { n: 0, busy: 0 }
      c.n++
      if (busy(s) || s.status === 'waiting') c.busy++
      m.set(s.projectId, c)
    }
    return m
  }, [sessions.value])
  return (
    <div class="slist">
      {sections.map((sec) => (
        <div key={sec.key} class="slist">
          {sections.length > 1 && (
            <div class="group-h">
              {sec.key !== 'me' && <IUsers size={11} />} {sec.key === 'me' ? t('Personal') : sec.label || t('Group')}
            </div>
          )}
          {sec.items.map((p) => {
        const c = counts.get(p.id)
        return (
          <a key={p.id} href={`#/p/${p.id}`} class={cls('prow', r.name === 'project' && r.id === p.id && 'active')}>
            {p.runtime === 'container' ? <IBox size={16} /> : <IFolder size={16} />}
            <div class="srow-main">
              <div class="srow-top">
                <span class="srow-title">{p.name}</span>
                {p.status === 'cloning' && <Spinner size={12} />}
                {c && <span class="srow-time">{c.busy ? t('{busy} active · {n}', { busy: c.busy, n: c.n }) : c.n}</span>}
              </div>
              <div class="srow-preview mono">{p.status === 'error' ? ts(p.error || '') : home(p.path)}</div>
            </div>
          </a>
        )
      })}
        </div>
      ))}
      <button class="btn ghost new-proj" onClick={() => go('#/projects?new=1')}>
        <IPlus size={15} /> {t('New project')}
      </button>
    </div>
  )
}

// Home: chats outside any project, whose agent coordinates the projects.
function HomeRow() {
  const h = homeProject.value
  const r = route.value
  if (!h) return null
  const mine = sessions.value.filter((s) => s.projectId === h.id && !s.archived)
  const waiting = mine.some((s) => s.status === 'waiting')
  const working = mine.some(busy)
  const active = (r.name === 'project' && r.id === h.id) || (r.name === 'session' && sessionById.value.get(r.id || '')?.projectId === h.id)
  return (
    <a href={`#/p/${h.id}`} class={cls('prow home-row', active && 'active')}>
      <IHome size={16} />
      <div class="srow-main">
        <div class="srow-top">
          <span class="srow-title">{t('Home')}</span>
          {waiting ? <StatusDot status="waiting" /> : working ? <Spinner size={12} /> : null}
        </div>
        <div class="srow-preview">{t('Talk about anything · coordinate your projects')}</div>
      </div>
    </a>
  )
}

export function Sidebar() {
  const tab = listTab.value
  const setTab = (t: 'sessions' | 'projects') => {
    listTab.value = t
    localStorage.setItem('collie.tab', t)
  }
  const n = attention.value.filter((s) => s.status === 'waiting').length
  return (
    <aside class="sidebar">
      <header class="side-head">
        <a href="#/" class="brand">
          <img src="/icon.svg" width={22} height={22} alt="" />
          <span>Collie</span>
        </a>
        <span class={cls('conn', conn.value)} title={conn.value === 'online' ? t('online') : conn.value === 'offline' ? t('offline') : t('connecting')} />
        <span class="grow" />
        <button class="btn primary small" onClick={() => go('#/new')} disabled={projects.value.length === 0}>
          <IPlus size={15} /> {t('New')}
        </button>
      </header>
      <div class="side-tabs">
        <button class={cls('side-tab', tab === 'sessions' && 'on')} onClick={() => setTab('sessions')}>
          {t('Sessions')}{n > 0 && <span class="badge">{n}</span>}
        </button>
        <button class={cls('side-tab', tab === 'projects' && 'on')} onClick={() => setTab('projects')}>
          {t('Projects')}
        </button>
      </div>
      <div class="side-scroll">
        <HomeRow />
        {tab === 'sessions' ? <SessionList /> : <ProjectList />}
      </div>
      <UsagePanel />
      <footer class="side-foot">
        <Menu
          up
          label={t('Account menu')}
          trigger={
            <span class="avatar" title={me.value?.displayName}>
              {(me.value?.displayName || '?').slice(0, 1).toUpperCase()}
            </span>
          }
          items={[
            { label: t('{name} — account', { name: me.value?.displayName || '' }), onClick: () => go('#/account') },
            { label: t('Groups'), onClick: () => go('#/groups') },
            ...(me.value?.admin ? [{ label: t('Admin'), onClick: () => go('#/admin') }] : []),
            { label: t('Sign out'), onClick: signOut, danger: true },
          ]}
        />
        <button class={cls('foot-btn', notifyEnabled.value && 'on')} onClick={() => setNotify(!notifyEnabled.value)} title={t('Notifications')}>
          <IBell size={14} /> {notifyEnabled.value ? t('Notifications on') : t('Notifications off')}
        </button>
        <ImageIndicator />
        <span class="grow" />
        <span class="dim small">v{version.value}</span>
      </footer>
    </aside>
  )
}
