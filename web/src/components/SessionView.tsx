import { lazy, Suspense } from 'preact/compat'
import { useEffect, useState } from 'preact/hooks'
import type { Session } from '../types'
import { api } from '../api'
import { go, groupById, homeProject, markSeen, me, projectById, route, scheduleById, sessionById, sessions, subscribe, timeline, toastError, agentCaps, userName } from '../store'
import { Timeline } from './Timeline'
import { Composer } from './Composer'
import { TasksBar } from './Tasks'
import { ChangesView } from './Changes'
import { PortsView } from './Ports'
import { AgentBadge, Menu, Modal, StatusPill } from './ui'
import { SessionMeter } from './Usage'
import { PendingBanner } from './Pending'
import { IBack, IBell, IBox, IBranch, IChat, IClock, IDiff, IGlobe, IMore, ITerminal, IUsers, Spinner } from './icons'
import { cls, home, money, shortMode } from '../util'
import { t } from '../i18n'

const TerminalView = lazy(() => import('./Terminal').then((m) => ({ default: m.TerminalView })))

function useVisible() {
  const [v, setV] = useState(!document.hidden)
  useEffect(() => {
    const on = () => setV(!document.hidden)
    document.addEventListener('visibilitychange', on)
    return () => document.removeEventListener('visibilitychange', on)
  }, [])
  return v
}

const sharedTitle = (group?: string) => (group ? t('Shared with {name}', { name: group }) : t('Shared with the group'))
const shareLabel = (group?: string) => (group ? t('Share with {name}', { name: group }) : t('Share with group'))

function SettingsModal({ s, onClose }: { s: Session; onClose: () => void }) {
  const [title, setTitle] = useState(s.title)
  const [model, setModel] = useState(s.model || '')
  const caps = timeline(s.id).caps.value
  const models = caps.models?.length ? caps.models : agentCaps.value[s.agent]?.models || []
  const save = async () => {
    try {
      await api('PATCH', `/api/sessions/${s.id}`, { title, model: model === s.model ? undefined : model })
      onClose()
    } catch (e) {
      toastError(e)
    }
  }
  return (
    <Modal title={t('Session settings')} onClose={onClose}>
      <label class="field">
        <span class="field-l">{t('Title')}</span>
        <input class="input" value={title} onInput={(e) => setTitle((e.target as HTMLInputElement).value)} />
      </label>
      <label class="field">
        <span class="field-l">{t('Model')}</span>
        <input class="input" list="model-list" value={model} placeholder={t('default')} onInput={(e) => setModel((e.target as HTMLInputElement).value)} />
        <datalist id="model-list">
          {models.map((m) => (
            <option key={m.id} value={m.id}>
              {m.label}
            </option>
          ))}
        </datalist>
        <span class="field-h">{t('Takes effect on the next message.')}</span>
      </label>
      <div class="kv">
        <span>{t('Directory')}</span>
        <span class="mono">{home(s.cwd)}</span>
        {s.branch && (
          <>
            <span>{t('Branch')}</span>
            <span class="mono">{s.branch}</span>
          </>
        )}
        {s.agentRef && (
          <>
            <span>{s.agent === 'claude' ? t('Claude session') : t('Codex thread')}</span>
            <span class="mono small">{s.agentRef}</span>
          </>
        )}
        {!!s.costUsd && (
          <>
            <span>{t('Cost')}</span>
            <span>{money(s.costUsd)}</span>
          </>
        )}
      </div>
      <div class="modal-actions">
        <button class="btn" onClick={onClose}>
          {t('Cancel')}
        </button>
        <button class="btn primary" onClick={save}>
          {t('Save')}
        </button>
      </div>
    </Modal>
  )
}

