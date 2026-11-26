import { memo } from 'preact/compat'
import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'preact/hooks'
import type { Item, Session, Ask, PlanStep } from '../types'
import { timeline, toastError } from '../store'
import { api } from '../api'
import { renderMarkdown } from '../markdown'
import { parseDiff, snippetDiff, diffStats, type DiffLine } from '../diff'
import { describeTool } from './tools'
import { cls, copyText, duration, money } from '../util'
import { ICheck, IX, IChevron, IBrain, IShield, IDown, IStop, Spinner } from './icons'

// ---------------------------------------------------------------- shared bits

function onMdClick(e: MouseEvent) {
  const t = e.target as HTMLElement
  if (t.dataset && 'copy' in t.dataset) {
    const code = t.closest('.code')?.querySelector('code')?.textContent || ''
    copyText(code).then((ok) => {
      t.textContent = ok ? 'Copied' : 'Failed'
      setTimeout(() => (t.textContent = 'Copy'), 1200)
    })
  }
}

export function Markdown({ text, class: c }: { text: string; class?: string }) {
  const html = useMemo(() => renderMarkdown(text), [text])
  return <div class={cls('md', c)} dangerouslySetInnerHTML={{ __html: html }} onClick={onMdClick} />
}

export function DiffView({ lines, max = 1500 }: { lines: DiffLine[]; max?: number }) {
  const shown = lines.length > max ? lines.slice(0, max) : lines
  return (
    <div class="diff">
      {shown.map((l, i) => (
        <div key={i} class={'dl ' + l.k}>
          {l.t || ' '}
        </div>
      ))}
      {lines.length > max && <div class="dl meta">… {lines.length - max} more lines</div>}
    </div>
  )
}

function Stats({ lines }: { lines: DiffLine[] }) {
  const { add, del } = diffStats(lines)
  if (!add && !del) return null
  return (
    <span class="stats">
      {add > 0 && <span class="add">+{add}</span>}
      {del > 0 && <span class="del">−{del}</span>}
    </span>
  )
}

function Pre({ text, class: c }: { text: string; class?: string }) {
  return <pre class={cls('out', c)}>{text}</pre>
}

function editLines(name: string, input: any): DiffLine[] | null {
  if (!input) return null
  if (name === 'Edit') return snippetDiff(input.old_string ?? '', input.new_string ?? '')
  if (name === 'MultiEdit' && Array.isArray(input.edits)) {
    const out: DiffLine[] = []
    input.edits.forEach((e: any, i: number) => {
      if (i) out.push({ k: 'hunk', t: '⋯' })
      out.push(...snippetDiff(e.old_string ?? '', e.new_string ?? ''))
    })
    return out
  }
  if (name === 'Write' && typeof input.content === 'string') {
    return input.content.split('\n').map((t: string) => ({ k: 'add' as const, t: '+' + t }))
  }
  return null
}

// ---------------------------------------------------------------- items

function UserMsg({ item }: { item: Item }) {
  return (
    <div class="msg-user">
      {item.images && item.images.length > 0 && (
        <div class="thumbs">
          {item.images.map((id) => (
            <a key={id} href={`/api/uploads/${id}`} target="_blank" rel="noopener">
              <img src={`/api/uploads/${id}`} loading="lazy" alt="" />
            </a>
          ))}
        </div>
      )}
      {item.text && <div class="bubble">{item.text}</div>}
    </div>
  )
}

function Thinking({ item }: { item: Item }) {
  const [open, setOpen] = useState(false)
  if (!item.text) {
    if (item.done) return null
    return (
      <div class="thinking">
        <IBrain size={14} /> <span class="shimmer">Thinking…</span>
      </div>
    )
  }
  return (
    <div class={cls('thinking', open && 'open')}>
      <button class="row-btn" onClick={() => setOpen(!open)}>
        <IBrain size={14} />
        <span class={cls(!item.done && 'shimmer')}>{item.done ? 'Thought' : 'Thinking…'}</span>
        <IChevron size={12} class="chev" />
      </button>
      {open && <Markdown text={item.text} class="dim" />}
    </div>
  )
}

