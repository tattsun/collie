import { useEffect, useMemo, useRef, useState } from 'preact/hooks'
import type { Agent, Project, Session } from '../types'
import { api } from '../api'
import { agentCaps, agentsAvail, go, groupById, modes, projectById, projects, route, toastError } from '../store'
import { cls, home, isTouch } from '../util'
import { IBack, IBranch, IImage, IPlus, ISend, Spinner } from './icons'
import { Segmented, Toggle } from './ui'
import { Thumbs, pastedImages, useAttachments } from './attach'
import { t, ts } from '../i18n'

const pref = (k: string, d = '') => localStorage.getItem('collie.new.' + k) ?? d
const setPref = (k: string, v: string) => localStorage.setItem('collie.new.' + k, v)
// The permission mode is remembered only when it differs from the default
// (the first mode the server lists), so a changed default reaches devices
// that never picked one. ("mode2": "mode" also stored untouched defaults.)
const modeKey = (agent: string) => 'mode2.' + agent

const EFFORTS: Record<Agent, string[]> = {
  claude: ['low', 'medium', 'high', 'xhigh', 'max'],
  codex: ['low', 'medium', 'high', 'xhigh'],
}

export function NewSession() {
  const r = route.value
  const plist = projects.value
  const [projectId, setProjectId] = useState(() => {
    const q = r.query.get('project')
    if (q && projectById.value.has(q)) return q
    const last = pref('project')
    return projectById.value.has(last) ? last : plist[0]?.id || ''
  })
  const [agent, setAgent] = useState<Agent>(() => (pref('agent', 'claude') as Agent) || 'claude')
  const [mode, setMode] = useState(() => pref(modeKey(agent)))
  const [model, setModel] = useState(() => pref('model.' + agent))
  const [effort, setEffort] = useState(() => pref('effort.' + agent))
  const [worktree, setWorktree] = useState(() => pref('worktree') === '1')
  const [shared, setShared] = useState(true)
  const [prompt, setPrompt] = useState(() => sessionStorage.getItem('collie.newprompt') || '')
  const [busy, setBusy] = useState(false)
  const { atts, add, remove, uploading, ids } = useAttachments()
  const ta = useRef<HTMLTextAreaElement>(null)
  const file = useRef<HTMLInputElement>(null)

  useEffect(() => {
    const q = r.query.get('project')
    if (q && projectById.value.has(q)) setProjectId(q)
  }, [r.query.get('project')])

  useEffect(() => {
    if (!projectId && plist.length) setProjectId(plist[0].id)
  }, [plist.length])

  useEffect(() => {
    setMode(pref(modeKey(agent)))
    setModel(pref('model.' + agent))
    setEffort(pref('effort.' + agent))
  }, [agent])

  useEffect(() => {
    sessionStorage.setItem('collie.newprompt', prompt)
    const el = ta.current
    if (el) {
      el.style.height = 'auto'
      el.style.height = Math.min(el.scrollHeight, window.innerHeight * 0.45) + 'px'
    }
  }, [prompt])

  useEffect(() => {
    if (!isTouch) ta.current?.focus()
  }, [])

  const project = projectById.value.get(projectId)
  const ownerGroupName = project?.ownerGroup ? groupById.value.get(project.ownerGroup)?.name : ''
  const modeList = modes.value[agent] || []
  const effMode = modeList.some((m) => m.id === mode) ? mode : modeList[0]?.id || ''
  const models = agentCaps.value[agent]?.models?.length
    ? agentCaps.value[agent]!.models!
    : agent === 'claude'
      ? [{ id: 'opus', label: 'Opus' }, { id: 'sonnet', label: 'Sonnet' }, { id: 'haiku', label: 'Haiku' }]
      : []
  const canStart = !!project && project.status !== 'cloning' && (prompt.trim() || atts.length) && !uploading && !busy && agentsAvail.value[agent] !== false

  const start = async () => {
    if (!canStart) return
    setBusy(true)
    try {
      const s = await api<Session>('POST', '/api/sessions', {
        projectId,
        agent,
        mode: effMode,
        model: model === 'default' ? '' : model,
        effort,
        prompt,
        images: ids(),
        worktree: worktree && !!project?.git,
        shared: project?.ownerGroup ? shared : undefined,
      })
      setPref('project', projectId)
      setPref('agent', agent)
      if (effMode === modeList[0]?.id) localStorage.removeItem('collie.new.' + modeKey(agent))
      else setPref(modeKey(agent), effMode)
      setPref('model.' + agent, model)
      setPref('effort.' + agent, effort)
      setPref('worktree', worktree ? '1' : '0')
      sessionStorage.removeItem('collie.newprompt')
      setPrompt('')
      go(`#/s/${s.id}`)
    } catch (e) {
      toastError(e)
    } finally {
      setBusy(false)
    }
  }

  const modelOptions = useMemo(() => {
    const opts = models.map((m) => ({ id: m.id, label: m.label || m.id }))
    if (model && model !== 'default' && !opts.some((o) => o.id === model)) opts.push({ id: model, label: model })
    return opts
  }, [models, model])

  if (!plist.length) {
    return (
      <div class="page center-page">
        <div class="hero">
          <img src="/icon.svg" width={56} height={56} alt="" />
          <h1>{t('Welcome to Collie')}</h1>
          <p class="dim">{t('Create a project, then start Claude Code or Codex sessions on it from any device.')}</p>
          <button class="btn primary big" onClick={() => go('#/projects?new=1')}>
            <IPlus size={16} /> {t('Create a project')}
          </button>
        </div>
      </div>
    )
  }

  return (
    <div class="page new-session">
      <button class="icon-btn back new-back" onClick={() => go('#/')} aria-label={t('Back')}>
        <IBack size={20} />
      </button>
      <div class="new-inner">
        <h1 class="new-title">{t('What should we work on?')}</h1>
        <div class="composer-box big" onDragOver={(e) => e.preventDefault()} onDrop={(e) => {
          e.preventDefault()
          if (e.dataTransfer?.files) add(e.dataTransfer.files)
        }}>
          <Thumbs atts={atts} remove={remove} />
          <textarea
            ref={ta}
            rows={4}
            value={prompt}
            placeholder={t('Describe a task for {agent}…', { agent: agent === 'claude' ? 'Claude Code' : 'Codex' })}
            onInput={(e) => setPrompt((e.target as HTMLTextAreaElement).value)}
            onPaste={(e) => {
              const f = pastedImages(e)
              if (f.length) {
                e.preventDefault()
                add(f)
              }
            }}
            onKeyDown={(e) => {
              if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
                e.preventDefault()
                start()
              }
            }}
          />
          <div class="composer-bar">
            <button class="icon-btn" onClick={() => file.current?.click()} aria-label={t('Attach image')}>
              <IImage size={18} />
            </button>
            <input ref={file} type="file" accept="image/*" multiple hidden onChange={(e) => {
              const input = e.target as HTMLInputElement
              if (input.files) add(input.files)
              input.value = ''
            }} />
            <span class="grow" />
            <span class="dim small kbd-hint">{isTouch ? '' : 'Ctrl+Enter'}</span>
            <button class="send" disabled={!canStart} onClick={start} aria-label={t('Start session')}>
              {busy ? <Spinner size={14} /> : <ISend size={17} />}
            </button>
          </div>
        </div>

        <div class="new-opts">
          <label class="field">
            <span class="field-l">{t('Project')}</span>
            <div class="row gap">
              <select class="select" value={projectId} onChange={(e) => setProjectId((e.target as HTMLSelectElement).value)}>
                {projectGroups(plist).map(([label, items]) => (
                  <optgroup key={label} label={label}>
                    {items.map((p) => (
                      <option key={p.id} value={p.id} disabled={p.status === 'cloning'}>
                        {p.status === 'cloning' ? t('{name} (cloning…)', { name: p.name }) : p.name}
                      </option>
                    ))}
                  </optgroup>
                ))}
              </select>
              <button class="btn" onClick={() => go('#/projects?new=1')} title={t('New project')}>
                <IPlus size={15} />
              </button>
            </div>
            {project && <span class="field-h mono">{home(project.path)}</span>}
          </label>
          {project?.ownerGroup && (
            <div class="field">
              <Toggle
                checked={shared}
                onChange={setShared}
                label={ownerGroupName ? t('Share with {name}', { name: ownerGroupName }) : t('Share with the group')}
                hint={t('Members can follow and drive this session. It runs with your agent accounts.')}
              />
            </div>
          )}

          <div class="field">
            <span class="field-l">{t('Agent')}</span>
            <Segmented<Agent>
              value={agent}
              onChange={setAgent}
              options={[
                { value: 'claude', label: <span class="agent-opt claude">Claude Code</span>, disabled: agentsAvail.value.claude === false },
                { value: 'codex', label: <span class="agent-opt codex">Codex</span>, disabled: agentsAvail.value.codex === false },
              ]}
            />
            {agentsAvail.value[agent] === false && <span class="field-h err">{t("`{agent}` was not found on the server's PATH.", { agent })}</span>}
          </div>

          <label class="field">
            <span class="field-l">{t('Permissions')}</span>
            <select class="select" value={effMode} onChange={(e) => setMode((e.target as HTMLSelectElement).value)}>
              {modeList.map((m) => (
                <option key={m.id} value={m.id}>
                  {ts(m.label)}
                </option>
              ))}
            </select>
          </label>

          <div class="row gap wrap">
            <label class="field grow">
              <span class="field-l">{t('Model')}</span>
              <select class="select" value={model || 'default'} onChange={(e) => setModel((e.target as HTMLSelectElement).value)}>
                <option value="default">{t('Default')}</option>
                {modelOptions
                  .filter((m) => m.id !== 'default')
                  .map((m) => (
                    <option key={m.id} value={m.id}>
                      {m.label}
                    </option>
                  ))}
              </select>
            </label>
            <label class="field">
              <span class="field-l">{t('Effort')}</span>
              <select class="select" value={effort} onChange={(e) => setEffort((e.target as HTMLSelectElement).value)}>
                <option value="">{t('Default')}</option>
                {EFFORTS[agent].map((x) => (
                  <option key={x} value={x}>
                    {x}
                  </option>
                ))}
              </select>
            </label>
          </div>

          <div class={cls('field', !project?.git && 'muted')}>
            <Toggle
              checked={worktree && !!project?.git}
              disabled={!project?.git}
              onChange={setWorktree}
              label={t('Isolated worktree')}
              hint={project?.git ? t('Work on a new branch in its own checkout, so parallel sessions never collide.') : t('Requires a git repository.')}
            />
            {worktree && project?.git && (
              <span class="field-h">
                <IBranch size={12} /> {t('collie/<id> from {base}', { base: project ? 'HEAD' : '' })}
              </span>
            )}
          </div>
        </div>
      </div>
    </div>
  )
}

function projectGroups(list: Project[]): [string, Project[]][] {
  const mine = list.filter((p) => !p.ownerGroup)
  const out: [string, Project[]][] = []
  if (mine.length) out.push([t('Personal'), mine])
  const byGroup = new Map<string, Project[]>()
  for (const p of list) if (p.ownerGroup) byGroup.set(p.ownerGroup, [...(byGroup.get(p.ownerGroup) || []), p])
  for (const [gid, items] of byGroup) out.push([groupById.value.get(gid)?.name || t('Group'), items])
  return out
}