export function SessionView({ id }: { id: string }) {
  const s = sessionById.value.get(id)
  const r = route.value
  const tab = r.tab || 'chat'
  const visible = useVisible()
  const [settings, setSettings] = useState(false)

  useEffect(() => {
    subscribe(id)
    return () => subscribe(null)
  }, [id])

  useEffect(() => {
    if (s?.unread && visible) markSeen(id)
  }, [s?.unread, visible, id])

  if (!s) {
    return (
      <div class="page center-page dim">
        <p>{t('This session no longer exists.')}</p>
        <button class="btn" onClick={() => go('#/')}>
          {t('Back')}
        </button>
      </div>
    )
  }
  const project = projectById.value.get(s.projectId)
  const caps = timeline(id).caps.value
  const modeLabel = shortMode(s.mode)

  const act = (fn: () => Promise<unknown>) => () => fn().catch(toastError)
  // Home conversations that Collie tells about this session.
  const watchers = sessions.value.filter((h) => h.projectId === homeProject.value?.id && h.watching?.includes(id))
  const del = async () => {
    const wt = s.worktree ? '\n\n' + t('Its worktree checkout will be removed (the branch is kept).') : ''
    if (!confirm(t('Delete “{title}”?', { title: s.title }) + wt)) return
    try {
      await api('DELETE', `/api/sessions/${id}`)
      go('#/')
    } catch (e) {
      toastError(e)
    }
  }

  return (
    <div class="session">
      <header class="sess-head">
        <button class="icon-btn back" onClick={() => go('#/')} aria-label={t('Back')}>
          <IBack size={20} />
        </button>
        <div class="sess-title-wrap">
          <button class="sess-title" onClick={() => setSettings(true)} title={t('Session settings')}>
            {s.title}
          </button>
          <div class="sess-sub">
            <AgentBadge agent={s.agent} small />
            {s.shared && (
              <span class="sub-item shared" title={sharedTitle(groupById.value.get(project?.ownerGroup || '')?.name)}>
                <IUsers size={11} /> {s.owner === me.value?.id ? t('shared') : userName(s.owner)}
              </span>
            )}
            {project?.runtime === 'container' && (
              <span class="sub-item iso" title={t("Runs in the project's isolated container")}>
                <IBox size={11} />
              </span>
            )}
            {project && (
              <a class="sub-link" href={`#/p/${project.id}`}>
                {project.name}
              </a>
            )}
            {s.schedule &&
              (scheduleById.value.has(s.schedule) ? (
                <a class="sub-link sched" href={`#/schedules/${s.schedule}`} title={t('Started by a schedule')}>
                  <IClock size={11} /> {t('Scheduled')}
                </a>
              ) : (
                <span class="sub-item" title={t('Started by a schedule')}>
                  <IClock size={11} />
                </span>
              ))}
            {s.branch && (
              <span class="sub-item mono">
                <IBranch size={11} />
                {s.branch.replace(/^collie\//, '')}
              </span>
            )}
            {s.model && <span class="sub-item wide-only">{s.model}</span>}
            {watchers.length > 0 && (
              <span class="sub-item watched" title={t('Your Home agent is told when this session finishes, needs you, or fails')}>
                <IBell size={11} /> {t('Home')}
              </span>
            )}
          </div>
        </div>
        <StatusPill status={s.status} bg={!!s.tasks?.length} />
        <Menu
          align="right"
          label={t('Session menu')}
          trigger={<IMore size={18} />}
          items={[
            { label: t('Settings…'), onClick: () => setSettings(true) },
            ...(project?.ownerGroup && s.owner === me.value?.id
              ? [
                  {
                    label: s.shared ? t('Make private') : shareLabel(groupById.value.get(project.ownerGroup)?.name),
                    onClick: act(() => api('PATCH', `/api/sessions/${id}`, { shared: !s.shared })),
                  },
                ]
              : []),
            ...(s.status === 'running' || s.status === 'waiting' ? [{ label: t('Interrupt'), hint: 'Esc', onClick: act(() => api('POST', `/api/sessions/${id}/interrupt`)) }] : []),
            ...(s.status !== 'stopped' ? [{ label: t('Stop agent process'), onClick: act(() => api('POST', `/api/sessions/${id}/stop`)) }] : []),
            ...(project?.runtime === 'container'
              ? [
                  {
                    label: t('Kill project container'),
                    danger: true,
                    onClick: () => {
                      if (confirm(t('Kill everything running in “{name}” now?', { name: project.name })))
                        api('POST', `/api/projects/${project.id}/stop`, { kill: true }).catch(toastError)
                    },
                  },
                ]
              : []),
            ...(watchers.length > 0
              ? [{ label: t('Stop Home from watching'), onClick: act(() => Promise.all(watchers.map((h) => api('DELETE', `/api/sessions/${h.id}/watching/${id}`)))) }]
              : []),
            { label: s.archived ? t('Unarchive') : t('Archive'), onClick: act(() => api('PATCH', `/api/sessions/${id}`, { archived: !s.archived })) },
            { label: t('Delete session'), danger: true, onClick: del },
          ]}
        />
      </header>
      <nav class="tabs">
        <a href={`#/s/${id}`} class={cls('tab', tab === 'chat' && 'on')}>
          <IChat size={14} /> {t('Chat')}
        </a>
        <a href={`#/s/${id}/changes`} class={cls('tab', tab === 'changes' && 'on')}>
          <IDiff size={14} /> {t('Changes')}
        </a>
        <a href={`#/s/${id}/term`} class={cls('tab', tab === 'term' && 'on')}>
          <ITerminal size={14} /> {t('Terminal')}
        </a>
        {(!s.owner || s.owner === me.value?.id) && (
          <a href={`#/s/${id}/ports`} class={cls('tab', tab === 'ports' && 'on')}>
            <IGlobe size={14} /> {t('Ports')}
          </a>
        )}
        <span class="grow" />
        {modeLabel && tab !== 'chat' && <span class="dim small tab-note">{modeLabel}</span>}
        {/* the composer shows it in the chat */}
        {tab !== 'chat' && <SessionMeter s={s} />}
      </nav>
      {project?.runtime === 'container' && <PendingBanner p={project} user={s.owner} />}
      {tab === 'chat' && (
        <>
          <Timeline session={s} />
          <TasksBar s={s} />
          <Composer session={s} caps={caps} />
        </>
      )}
      {tab === 'changes' && (
        <div class="tab-body">
          <ChangesView url={`/api/sessions/${id}/changes`} refreshKey={s.status === 'idle' ? s.updatedAt : 0} />
        </div>
      )}
      {tab === 'ports' && (
        <div class="tab-body">
          <PortsView s={s} />
        </div>
      )}
      {tab === 'term' && (
        <Suspense fallback={<div class="center pad"><Spinner /></div>}>
          <TerminalView owner={id} sessionId={id} />
        </Suspense>
      )}
      {settings && <SettingsModal s={s} onClose={() => setSettings(false)} />}
    </div>
  )
}