function TodoList({ todos }: { todos: { content?: string; step?: string; status: string; activeForm?: string }[] }) {
  return (
    <ul class="todos">
      {todos.map((t, i) => {
        const st = t.status === 'in_progress' ? 'inProgress' : t.status
        return (
          <li key={i} class={st}>
            <span class="box">{st === 'completed' ? <ICheck size={11} /> : st === 'inProgress' ? <Spinner size={9} /> : null}</span>
            <span>{st === 'inProgress' && t.activeForm ? t.activeForm : t.content ?? t.step}</span>
          </li>
        )
      })}
    </ul>
  )
}

function StatusIcon({ status }: { status: string }) {
  if (status === 'running') return <Spinner size={12} />
  if (status === 'error') return <IX size={13} class="err" />
  if (status === 'denied') return <IX size={13} class="warn" />
  if (status === 'interrupted') return <IStop size={11} class="warn" />
  return <ICheck size={13} class="ok" />
}

function ToolCall({ item, kids, cwd }: { item: Item; kids?: Item[]; cwd: string }) {
  const tool = item.tool!
  const [open, setOpen] = useState(false)
  const v = describeTool(tool.name, tool.input, cwd)
  const elines = useMemo(() => (tool.diff ? parseDiff(tool.diff) : editLines(tool.name, tool.input)), [tool.diff, tool.input, tool.name])
  const Icon = v.icon

  if (tool.name === 'TodoWrite' && Array.isArray(tool.input?.todos)) {
    return (
      <div class="tool todo">
        <div class="row-btn static">
          <Icon size={14} />
          <span class="label">Plan</span>
        </div>
        <TodoList todos={tool.input.todos} />
      </div>
    )
  }

  const isAgent = tool.name === 'Task' || tool.name === 'Agent'
  return (
    <div class={cls('tool', open && 'open', tool.status)}>
      <button class="row-btn" onClick={() => setOpen(!open)}>
        <Icon size={14} />
        <span class="label">{v.label}</span>
        {v.detail && <span class={cls('detail', v.mono && 'mono')}>{v.detail}</span>}
        {elines && <Stats lines={elines} />}
        {isAgent && kids && kids.length > 0 && <span class="count">{kids.length}</span>}
        <span class="st">
          <StatusIcon status={tool.status} />
        </span>
      </button>
      {open && (
        <div class="tool-body">
          {tool.name === 'Bash' && tool.input?.command && <Pre class="cmd" text={'$ ' + tool.input.command} />}
          {elines && <DiffView lines={elines} />}
          {isAgent && tool.input?.prompt && <Markdown text={tool.input.prompt} class="dim small" />}
          {isAgent && kids && kids.length > 0 && (
            <div class="kids">
              {kids.map((k) => (
                <ItemView key={k.id} item={k} cwd={cwd} />
              ))}
            </div>
          )}
          {!elines && tool.name !== 'Bash' && !isAgent && tool.input && Object.keys(tool.input).length > 0 && (
            <Pre class="json" text={JSON.stringify(tool.input, null, 2)} />
          )}
          {tool.output &&
            (isAgent ? <Markdown text={tool.output} /> : <Pre text={tool.output} class={tool.status === 'error' ? 'error' : ''} />)}
          {!tool.output && tool.status === 'running' && <div class="dim small pad">Running…</div>}
        </div>
      )}
    </div>
  )
}

function AskDetail({ ask, cwd }: { ask: Ask; cwd: string }) {
  const lines = useMemo(() => {
    if (ask.bodyKind === 'diff' && ask.body) return parseDiff(ask.body)
    return editLines(ask.tool || '', ask.input)
  }, [ask.body, ask.bodyKind, ask.tool, ask.input])
  if (ask.bodyKind === 'md' && ask.body) return <Markdown text={ask.body} class="plan-body" />
  if (lines) return <DiffView lines={lines} max={400} />
  if (ask.bodyKind === 'code' && ask.body) return <Pre text={ask.body} />
  if (ask.tool === 'Bash' && ask.input?.command) return <Pre class="cmd" text={'$ ' + ask.input.command} />
  if (ask.input && Object.keys(ask.input).length) {
    const v = describeTool(ask.tool || '', ask.input, cwd)
    if (v.detail && Object.keys(ask.input).length <= 2) return <Pre text={v.detail} />
    return <Pre class="json" text={JSON.stringify(ask.input, null, 2)} />
  }
  return null
}

