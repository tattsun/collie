import { signal, computed, batch } from '@preact/signals'
import { t, ts } from './i18n'
import type { Caps, GroupInfo, Item, ModeInfo, Project, SandboxStatus, Session, TermInfo, UserInfo } from './types'

// ---------------------------------------------------------------- state

export const authed = signal<boolean | null>(null)
export const conn = signal<'connecting' | 'online' | 'offline'>('connecting')
export const projects = signal<Project[]>([])
export const sessions = signal<Session[]>([])
export const agentCaps = signal<Record<string, Caps>>({})
export const modes = signal<Record<string, ModeInfo[]>>({})
export const agentsAvail = signal<Record<string, boolean>>({})
export const terms = signal<TermInfo[]>([])
export const sandbox = signal<SandboxStatus>({ available: false })
export const root = signal('')
export const rootBase = signal('')
export const version = signal('')
export const me = signal<UserInfo | null>(null)
export const users = signal<UserInfo[]>([])
export const groups = signal<GroupInfo[]>([])
// signed in with the legacy token (no passkey yet): prompt to add one
export const legacyLogin = signal(false)
export const passkeyCount = signal(0)

export const userById = computed(() => new Map(users.value.map((u) => [u.id, u])))
export const groupById = computed(() => new Map(groups.value.map((g) => [g.id, g])))

export function userName(id?: string): string {
  if (!id) return ''
  return userById.value.get(id)?.displayName || '(former member)'
}

/** Owner label of a project: "" for my own, else the group name. */
export function ownerLabel(p: Project): string {
  if (p.ownerGroup) return groupById.value.get(p.ownerGroup)?.name || 'group'
  if (p.ownerUser && p.ownerUser !== me.value?.id) return userName(p.ownerUser)
  return ''
}

export const projectById = computed(() => new Map(projects.value.map((p) => [p.id, p])))
export const sessionById = computed(() => new Map(sessions.value.map((s) => [s.id, s])))

export const attention = computed(() =>
  sessions.value.filter((s) => !s.archived && (s.status === 'waiting' || s.status === 'error' || s.unread)),
)

// ---------------------------------------------------------------- routing

export interface Route {
  name: 'home' | 'new' | 'session' | 'projects' | 'project' | 'account' | 'groups' | 'admin'
  id?: string
  tab?: string
  query: URLSearchParams
}

