import { useEffect, useMemo, useRef, useState } from 'preact/hooks'
import type { Agent, Schedule, ScheduleRun } from '../types'
import { api } from '../api'
import { agentsAvail, efforts, go, groupById, homeProject, modes, projectById, projects, route, scheduleById, schedules, sessionById, sessionPrefs, toast, toastError } from '../store'
import { ago, cls, shortMode, until } from '../util'
import { browserTZ, defaultTiming, describeCron, fmtWhen, fromCron, timeZones, toCron, weekday, type Freq, type Timing } from '../schedule'
import { t, ts } from '../i18n'
import { IBack, IBranch, IChevron, IClock, IEdit, IHome, IPause, IPlay, IPlus, ITrash, Spinner } from './icons'
import { AgentBadge, Segmented, StatusDot, Toggle } from './ui'
import { projectGroups } from './NewSession'
import { modelOptions } from './SessionPrefs'

/** Schedules: #/schedules (list), #/schedules/new, #/schedules/<id>. */
export function SchedulesPage({ id }: { id?: string }) {
  if (id === 'new') return <NewSchedule />
  if (id) return <ScheduleDetail id={id} />
  return <ScheduleList />
}

const projectName = (pid: string) => {
  const p = projectById.value.get(pid)
  return p ? (p.home ? t('Home') : p.name) : t('(project not available)')
}

function nextLabel(s: Schedule): string {
  if (s.paused) return t('Paused')
  if (!s.nextAt) return t('No upcoming run')
  return t('Next: {when} (in {left})', { when: fmtWhen(s.nextAt, s.tz), left: until(s.nextAt) })
}

/** A run's state: running runs show their session's. */
function RunDot({ run }: { run: ScheduleRun }) {
  const s = run.sessionId ? sessionById.value.get(run.sessionId) : undefined
  if (run.status === 'running') return <StatusDot status={s?.status || 'starting'} bg={!!s?.tasks?.length} />
  return <span class={cls('run-dot', run.status)} />
}

function ScheduleList() {
  const list = useMemo(() => [...schedules.value].sort((a, b) => (a.paused ? 1 : 0) - (b.paused ? 1 : 0) || (a.nextAt || 0) - (b.nextAt || 0)), [schedules.value])
  return (
    <div class="page page-pad narrow">
      <div class="page-head">
        <button class="icon-btn back" onClick={() => go('#/')} aria-label={t('Back')}>
          <IBack size={20} />
        </button>
        <h1>{t('Schedules')}</h1>
        <span class="grow" />
        <button class="btn primary small" onClick={() => go('#/schedules/new')} disabled={!projects.value.length && !homeProject.value}>
          <IPlus size={15} /> {t('New schedule')}
        </button>
      </div>
      <p class="dim small sched-intro">{t('Start an agent with the same instructions at set times. Each run is a new session; a run never starts while the previous one is still going.')}</p>
      <div class="plist">
        {list.map((s) => {
          const last = s.runs?.[0]
          return (
            <a key={s.id} href={`#/schedules/${s.id}`} class={cls('card prow-card sched-card', s.paused && 'paused')}>
              {s.paused ? <IPause size={18} /> : <IClock size={18} />}
              <div class="grow minw0">
                <div class="b ellipsis">{s.title || s.prompt}</div>
                <div class="dim small ellipsis">
                  {describeCron(s.cron)} · {projectName(s.projectId)}
                </div>
                <div class="small sched-next ellipsis">{nextLabel(s)}</div>
              </div>
              {last && (
                <span class="sched-last" title={runLabel(last)}>
                  <RunDot run={last} />
                </span>
              )}
              <IChevron size={16} />
            </a>
          )
        })}
      </div>
      {list.length === 0 && (
        <div class="card sched-empty">
          <p>{t('For example:')}</p>
          <ul>
            <li>{t('Every morning at 9:00, check for dependency updates and open a PR if needed')}</li>
            <li>{t('Every Friday at 18:00, sum up what was merged this week')}</li>
          </ul>
          <p class="dim small">{t('You can also ask your Home agent: “every Friday at 6pm, summarize the week”.')}</p>
        </div>
      )}
    </div>
  )
}

function NewSchedule() {
  return (
    <div class="page page-pad narrow">
      <div class="page-head">
        <button class="icon-btn back" onClick={() => go('#/schedules')} aria-label={t('Back')}>
          <IBack size={20} />
        </button>
        <h1>{t('New schedule')}</h1>
      </div>
      <ScheduleForm onDone={(s) => go(`#/schedules/${s.id}`)} />
    </div>
  )
}