function useReply(sid: string, itemId: string) {
  const [busy, setBusy] = useState<string | null>(null)
  const send = async (body: object, key: string) => {
    setBusy(key)
    try {
      await api('POST', `/api/sessions/${sid}/asks/${itemId}`, body)
    } catch (e) {
      toastError(e)
    } finally {
      setBusy(null)
    }
  }
  return [busy, send] as const
}

function Approval({ item, sid, cwd }: { item: Item; sid: string; cwd: string }) {
  const ask = item.ask!
  const [busy, send] = useReply(sid, item.id)
  const v = describeTool(ask.tool || '', ask.input, cwd)
  const Icon = v.icon
  const plan = ask.tool === 'ExitPlanMode'

  if (ask.state !== 'pending') {
    const denied = /deny|denied|keep planning|declin/i.test(ask.answer || '') || ask.state === 'cancelled'
    return (
      <div class={cls('ask-done', denied && 'denied')}>
        {denied ? <IX size={13} /> : <ICheck size={13} />}
        <span class="label">{ask.state === 'cancelled' ? 'Cancelled' : ask.answer}</span>
        <span class="detail mono">{plan ? 'Plan' : `${v.label} ${v.detail}`}</span>
      </div>
    )
  }
  return (
    <div class={cls('card ask', plan && 'plan')}>
      <div class="card-head">
        <IShield size={15} />
        <span>{plan ? 'Plan ready' : 'Permission needed'}</span>
      </div>
      {!plan && (
        <div class="ask-tool">
          <Icon size={14} />
          <b>{v.label}</b>
          <span class={cls('detail', v.mono && 'mono')}>{v.detail}</span>
        </div>
      )}
      {ask.title && !plan && ask.title !== v.detail && !v.detail.endsWith(ask.title) && <div class="ask-title">{ask.title}</div>}
      <div class="ask-body">
        <AskDetail ask={ask} cwd={cwd} />
      </div>
      <div class="ask-actions">
        {(ask.options || []).map((o) => (
          <button key={o.id} class={cls('btn', o.style)} disabled={!!busy} onClick={() => send({ option: o.id }, o.id)}>
            {busy === o.id ? <Spinner size={12} /> : null}
            {o.label}
          </button>
        ))}
      </div>
      <div class="hint">Or type a message below to decline with instructions.</div>
    </div>
  )
}