function parseHash(): Route {
  const h = location.hash.replace(/^#/, '') || '/'
  const [path, qs] = h.split('?')
  const parts = path.split('/').filter(Boolean)
  const query = new URLSearchParams(qs || '')
  if (parts[0] === 's' && parts[1]) return { name: 'session', id: parts[1], tab: parts[2] || 'chat', query }
  if (parts[0] === 'p' && parts[1]) return { name: 'project', id: parts[1], tab: parts[2] || 'sessions', query }
  if (parts[0] === 'projects') return { name: 'projects', query }
  if (parts[0] === 'account') return { name: 'account', tab: parts[1], query }
  if (parts[0] === 'groups') return { name: 'groups', id: parts[1], query }
  if (parts[0] === 'admin') return { name: 'admin', tab: parts[1] || 'users', query }
  if (parts[0] === 'new') return { name: 'new', query }
  return { name: 'home', query }
}

export const route = signal<Route>(parseHash())
addEventListener('hashchange', () => {
  route.value = parseHash()
})
export function go(hash: string) {
  if (location.hash !== hash) location.hash = hash
  else route.value = parseHash()
}

// ---------------------------------------------------------------- toasts

export interface Toast {
  id: number
  text: string
  kind: 'error' | 'info'
}
export const toasts = signal<Toast[]>([])
let toastSeq = 0
export function toast(text: string, kind: Toast['kind'] = 'info') {
  const id = ++toastSeq
  toasts.value = [...toasts.value, { id, text, kind }]
  setTimeout(() => (toasts.value = toasts.value.filter((t) => t.id !== id)), kind === 'error' ? 6000 : 3000)
}
export function toastError(e: unknown) {
  toast(ts(e instanceof Error ? e.message : String(e)), 'error')
}

// ---------------------------------------------------------------- timelines

export class Timeline {
  items = signal<Item[]>([])
  start = signal(0)
  caps = signal<Caps>({})
  loaded = signal(false)
  idx = new Map<string, number>()

  reset(items: Item[], start: number) {
    this.idx.clear()
    items.forEach((it, i) => this.idx.set(it.id, i))
    batch(() => {
      this.items.value = items
      this.start.value = start
      this.loaded.value = true
    })
  }

  prepend(older: Item[], start: number) {
    const cur = this.items.value.filter((it) => !older.some((o) => o.id === it.id))
    this.reset([...older, ...cur], start)
  }
}

const timelines = new Map<string, Timeline>()
export function timeline(sid: string): Timeline {
  let t = timelines.get(sid)
  if (!t) {
    t = new Timeline()
    timelines.set(sid, t)
  }
  return t
}

// Live updates are queued and applied once per animation frame so a fast
// token stream re-renders at most ~60 times a second.
type Op = { t: 'item'; item: Item } | { t: 'delta'; id: string; f: string; d: string }
const pending = new Map<string, Op[]>()
let flushScheduled = false

function scheduleFlush() {
  if (flushScheduled) return
  flushScheduled = true
  const run = () => {
    flushScheduled = false
    flush()
  }
  if (document.hidden) setTimeout(run, 250)
  else requestAnimationFrame(run)
}

function flush() {
  batch(() => {
    for (const [sid, ops] of pending) {
      const tl = timelines.get(sid)
      if (!tl || !tl.loaded.value) continue
      let arr: Item[] | null = null
      for (const op of ops) {
        if (!arr) arr = tl.items.value.slice()
        if (op.t === 'item') {
          const i = tl.idx.get(op.item.id)
          if (i === undefined) {
            tl.idx.set(op.item.id, arr.length)
            arr.push(op.item)
          } else arr[i] = op.item
        } else {
          const i = tl.idx.get(op.id)
          if (i === undefined) continue
          const it = arr[i]
          if (op.f === 'output') {
            if (it.tool) arr[i] = { ...it, tool: { ...it.tool, output: (it.tool.output || '') + op.d } }
          } else arr[i] = { ...it, text: (it.text || '') + op.d }
        }
      }
      if (arr) tl.items.value = arr
    }
    pending.clear()
  })
}

function queue(sid: string, op: Op) {
  let ops = pending.get(sid)
  if (!ops) pending.set(sid, (ops = []))
  ops.push(op)
  scheduleFlush()
}

// ---------------------------------------------------------------- websocket

let ws: WebSocket | null = null
let ready = false
let retry = 0
let retryTimer: number | undefined
let subscribed: string | null = null

export function connect() {
  if (ws && (ws.readyState === WebSocket.OPEN || ws.readyState === WebSocket.CONNECTING)) return
  clearTimeout(retryTimer)
  conn.value = 'connecting'
  ready = false
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
  const sock = new WebSocket(`${proto}//${location.host}/api/ws`)
  ws = sock
  sock.onopen = () => {
    retry = 0
  }
  sock.onmessage = (ev) => onMessage(JSON.parse(ev.data))
  sock.onclose = (ev) => {
    if (ws !== sock) return
    if (ev.code === 4401) {
      // signed out elsewhere / session revoked
      ws = null
      authed.value = false
      return
    }
    ws = null
    ready = false
    conn.value = 'offline'
    retry++
    const delay = Math.min(5000, 300 * 2 ** Math.min(retry, 4))
    retryTimer = window.setTimeout(async () => {
      if (retry > 2) {
        // Maybe the cookie expired: check before hammering the socket.
        const r = await fetch('/api/state', { credentials: 'same-origin' }).catch(() => null)
        if (r && r.status === 401) {
          authed.value = false
          return
        }
      }
      connect()
    }, delay)
  }
}

export function disconnect() {
  const s = ws
  ws = null
  s?.close()
}

document.addEventListener('visibilitychange', () => {
  if (!document.hidden && authed.value) {
    if (!ws) connect()
    else if (pending.size) scheduleFlush()
  }
})
addEventListener('online', () => authed.value && !ws && connect())

function send(msg: object) {
  if (ws && ready) ws.send(JSON.stringify(msg))
}

export function subscribe(sid: string | null) {
  if (subscribed === sid) return
  if (subscribed) send({ t: 'unsub', sid: subscribed })
  subscribed = sid
  if (sid) send({ t: 'sub', sid })
}

export function markSeen(sid: string) {
  send({ t: 'seen', sid })
}

let loadedBuild: string | null = null

export function applyState(d: any) {
  // The server was upgraded since this page loaded: pick up the new UI.
  if (d.build) {
    if (loadedBuild && loadedBuild !== d.build) {
      location.reload()
      return
    }
    loadedBuild = d.build
  }
  batch(() => {
    projects.value = d.projects || []
    sessions.value = d.sessions || []
    agentCaps.value = d.agentCaps || {}
    modes.value = d.modes || {}
    agentsAvail.value = d.agents || {}
    terms.value = d.terms || []
    sandbox.value = d.sandbox || { available: false }
    me.value = d.me || null
    users.value = d.users || []
    groups.value = d.groups || []
    legacyLogin.value = !!d.legacy
    passkeyCount.value = d.passkeys || 0
    root.value = d.root || ''
    rootBase.value = d.rootBase || ''
    version.value = d.version || ''
  })
}

function upsertSession(s: Session) {
  const prev = sessionById.value.get(s.id)
  const list = sessions.value.slice()
  const i = list.findIndex((x) => x.id === s.id)
  if (i >= 0) list[i] = s
  else list.unshift(s)
  sessions.value = list
  maybeNotify(prev, s)
}

function onMessage(m: any) {
  if (m.t === 'init') {
    ready = true
    conn.value = 'online'
    applyState(m)
    if (subscribed) send({ t: 'sub', sid: subscribed })
    return
  }
  if (!ready) return
  switch (m.t) {
    case 'state':
      // users / groups changed: a fresh view
      applyState(m)
      if (subscribed && !sessionById.value.has(subscribed)) subscribed = null
      break
    case 'session':
      upsertSession(m.session)
      break
    case 'session_del':
      sessions.value = sessions.value.filter((s) => s.id !== m.id)
      break
    case 'project': {
      const list = projects.value.slice()
      const i = list.findIndex((x) => x.id === m.project.id)
      if (i >= 0) list[i] = m.project
      else list.unshift(m.project)
      projects.value = list
      break
    }
    case 'project_del':
      projects.value = projects.value.filter((p) => p.id !== m.id)
      break
    case 'agentcaps':
      agentCaps.value = { ...agentCaps.value, [m.agent]: m.caps }
      break
    case 'terms':
      terms.value = m.terms || []
      break
    case 'sandbox':
      sandbox.value = m.sandbox || { available: false }
      break
    case 'snap': {
      pending.delete(m.sid)
      const tl = timeline(m.sid)
      tl.reset(m.items || [], m.start || 0)
      tl.caps.value = m.caps || {}
      break
    }
    case 'caps':
      timeline(m.sid).caps.value = m.caps || {}
      break
    case 'item':
      queue(m.sid, { t: 'item', item: m.item })
      break
    case 'delta':
      queue(m.sid, { t: 'delta', id: m.id, f: m.f, d: m.d })
      break
    case 'suberr':
      toast(ts(m.error), 'error')
      break
  }
}

// ---------------------------------------------------------------- notifications

export const notifyEnabled = signal(localStorage.getItem('collie.notify') === '1')

export async function setNotify(on: boolean) {
  if (on && 'Notification' in window && Notification.permission !== 'granted') {
    const p = await Notification.requestPermission()
    if (p !== 'granted') {
      toast(t('Notifications need HTTPS (e.g. tailscale serve) and permission'), 'error')
      on = false
    }
  }
  notifyEnabled.value = on
  localStorage.setItem('collie.notify', on ? '1' : '0')
}

function maybeNotify(prev: Session | undefined, s: Session) {
  if (!prev || prev.status === s.status) return
  const viewing = route.value.name === 'session' && route.value.id === s.id && !document.hidden
  if (viewing) return
  let body = ''
  if (s.status === 'waiting') body = t('Needs your approval')
  else if (s.status === 'idle' && (prev.status === 'running' || prev.status === 'waiting')) body = s.preview || t('Finished')
  else if (s.status === 'error') body = t('Stopped with an error')
  if (!body) return
  if (notifyEnabled.value && 'Notification' in window && Notification.permission === 'granted') {
    const opts: NotificationOptions = { body, tag: s.id, icon: '/icon-192.png', data: { url: `/#/s/${s.id}` } }
    const fallback = () => {
      try {
        const n = new Notification(s.title, opts)
        n.onclick = () => {
          window.focus()
          go(`#/s/${s.id}`)
          n.close()
        }
      } catch {
        /* ignore */
      }
    }
    // Android/installed PWAs only allow notifications via the service worker.
    if (navigator.serviceWorker?.controller) {
      navigator.serviceWorker.ready.then((r) => r.showNotification(s.title, opts)).catch(fallback)
    } else fallback()
  }
}

// Title badge with the number of sessions needing attention.
attention.subscribe((a) => {
  document.title = a.length ? `(${a.length}) Collie` : 'Collie'
})