function runLabel(r: ScheduleRun): string {
  switch (r.status) {
    case 'running':
      return t('Running')
    case 'ok':
      return t('Succeeded')
    case 'error':
      return t('Failed')
  }
  return t('Skipped')
}

function triggerLabel(r: ScheduleRun): string {
  if (r.status === 'skipped') return r.count && r.count > 1 ? t('Skipped {n} times', { n: r.count }) : t('Skipped')
  if (r.trigger === 'manual') return t('Run now')
  if (r.trigger === 'catchup') return t('Late run')
  return t('On schedule')
}

function RunRow({ r, tz }: { r: ScheduleRun; tz: string }) {
  const s = r.sessionId ? sessionById.value.get(r.sessionId) : undefined
  const note =
    r.trigger === 'catchup' && r.planned
      ? r.missed && r.missed > 1
        ? t('Collie was not running at {when}; {n} planned runs were made up with this one.', { when: fmtWhen(r.planned, tz), n: r.missed })
        : t('Collie was not running at {when}.', { when: fmtWhen(r.planned, tz) })
      : ''
  const body = (
    <>
      <RunDot run={r} />
      <div class="grow minw0">
        <div class="run-top">
          <span class="b">{triggerLabel(r)}</span>
          <span class="dim small">{fmtWhen(r.at)}</span>
          <span class="run-ago small">{ago(r.at)}</span>
        </div>
        {r.status !== 'skipped' && <div class={cls('small', r.status === 'error' ? 'err' : 'dim')}>{runLabel(r)}{s && r.status === 'running' && s.status === 'waiting' ? ' · ' + t('Needs your input') : ''}</div>}
        {r.error && <div class="small dim run-err">{ts(r.error)}</div>}
        {note && <div class="small dim">{note}</div>}
        {r.sessionId && !s && <div class="small dim">{t('The session was deleted.')}</div>}
      </div>
      {s && <IChevron size={16} />}
    </>
  )
  return s ? (
    <a class="run-row link" href={`#/s/${s.id}`} title={t('Open session')}>
      {body}
    </a>
  ) : (
    <div class="run-row">{body}</div>
  )
}

function ScheduleDetail({ id }: { id: string }) {
  const s = scheduleById.value.get(id)
  const [editing, setEditing] = useState(route.value.query.get('edit') === '1')
  const [busy, setBusy] = useState('')
  if (!s) {
    return (
      <div class="page center-page dim">
        <p>{t('This schedule no longer exists.')}</p>
        <button class="btn" onClick={() => go('#/schedules')}>
          {t('Schedules')}
        </button>
      </div>
    )
  }
  const act = async (what: string, fn: () => Promise<unknown>) => {
    setBusy(what)
    try {
      await fn()
    } catch (e) {
      toastError(e)
    } finally {
      setBusy('')
    }
  }
  const runNow = () =>
    act('run', async () => {
      const run = await api<ScheduleRun>('POST', `/api/schedules/${id}/run`)
      toast(t('Started'))
      if (run.sessionId) go(`#/s/${run.sessionId}`)
    })
  const togglePause = () => act('pause', () => api('PATCH', `/api/schedules/${id}`, { paused: !s.paused }))
  const remove = () => {
    if (!confirm(t('Delete the schedule “{name}”? The sessions it started are kept.', { name: s.title || s.prompt.slice(0, 40) }))) return
    act('delete', async () => {
      await api('DELETE', `/api/schedules/${id}`)
      go('#/schedules')
    })
  }
  const running = s.runs?.some((r) => r.status === 'running')
  const p = projectById.value.get(s.projectId)
  return (
    <div class="page page-pad narrow">
      <div class="page-head">
        <button class="icon-btn back" onClick={() => (editing ? setEditing(false) : go('#/schedules'))} aria-label={t('Back')}>
          <IBack size={20} />
        </button>
        <h1 class="ellipsis grow">{editing ? t('Edit schedule') : s.title || t('Schedule')}</h1>
      </div>
      {editing ? (
        <ScheduleForm sc={s} onDone={() => setEditing(false)} />
      ) : (
        <>
          <div class="card sched-summary">
            <div class="sched-when">
              {s.paused ? <IPause size={16} /> : <IClock size={16} />}
              <span class="b">{describeCron(s.cron)}</span>
              <span class="dim small">{s.tz}</span>
            </div>
            <div class={cls('small', s.paused ? 'warn' : 'dim')}>{nextLabel(s)}</div>
            <div class="sched-meta">
              {p?.home && <IHome size={12} />}
              <span>{projectName(s.projectId)}</span>
              <AgentBadge agent={s.agent} small />
              <span class="chip">{shortMode(s.mode)}</span>
              {s.model && <span class="chip">{s.model}</span>}
              {s.effort && <span class="chip">{s.effort}</span>}
              {s.worktree && (
                <span class="chip">
                  <IBranch size={11} /> {t('worktree')}
                </span>
              )}
            </div>
            <div class="sched-prompt">{s.prompt}</div>
          </div>
          <div class="sched-actions">
            <button class="btn primary" onClick={runNow} disabled={!!busy || running} title={running ? t('The previous run is still running') : undefined}>
              {busy === 'run' ? <Spinner size={12} /> : <IPlay size={14} />} {t('Run now')}
            </button>
            <button class="btn" onClick={togglePause} disabled={!!busy}>
              {s.paused ? <IPlay size={14} /> : <IPause size={14} />} {s.paused ? t('Resume') : t('Pause')}
            </button>
            <button class="btn" onClick={() => setEditing(true)} disabled={!!busy}>
              <IEdit size={14} /> {t('Edit')}
            </button>
            <button class="btn ghost danger" onClick={remove} disabled={!!busy} aria-label={t('Delete schedule')}>
              <ITrash size={14} />
            </button>
          </div>
          <h2 class="sched-h">{t('History')}</h2>
          {s.runs?.length ? (
            <div class="card run-list">
              {s.runs.map((r) => (
                <RunRow key={r.id} r={r} tz={s.tz} />
              ))}
            </div>
          ) : (
            <div class="dim small pad">{t('No runs yet.')}</div>
          )}
        </>
      )}
    </div>
  )
}