function QuestionCard({ item, sid }: { item: Item; sid: string }) {
  const ask = item.ask!
  const qs = ask.questions || []
  const [sel, setSel] = useState<Record<string, string[]>>({})
  const [other, setOther] = useState<Record<string, string>>({})
  const [busy, send] = useReply(sid, item.id)

  if (ask.state !== 'pending') {
    return (
      <div class="ask-done">
        <ICheck size={13} />
        <span class="label">{ask.state === 'cancelled' ? 'Cancelled' : 'Answered'}</span>
        <span class="detail">{ask.answer}</span>
      </div>
    )
  }
  const answers = () => {
    const out: Record<string, string[]> = {}
    for (const q of qs) {
      const v = [...(sel[q.id] || [])]
      if (other[q.id]?.trim()) v.push(other[q.id].trim())
      out[q.id] = v
    }
    return out
  }
  const complete = qs.every((q) => (sel[q.id]?.length || 0) > 0 || other[q.id]?.trim())
  const toggle = (qid: string, label: string, multi?: boolean) => {
    const cur = sel[qid] || []
    const next = multi ? (cur.includes(label) ? cur.filter((x) => x !== label) : [...cur, label]) : cur[0] === label ? [] : [label]
    setSel({ ...sel, [qid]: next })
    if (!multi && next.length) setOther({ ...other, [qid]: '' })
  }
  return (
    <div class="card ask question">
      <div class="card-head">
        <IChevron size={15} />
        <span>{qs.length > 1 ? 'Questions' : 'Question'}</span>
      </div>
      {qs.map((q) => (
        <div key={q.id} class="q">
          {q.header && <div class="chip">{q.header}</div>}
          <div class="q-text">{q.question}</div>
          <div class="q-opts">
            {(q.options || []).map((o) => {
              const on = (sel[q.id] || []).includes(o.label)
              return (
                <button key={o.label} class={cls('q-opt', on && 'on', q.multi && 'multi')} onClick={() => toggle(q.id, o.label, q.multi)}>
                  <span class="mark" />
                  <span class="q-label">{o.label}</span>
                  {o.description && <span class="q-desc">{o.description}</span>}
                </button>
              )
            })}
          </div>
          {(q.other || !q.options?.length) && (
            <input
              class="input"
              type={(q.secret ? 'password' : 'text') as 'text'}
              placeholder={q.options?.length ? 'Other…' : 'Your answer'}
              value={other[q.id] || ''}
              onInput={(e) => {
                const v = (e.target as HTMLInputElement).value
                setOther({ ...other, [q.id]: v })
                if (v && !q.multi) setSel({ ...sel, [q.id]: [] })
              }}
            />
          )}
        </div>
      ))}
      <div class="ask-actions">
        <button class="btn primary" disabled={!complete || !!busy} onClick={() => send({ option: 'answer', answers: answers() }, 'ok')}>
          {busy === 'ok' && <Spinner size={12} />}Submit
        </button>
        <button class="btn" disabled={!!busy} onClick={() => send({ option: 'deny' }, 'deny')}>
          Skip
        </button>
      </div>
    </div>
  )
}

function Plan({ steps, text }: { steps: PlanStep[]; text?: string }) {
  return (
    <div class="tool todo">
      <div class="row-btn static">
        <span class="label">Plan</span>
        {text && <span class="detail">{text}</span>}
      </div>
      <TodoList todos={steps} />
    </div>
  )
}

function ResultLine({ item }: { item: Item }) {
  const r = item.result || {}
  const st = r.status || ''
  const interrupted = st === 'interrupted' || st === 'error_during_execution'
  const bad = (r.isError || (st && st !== 'success' && st !== 'completed')) && !interrupted
  const parts = [duration(r.durationMs), money(r.costUsd), r.turns && r.turns > 1 ? `${r.turns} turns` : ''].filter(Boolean)
  return (
    <div class={cls('result', bad && 'bad')}>
      <span class="line" />
      <span>
        {interrupted ? 'Interrupted' : bad ? `Stopped · ${st.replace(/_/g, ' ')}` : 'Done'}
        {parts.length > 0 && ' · ' + parts.join(' · ')}
      </span>
      <span class="line" />
      {bad && item.text && <Pre class="error" text={item.text} />}
    </div>
  )
}

interface ItemProps {
  item: Item
  kids?: Item[]
  sid?: string
  cwd: string
}

export const ItemView = memo(function ItemView({ item, kids, sid = '', cwd }: ItemProps) {
  switch (item.kind) {
    case 'user':
      return <UserMsg item={item} />
    case 'text':
      return item.text ? <Markdown text={item.text} class="msg-text" /> : null
    case 'thinking':
      return <Thinking item={item} />
    case 'tool':
      // These are shown as question / plan cards instead.
      if (!item.tool || item.tool.name === 'AskUserQuestion' || item.tool.name === 'ExitPlanMode') return null
      return <ToolCall item={item} kids={kids} cwd={cwd} />
    case 'approval':
      return item.ask ? <Approval item={item} sid={sid} cwd={cwd} /> : null
    case 'question':
      return item.ask ? <QuestionCard item={item} sid={sid} /> : null
    case 'plan':
      return <Plan steps={item.plan || []} text={item.text} />
    case 'notice':
      return <div class="notice">{item.text}</div>
    case 'error':
      return <Pre class="error-box" text={item.text || 'Error'} />
    case 'result':
      return <ResultLine item={item} />
  }
  return null
})

// ---------------------------------------------------------------- list

