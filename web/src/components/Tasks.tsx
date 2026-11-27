import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks'
import type { BgTask, Session } from '../types'
import { api } from '../api'
import { timeline, toastError } from '../store'
import { cls, duration, firstLine } from '../util'
import { t, ts } from '../i18n'
import { IBolt, IBot, IChevron, IList, ITerminal, IX } from './icons'

// What the agent left running in the background (commands, sub-agents...):
// it goes on after the turn has ended and the agent reports back when it
// ends. Shown above the composer, like Claude Code's background tasks.

const kindIcon = (k?: string) => (k === 'local_bash' ? ITerminal : k?.includes('agent') || k === 'in_process_teammate' ? IBot : k?.startsWith('workflow') ? IList : IBolt)

/** The command or prompt that started the task, from its tool call. */
function source(sid: string, task: BgTask): string {
  if (!task.toolId) return ''
  const tl = timeline(sid)
  const i = tl.idx.get(task.toolId)
  const input = i === undefined ? undefined : tl.items.value[i]?.tool?.input
  if (!input || typeof input !== 'object') return ''
  return firstLine(String(input.command || input.prompt || ''), 300)
}

/** Scroll the timeline to the tool call that started the task. */
function reveal(task: BgTask) {
  if (!task.toolId) return
  document.querySelector(`[data-item="${CSS.escape(task.toolId)}"]`)?.scrollIntoView({ behavior: 'smooth', block: 'center' })
}

/** Background commands: Claude Code writes what they print to a file,
 *  whose end Collie can show. */
const hasOutput = (task: BgTask) => task.kind === 'local_bash' && !!task.toolId

interface Output {
  text: string
  /** it printed more before text */
  more?: boolean
}

/** The end of what a background command has printed, kept up to date
 *  while shown (and scrolled to its end unless the user scrolled up). */
function TaskOutput({ sid, task }: { sid: string; task: BgTask }) {
  const [out, setOut] = useState<Output | null>(null)
  const [err, setErr] = useState('')
  const pre = useRef<HTMLPreElement>(null)
  const stick = useRef(true)
  useEffect(() => {
    let alive = true
    let timer = 0
    const load = async () => {
      try {
        const o = await api<Output>('GET', `/api/sessions/${sid}/tasks/${encodeURIComponent(task.id)}/output`)
        if (alive) {
          setOut((prev) => (prev && prev.text === o.text && !!prev.more === !!o.more ? prev : o))
          setErr('')
        }
      } catch (e) {
        if (alive) setErr(ts(e instanceof Error ? e.message : String(e)))
      }
      if (alive) timer = window.setTimeout(load, document.hidden ? 6000 : 2000)
    }
    load()
    return () => {
      alive = false
      clearTimeout(timer)
    }
  }, [sid, task.id])
  useLayoutEffect(() => {
    const el = pre.current
    if (el && stick.current) el.scrollTop = el.scrollHeight
  }, [out])
  const onScroll = () => {
    const el = pre.current
    if (el) stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 24
  }

  return (
    <div class="task-out">
      {out === null && !err ? (
        <div class="task-out-note">{t('Loading…')}</div>
      ) : out && out.text.trim() ? (
        <pre ref={pre} class="task-out-text" onScroll={onScroll}>
          {out.text}
        </pre>
      ) : (
        !err && <div class="task-out-note">{t('No output yet')}</div>
      )}
      {err && <div class="task-out-note err">{err}</div>}
      <div class="task-out-foot">
        {out?.more && <span>{t('Only the last 64 KB')}</span>}
        <span class="grow" />
        <button class="link-btn" onClick={() => reveal(task)}>
          {t('Show where it started')}
        </button>
      </div>
    </div>
  )
}

// (the server's clock: never show a negative time)
const elapsed = (now: number, task: BgTask) => duration(Math.max(1000, now - task.started))

function useTick(on: boolean) {
  const [, tick] = useState(0)
  useEffect(() => {
    if (!on) return
    const h = setInterval(() => tick((x) => x + 1), 1000)
    return () => clearInterval(h)
  }, [on])
}

export function TasksBar({ s }: { s: Session }) {
  const tasks = s.tasks || []
  const [open, setOpen] = useState(false)
  const [stopping, setStopping] = useState<string | null>(null)
  const [shown, setShown] = useState<string | null>(null)
  useTick(tasks.length > 0)
  if (!tasks.length) return null

  const stop = async (task: BgTask, title: string) => {
    if (!confirm(t('Stop “{title}”?', { title }))) return
    setStopping(task.id)
    try {
      await api('POST', `/api/sessions/${s.id}/tasks/${encodeURIComponent(task.id)}/stop`)
    } catch (e) {
      toastError(e)
    } finally {
      setStopping(null)
    }
  }
  const now = Date.now()
  const first = tasks[0]

  return (
    <div class={cls('tasks', open && 'open')}>
      <div class="tasks-card">
        <button class="tasks-head" onClick={() => setOpen(!open)} aria-expanded={open}>
          <span class="dot idle bg" />
          <span class="tasks-n">{tasks.length === 1 ? t('1 task running in the background') : t('{n} tasks running in the background', { n: tasks.length })}</span>
          {!open && <span class="tasks-first">{first.title || source(s.id, first)}</span>}
          <span class="grow" />
          <span class="tasks-time">{elapsed(now, first)}</span>
          <IChevron size={13} class="chev" />
        </button>
        {open && (
          <ul class="tasks-list">
            {tasks.map((task) => {
              const Icon = kindIcon(task.kind)
              const src = source(s.id, task)
              const title = task.title || src || task.kind || t('Task')
              const out = hasOutput(task)
              const showing = out && shown === task.id
              return (
                <li key={task.id} class={cls('task', showing && 'showing')}>
                  <div class="task-row">
                    <Icon size={14} />
                    {/* a command: the whole entry opens its output (the button's click bubbles up) */}
                    <div class={cls('task-main', out && 'tappable')} onClick={out ? () => setShown(showing ? null : task.id) : undefined}>
                      {out ? (
                        <button class="task-title" aria-expanded={showing} title={t('Show output')}>
                          {title}
                          <IChevron size={12} class="chev" />
                        </button>
                      ) : (
                        <button class="task-title" onClick={() => reveal(task)} disabled={!task.toolId} title={task.toolId ? t('Show where it started') : undefined}>
                          {title}
                        </button>
                      )}
                      {src && src !== title && <div class={cls('task-src', task.kind === 'local_bash' && 'mono')}>{src}</div>}
                      {task.summary && <div class="task-sum">{task.summary}</div>}
                    </div>
                    <span class="tasks-time">{elapsed(now, task)}</span>
                    <button class="icon-btn task-stop" onClick={() => stop(task, title)} disabled={stopping === task.id} aria-label={t('Stop task')} title={t('Stop task')}>
                      <IX size={14} />
                    </button>
                  </div>
                  {showing && <TaskOutput sid={s.id} task={task} />}
                </li>
              )
            })}
          </ul>
        )}
      </div>
    </div>
  )
}