const FREQS: { value: Freq; label: string }[] = [
  { value: 'daily', label: 'Every day' },
  { value: 'weekdays', label: 'Weekdays' },
  { value: 'weekly', label: 'Weekly' },
  { value: 'monthly', label: 'Monthly' },
  { value: 'hourly', label: 'Hourly' },
  { value: 'custom', label: 'Custom (cron)' },
]

// Modes that run without stopping to ask (the rest may wait for the user).
const UNATTENDED = new Set(['auto', 'bypassPermissions', 'dontAsk', 'full'])

const pref = (k: string, d = '') => localStorage.getItem('collie.new.' + k) ?? d

function ScheduleForm({ sc, onDone }: { sc?: Schedule; onDone: (s: Schedule) => void }) {
  const q = route.value.query
  const [projectId, setProjectId] = useState(() => {
    if (sc) return sc.projectId
    const want = q.get('project') || pref('project')
    return projectById.value.has(want) ? want : projects.value[0]?.id || homeProject.value?.id || ''
  })
  const [title, setTitle] = useState(sc?.title || '')
  const [prompt, setPrompt] = useState(sc?.prompt || '')
  const [timing, setTiming] = useState<Timing>(() => (sc ? fromCron(sc.cron) : defaultTiming()))
  const [tz, setTz] = useState(sc?.tz || browserTZ())
  const [agent, setAgent] = useState<Agent>(sc?.agent || (pref('agent', 'claude') as Agent) || 'claude')
  const [mode, setMode] = useState(sc?.mode || '')
  const [model, setModel] = useState(sc?.model || '')
  const [effort, setEffort] = useState(sc?.effort || '')
  const [worktree, setWorktree] = useState(!!sc?.worktree)
  const [shared, setShared] = useState(sc ? !!sc.shared : true)
  const [busy, setBusy] = useState(false)
  const [preview, setPreview] = useState<{ times?: number[]; error?: string }>({})
  const ta = useRef<HTMLTextAreaElement>(null)

  const project = projectById.value.get(projectId)
  const cron = toCron(timing)
  const modeList = modes.value[agent] || []
  const effMode = modeList.some((m) => m.id === mode) ? mode : modeList[0]?.id || ''
  // '' follows my defaults at each run; 'default' is the agent's own.
  const mine = sessionPrefs.value.agents[agent] || {}
  const yours = (v?: string) => t('Your default ({value})', { value: v || t('Default') })
  const zones = useMemo(() => timeZones(tz), [])

  // When it would run, as the server reads it.
  useEffect(() => {
    if (!cron) return setPreview({})
    const h = setTimeout(() => {
      api<{ times: number[] }>('GET', `/api/schedules/times?cron=${encodeURIComponent(cron)}&tz=${encodeURIComponent(tz)}`)
        .then((r) => setPreview({ times: r.times }))
        .catch((e) => setPreview({ error: ts(e instanceof Error ? e.message : String(e)) }))
    }, 250)
    return () => clearTimeout(h)
  }, [cron, tz])

  useEffect(() => {
    const el = ta.current
    if (el) {
      el.style.height = 'auto'
      el.style.height = Math.min(el.scrollHeight, window.innerHeight * 0.4) + 'px'
    }
  }, [prompt])

  const set = (patch: Partial<Timing>) => setTiming({ ...timing, ...patch })
  const canSave = !!project && !!prompt.trim() && !!cron && !preview.error && !busy

  const save = async (e: Event) => {
    e.preventDefault()
    if (!canSave) return
    setBusy(true)
    try {
      const body = {
        projectId,
        title: title.trim(),
        prompt,
        agent,
        mode: effMode,
        model,
        effort,
        worktree: worktree && !!project?.git,
        shared: project?.ownerGroup ? shared : undefined,
        cron,
        tz,
      }
      const out = sc ? await api<Schedule>('PATCH', `/api/schedules/${sc.id}`, body) : await api<Schedule>('POST', '/api/schedules', body)
      onDone(out)
    } catch (err) {
      toastError(err)
    } finally {
      setBusy(false)
    }
  }

  const homeId = homeProject.value?.id
  return (
    <form class="card form sched-form" onSubmit={save}>
      <label class="field">
        <span class="field-l">{t('Instructions')}</span>
        <textarea
          ref={ta}
          class="input"
          rows={4}
          value={prompt}
          placeholder={t('e.g. Check for outdated dependencies; if any can be updated safely, update them, run the tests and open a PR.')}
          onInput={(e) => setPrompt((e.target as HTMLTextAreaElement).value)}
        />
        <span class="field-h">{t('Each run starts a new session with these instructions; it does not see earlier runs.')}</span>
      </label>

      <label class="field">
        <span class="field-l">{t('Project')}</span>
        <select class="select" value={projectId} onChange={(e) => setProjectId((e.target as HTMLSelectElement).value)}>
          {homeId && <option value={homeId}>{t('Home')}</option>}
          {projectGroups(projects.value).map(([label, items]) => (
            <optgroup key={label} label={label}>
              {items.map((p) => (
                <option key={p.id} value={p.id}>
                  {p.name}
                </option>
              ))}
            </optgroup>
          ))}
          {!project && projectId && <option value={projectId}>{t('(project not available)')}</option>}
        </select>
      </label>

      <div class="field">
        <span class="field-l">{t('When')}</span>
        <div class="chips" role="radiogroup">
          {FREQS.map((f) => (
            <button
              type="button"
              key={f.value}
              role="radio"
              aria-checked={timing.freq === f.value}
              class={cls('chip-opt', timing.freq === f.value && 'on')}
              onClick={() => set({ freq: f.value, cron: timing.freq === 'custom' ? timing.cron : cron })}
            >
              {t(f.label)}
            </button>
          ))}
        </div>
        {timing.freq === 'weekly' && (
          <div class="days" role="group" aria-label={t('Days')}>
            {[1, 2, 3, 4, 5, 6, 0].map((d) => {
              const on = timing.days.includes(d)
              return (
                <button
                  type="button"
                  key={d}
                  aria-pressed={on}
                  class={cls('day', on && 'on')}
                  onClick={() => set({ days: on ? timing.days.filter((x) => x !== d) : [...timing.days, d] })}
                >
                  {weekday(d)}
                </button>
              )
            })}
          </div>
        )}
        <div class="row gap wrap sched-time">
          {timing.freq === 'monthly' && (
            <label class="row gap">
              <span class="dim small">{t('Day')}</span>
              <select class="select" value={timing.dom} onChange={(e) => set({ dom: +(e.target as HTMLSelectElement).value })}>
                {Array.from({ length: 31 }, (_, i) => i + 1).map((d) => (
                  <option key={d} value={d}>
                    {d}
                  </option>
                ))}
              </select>
            </label>
          )}
          {timing.freq !== 'hourly' && timing.freq !== 'custom' && (
            <input class="input time" type="time" required value={timing.time} onInput={(e) => set({ time: (e.target as HTMLInputElement).value })} aria-label={t('Time')} />
          )}
          {timing.freq === 'hourly' && (
            <label class="row gap">
              <span class="dim small">{t('Minutes past the hour')}</span>
              <input class="input num" type="number" min={0} max={59} value={timing.minute} onInput={(e) => set({ minute: Math.min(59, Math.max(0, parseInt((e.target as HTMLInputElement).value, 10) || 0)) })} />
            </label>
          )}
          {timing.freq === 'custom' && (
            <input
              class="input mono grow"
              value={timing.cron}
              placeholder="0 9 * * 1-5"
              autoCapitalize="off"
              autoCorrect="off"
              spellcheck={false}
              onInput={(e) => set({ cron: (e.target as HTMLInputElement).value })}
              aria-label={t('Cron expression')}
            />
          )}
        </div>
        {timing.freq === 'custom' && <span class="field-h">{t('minute hour day-of-month month day-of-week, e.g. “30 8 * * 1-5” for weekdays at 8:30.')}</span>}
        {timing.freq === 'monthly' && timing.dom > 28 && <span class="field-h">{t('Months without that day are skipped.')}</span>}
        <select class="select" value={tz} onChange={(e) => setTz((e.target as HTMLSelectElement).value)} aria-label={t('Time zone')}>
          {zones.map((z) => (
            <option key={z} value={z}>
              {z}
            </option>
          ))}
        </select>
        <div class={cls('sched-preview small', preview.error ? 'err' : 'dim')}>
          {preview.error
            ? preview.error
            : preview.times?.length
              ? t('Next runs: {times}', { times: preview.times.map((x) => fmtWhen(x, tz)).join(' · ') })
              : ' '}
        </div>
      </div>

      <label class="field">
        <span class="field-l">
          {t('Name')} <span class="dim">{t('(optional)')}</span>
        </span>
        <input class="input" value={title} maxLength={100} placeholder={t('Also the title of its sessions')} onInput={(e) => setTitle((e.target as HTMLInputElement).value)} />
      </label>

      <div class="field">
        <span class="field-l">{t('Agent')}</span>
        <Segmented<Agent>
          value={agent}
          onChange={(a) => {
            setAgent(a)
            setMode('')
            setModel('')
            setEffort('')
          }}
          options={[
            { value: 'claude', label: <span class="agent-opt claude">Claude Code</span>, disabled: agentsAvail.value.claude === false },
            { value: 'codex', label: <span class="agent-opt codex">Codex</span>, disabled: agentsAvail.value.codex === false },
          ]}
        />
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
        {effMode && !UNATTENDED.has(effMode) && <span class="field-h warn">{t('Runs start while you may be away; in this mode the agent may stop and wait for your approval.')}</span>}
      </label>

      <div class="field">
        <div class="row gap wrap sched-me">
          <label class="field">
            <span class="field-l">{t('Model')}</span>
            <select class="select" value={model} onChange={(e) => setModel((e.target as HTMLSelectElement).value)}>
              <option value="">{yours(modelOptions(agent, mine.model || '').find((m) => m.id === mine.model)?.label || mine.model)}</option>
              <option value="default">{t("Agent's default")}</option>
              {modelOptions(agent, model === 'default' ? '' : model).map((m) => (
                <option key={m.id} value={m.id}>
                  {m.label}
                </option>
              ))}
            </select>
          </label>
          <label class="field">
            <span class="field-l">{t('Effort')}</span>
            <select class="select" value={effort} onChange={(e) => setEffort((e.target as HTMLSelectElement).value)}>
              <option value="">{yours(mine.effort)}</option>
              <option value="default">{t("Agent's default")}</option>
              {(efforts.value[agent] || []).map((x) => (
                <option key={x} value={x}>
                  {x}
                </option>
              ))}
            </select>
          </label>
        </div>
        <span class="field-h sched-me-h">
          {t('Your default follows what you set for new sessions, at each run.')} <a href="#/account/agents">{t('Change defaults')}</a>
        </span>
      </div>

      <div class={cls('field', !project?.git && 'muted')}>
        <Toggle
          checked={worktree && !!project?.git}
          disabled={!project?.git}
          onChange={setWorktree}
          label={t('Isolated worktree')}
          hint={project?.git ? t('Each run works on a new branch in its own checkout.') : t('Requires a git repository.')}
        />
      </div>
      {project?.ownerGroup && (
        <div class="field">
          <Toggle
            checked={shared}
            onChange={setShared}
            label={groupById.value.get(project.ownerGroup)?.name ? t('Share with {name}', { name: groupById.value.get(project.ownerGroup)!.name }) : t('Share with the group')}
            hint={t('Members can follow and drive its sessions. The schedule itself stays yours.')}
          />
        </div>
      )}

      <div class="modal-actions">
        {sc && (
          <button type="button" class="btn" onClick={() => onDone(sc)}>
            {t('Cancel')}
          </button>
        )}
        <button class="btn primary" type="submit" disabled={!canSave}>
          {busy && <Spinner size={12} />}
          {sc ? t('Save') : t('Create schedule')}
        </button>
      </div>
    </form>
  )
}