function Working({ since, status }: { since: number; status: string }) {
  const [, tick] = useState(0)
  useEffect(() => {
    const t = setInterval(() => tick((x) => x + 1), 1000)
    return () => clearInterval(t)
  }, [])
  const label = status === 'starting' ? 'Starting agent' : status === 'waiting' ? 'Waiting for you' : 'Working'
  return (
    <div class={cls('working', status)}>
      <span class="pulse" />
      <span class="shimmer">{label}…</span>
      {since > 0 && <span class="dim">{duration(Date.now() - since)}</span>}
    </div>
  )
}

export function Timeline({ session }: { session: Session }) {
  const tl = timeline(session.id)
  const items = tl.items.value
  const loaded = tl.loaded.value
  const start = tl.start.value
  const scroller = useRef<HTMLDivElement>(null)
  const inner = useRef<HTMLDivElement>(null)
  const stick = useRef(true)
  const prevHeight = useRef<number | null>(null)
  const [showJump, setShowJump] = useState(false)
  const [loadingOlder, setLoadingOlder] = useState(false)

  const { top, kids } = useMemo(() => {
    const kids = new Map<string, Item[]>()
    const top: Item[] = []
    for (const it of items) {
      if (it.parent && tl.idx.has(it.parent)) {
        let k = kids.get(it.parent)
        if (!k) kids.set(it.parent, (k = []))
        k.push(it)
      } else top.push(it)
    }
    return { top, kids }
  }, [items])

  const lastUserTs = useMemo(() => {
    for (let i = items.length - 1; i >= 0; i--) if (items[i].kind === 'user') return items[i].ts
    return 0
  }, [items])

  // New session opened: jump to the end.
  useLayoutEffect(() => {
    stick.current = true
    const el = scroller.current
    if (el) el.scrollTop = el.scrollHeight
  }, [session.id, loaded])

  useLayoutEffect(() => {
    const el = scroller.current
    if (!el) return
    if (prevHeight.current !== null) {
      el.scrollTop += el.scrollHeight - prevHeight.current
      prevHeight.current = null
      return
    }
    if (stick.current) el.scrollTop = el.scrollHeight
    else setShowJump(true)
  }, [items])

  useEffect(() => {
    const el = scroller.current
    const box = inner.current
    if (!el || !box) return
    const ro = new ResizeObserver(() => {
      if (stick.current) el.scrollTop = el.scrollHeight
    })
    ro.observe(box)
    return () => ro.disconnect()
  }, [])

  const onScroll = () => {
    const el = scroller.current!
    const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 60
    stick.current = atBottom
    if (atBottom) setShowJump(false)
  }

  const loadOlder = async () => {
    setLoadingOlder(true)
    try {
      const r = await api<{ items: Item[]; start: number }>('GET', `/api/sessions/${session.id}/items?before=${start}&limit=300`)
      prevHeight.current = scroller.current?.scrollHeight ?? null
      tl.prepend(r.items, r.start)
    } catch (e) {
      toastError(e)
    } finally {
      setLoadingOlder(false)
    }
  }

  const busy = session.status === 'running' || session.status === 'starting'

  return (
    <div class="timeline" ref={scroller} onScroll={onScroll}>
      <div class="timeline-inner" ref={inner}>
        {start > 0 && (
          <button class="btn ghost small older" onClick={loadOlder} disabled={loadingOlder}>
            {loadingOlder ? <Spinner size={12} /> : null} Load earlier messages
          </button>
        )}
        {!loaded && (
          <div class="center dim pad">
            <Spinner />
          </div>
        )}
        {loaded && top.length === 0 && !busy && <div class="empty-chat dim">Send a message to start.</div>}
        {top.map((it) => (
          <div key={it.id} class={'it it-' + it.kind}>
            <ItemView item={it} kids={kids.get(it.id)} sid={session.id} cwd={session.cwd} />
          </div>
        ))}
        {(busy || session.status === 'waiting') && <Working since={lastUserTs} status={session.status} />}
      </div>
      {showJump && (
        <button
          class="jump"
          onClick={() => {
            stick.current = true
            setShowJump(false)
            const el = scroller.current!
            el.scrollTo({ top: el.scrollHeight, behavior: 'smooth' })
          }}
        >
          <IDown size={16} />
        </button>
      )}
    </div>
  )
}
