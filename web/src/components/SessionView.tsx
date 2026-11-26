import { lazy, Suspense } from 'preact/compat'
import { useEffect, useState } from 'preact/hooks'
import type { Session } from '../types'
import { api } from '../api'
import { go, groupById, markSeen, me, projectById, route, sessionById, subscribe, timeline, toastError, agentCaps, userName } from '../store'
import { Timeline } from './Timeline'
import { Composer } from './Composer'
import { ChangesView } from './Changes'
import { AgentBadge, Menu, Modal, StatusPill } from './ui'
import { IBack, IBox, IBranch, IChat, IDiff, IMore, ITerminal, IUsers, Spinner } from './icons'
import { cls, home, money, shortMode, tokens } from '../util'

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
    <Modal title="Session settings" onClose={onClose}>
      <label class="field">
        <span class="field-l">Title</span>
        <input class="input" value={title} onInput={(e) => setTitle((e.target as HTMLInputElement).value)} />
      </label>
      <label class="field">
        <span class="field-l">Model</span>
        <input class="input" list="model-list" value={model} placeholder="default" onInput={(e) => setModel((e.target as HTMLInputElement).value)} />
        <datalist id="model-list">
          {models.map((m) => (
            <option key={m.id} value={m.id}>
              {m.label}
            </option>
          ))}
        </datalist>
        <span class="field-h">Takes effect on the next message.</span>
      </label>
      <div class="kv">
        <span>Directory</span>
        <span class="mono">{home(s.cwd)}</span>
        {s.branch && (
          <>
            <span>Branch</span>
            <span class="mono">{s.branch}</span>
          </>
        )}
        {s.agentRef && (
          <>
            <span>{s.agent === 'claude' ? 'Claude session' : 'Codex thread'}</span>
            <span class="mono small">{s.agentRef}</span>
          </>
        )}
        {!!s.costUsd && (
          <>
            <span>Cost</span>
            <span>{money(s.costUsd)}</span>
          </>
        )}
      </div>
      <div class="modal-actions">
        <button class="btn" onClick={onClose}>
          Cancel
        </button>
        <button class="btn primary" onClick={save}>
          Save
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
        <p>This session no longer exists.</p>
        <button class="btn" onClick={() => go('#/')}>
          Back
        </button>
      </div>
    )
  }
  const project = projectById.value.get(s.projectId)
  const caps = timeline(id).caps.value
  const ctxPct = s.ctxMax ? Math.min(100, Math.round(((s.ctxUsed || 0) / s.ctxMax) * 100)) : 0
  const modeLabel = shortMode(s.mode)

  const act = (fn: () => Promise<unknown>) => () => fn().catch(toastError)
  const del = async () => {
    const wt = s.worktree ? '\n\nIts worktree checkout will be removed (the branch is kept).' : ''
    if (!confirm(`Delete “${s.title}”?${wt}`)) return
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
        <button class="icon-btn back" onClick={() => go('#/')} aria-label="Back">
          <IBack size={20} />
        </button>
        <div class="sess-title-wrap">
          <button class="sess-title" onClick={() => setSettings(true)} title="Session settings">
            {s.title}
          </button>
          <div class="sess-sub">
            <AgentBadge agent={s.agent} small />
            {s.shared && (
              <span class="sub-item shared" title={`Shared with ${groupById.value.get(project?.ownerGroup || '')?.name || 'the group'}`}>
                <IUsers size={11} /> {s.owner === me.value?.id ? 'shared' : userName(s.owner)}
              </span>
            )}
            {project?.runtime === 'container' && (
              <span class="sub-item iso" title="Runs in the project's isolated container">
                <IBox size={11} />
              </span>
            )}
            {project && (
              <a class="sub-link" href={`#/p/${project.id}`}>
                {project.name}
              </a>
            )}
            {s.branch && (
              <span class="sub-item mono">
                <IBranch size={11} />
                {s.branch.replace(/^collie\//, '')}
              </span>
            )}
            {s.model && <span class="sub-item wide-only">{s.model}</span>}
            {ctxPct > 0 && (
              <span class={cls('sub-item ctx wide-only', ctxPct > 80 && 'warn')} title={`Context ${tokens(s.ctxUsed)} / ${tokens(s.ctxMax)}`}>
                ctx {ctxPct}%
              </span>
            )}
          </div>
        </div>
        <StatusPill status={s.status} />
        <Menu
          align="right"
          label="Session menu"
          trigger={<IMore size={18} />}
          items={[
            { label: 'Settings…', onClick: () => setSettings(true) },
            ...(project?.ownerGroup && s.owner === me.value?.id
              ? [
                  {
                    label: s.shared ? 'Make private' : `Share with ${groupById.value.get(project.ownerGroup)?.name || 'group'}`,
                    onClick: act(() => api('PATCH', `/api/sessions/${id}`, { shared: !s.shared })),
                  },
                ]
              : []),
            ...(s.status === 'running' || s.status === 'waiting' ? [{ label: 'Interrupt', hint: 'Esc', onClick: act(() => api('POST', `/api/sessions/${id}/interrupt`)) }] : []),
            ...(s.status !== 'stopped' ? [{ label: 'Stop agent process', onClick: act(() => api('POST', `/api/sessions/${id}/stop`)) }] : []),
            ...(project?.runtime === 'container'
              ? [
                  {
                    label: 'Kill project container',
                    danger: true,
                    onClick: () => {
                      if (confirm(`Kill everything running in “${project.name}” now?`))
                        api('POST', `/api/projects/${project.id}/stop`, { kill: true }).catch(toastError)
                    },
                  },
                ]
              : []),
            { label: s.archived ? 'Unarchive' : 'Archive', onClick: act(() => api('PATCH', `/api/sessions/${id}`, { archived: !s.archived })) },
            { label: 'Delete session', danger: true, onClick: del },
          ]}
        />
      </header>
      <nav class="tabs">
        <a href={`#/s/${id}`} class={cls('tab', tab === 'chat' && 'on')}>
          <IChat size={14} /> Chat
        </a>
        <a href={`#/s/${id}/changes`} class={cls('tab', tab === 'changes' && 'on')}>
          <IDiff size={14} /> Changes
        </a>
        <a href={`#/s/${id}/term`} class={cls('tab', tab === 'term' && 'on')}>
          <ITerminal size={14} /> Terminal
        </a>
        <span class="grow" />
        {modeLabel && tab !== 'chat' && <span class="dim small tab-note">{modeLabel}</span>}
      </nav>
      {tab === 'chat' && (
        <>
          <Timeline session={s} />
          <Composer session={s} caps={caps} />
        </>
      )}
      {tab === 'changes' && (
        <div class="tab-body">
          <ChangesView url={`/api/sessions/${id}/changes`} refreshKey={s.status === 'idle' ? s.updatedAt : 0} />
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
