import { useEffect, useState } from 'preact/hooks'
import { api } from '../api'
import { go, me, route, toast, toastError } from '../store'
import { ago, cls, until } from '../util'
import { LinkBox } from './Account'
import { EnvEditor, ProfileEditor } from './Editors'
import { ImageLog } from './ProjectSettings'
import { IBack, IPlus, IRefresh, ITrash, Spinner } from './icons'

interface Overview {
  users: { id: string; name: string; displayName: string; admin?: boolean; disabled?: boolean; passkeys: number; createdAt: number; lastSeen?: number }[]
  invites: { hash: string; admin?: boolean; userId?: string; note?: string; expiresAt: number }[]
  containers: { projectId: string; userId: string; project: string; user: string; running: boolean; mem?: string; cpu?: string; pids?: string; limitsEnforced?: boolean }[]
}

export function AdminPage() {
  const tab = route.value.tab || 'users'
  const [d, setD] = useState<Overview | null>(null)
  const [busy, setBusy] = useState('')
  const [link, setLink] = useState('')
  const [showImage, setShowImage] = useState(false)
  const load = () => api<Overview>('GET', '/api/admin/overview').then(setD).catch(toastError)
  useEffect(() => {
    load()
  }, [tab])
  if (!me.value?.admin) return <div class="page center-page dim">Admins only.</div>
  const run = (key: string, fn: () => Promise<unknown>) => async () => {
    setBusy(key)
    try {
      await fn()
      await load()
    } catch (e) {
      toastError(e)
    } finally {
      setBusy('')
    }
  }
  const invite = (admin: boolean) =>
    run('inv', async () => {
      const note = prompt(admin ? 'Who is this admin invite for? (note)' : 'Who is this invite for? (note)') ?? ''
      const r = await api<{ url: string }>('POST', '/api/admin/invites', { admin, note })
      setLink(r.url)
    })

  return (
    <div class="page page-pad admin">
      <div class="page-head">
        <button class="icon-btn back" onClick={() => go('#/')} aria-label="Back">
          <IBack size={20} />
        </button>
        <h1>Admin</h1>
        <span class="grow" />
        <button class="btn small" onClick={load}>
          <IRefresh size={14} /> Refresh
        </button>
      </div>
      <nav class="tabs flat">
        {[
          ['users', 'Users'],
          ['containers', 'Containers'],
          ['profile', 'Global skills & MCP'],
          ['env', 'Global environment'],
        ].map(([k, label]) => (
          <a key={k} href={`#/admin/${k}`} class={cls('tab', tab === k && 'on')}>
            {label}
          </a>
        ))}
      </nav>
      {!d && tab !== 'profile' && tab !== 'env' && <Spinner />}

      {d && tab === 'users' && (
        <div class="col gap-l">
          <section class="card col gap">
            <div class="row gap wrap">
              <h3 class="grow">Users</h3>
              <button class="btn primary small" onClick={invite(false)} disabled={!!busy}>
                <IPlus size={14} /> Invite member
              </button>
              <button class="btn small" onClick={invite(true)} disabled={!!busy}>
                <IPlus size={14} /> Invite admin
              </button>
            </div>
            {link && <LinkBox url={link} onClose={() => setLink('')} />}
            <div class="table">
              {d.users.map((u) => (
                <div key={u.id} class={cls('row gap list-row', u.disabled && 'muted')}>
                  <span class="grow">
                    <b>{u.displayName}</b> <span class="mono dim small">{u.name}</span>
                    {u.admin && <span class="chip">admin</span>}
                    {u.disabled && <span class="chip">disabled</span>}
                    <span class="dim small">
                      {' '}
                      · {u.passkeys} passkey(s){u.lastSeen ? ` · seen ${ago(u.lastSeen)}` : ''}
                    </span>
                  </span>
                  <select
                    class="select tiny"
                    value=""
                    disabled={!!busy}
                    onChange={(e) => {
                      const v = (e.target as HTMLSelectElement).value
                      ;(e.target as HTMLSelectElement).value = ''
                      if (v === 'admin') run('u', () => api('PATCH', `/api/admin/users/${u.id}`, { admin: !u.admin }))()
                      if (v === 'disable') run('u', () => api('PATCH', `/api/admin/users/${u.id}`, { disabled: !u.disabled }))()
                      if (v === 'link')
                        run('u', async () => {
                          const r = await api<{ url: string }>('POST', `/api/admin/users/${u.id}/link`)
                          setLink(r.url)
                        })()
                      if (v === 'delete' && confirm(`Delete ${u.name}?\n\nTheir personal projects (registrations, sessions, containers) are removed; files on disk stay.`))
                        run('u', () => api('DELETE', `/api/admin/users/${u.id}`))()
                    }}
                  >
                    <option value="">Actions…</option>
                    <option value="admin">{u.admin ? 'Remove admin' : 'Make admin'}</option>
                    <option value="link">Passkey recovery link</option>
                    <option value="disable">{u.disabled ? 'Enable' : 'Disable'}</option>
                    {u.id !== me.value?.id && <option value="delete">Delete…</option>}
                  </select>
                </div>
              ))}
            </div>
          </section>
          {d.invites.length > 0 && (
            <section class="card col gap">
              <h3>Open invitations & links</h3>
              {d.invites.map((i) => (
                <div key={i.hash} class="row gap list-row">
                  <span class="grow small">
                    {i.userId ? 'Device / recovery link' : i.admin ? 'Admin invite' : 'Member invite'}
                    {i.note && ` · ${i.note}`} <span class="dim">· expires in {until(i.expiresAt)}</span>
                  </span>
                  <button class="icon-btn" aria-label="Revoke" onClick={run('rv', () => api('DELETE', `/api/admin/invites/${i.hash}`))}>
                    <ITrash size={14} />
                  </button>
                </div>
              ))}
            </section>
          )}
        </div>
      )}

      {d && tab === 'containers' && (
        <section class="card col gap">
          <div class="row gap">
            <h3 class="grow">Containers</h3>
            <button class="btn small" onClick={() => setShowImage(true)}>
              Agent image…
            </button>
          </div>
          <p class="dim small">Every (project, user) pair has its own container. Admins see usage, not content.</p>
          {d.containers.length === 0 && <div class="dim small">No containers.</div>}
          {d.containers.map((c) => (
            <div key={c.projectId + c.userId} class="row gap list-row">
              <span class={cls('dot', c.running ? 'running' : 'stopped')} />
              <span class="grow small">
                <b>{c.project || c.projectId}</b> · {c.user}
                {c.running && (
                  <span class="dim mono">
                    {' '}
                    · {c.mem} · {c.cpu} · {c.pids} procs{c.limitsEnforced === false ? ' · limits NOT enforced' : ''}
                  </span>
                )}
              </span>
              {c.running && (
                <button
                  class="btn small danger"
                  disabled={!!busy}
                  onClick={() =>
                    confirm(`Kill ${c.user}'s container for ${c.project}?`) &&
                    run('k', async () => (await api('POST', '/api/admin/containers/kill', { projectId: c.projectId, userId: c.userId }), toast('Killed')))()
                  }
                >
                  Kill
                </button>
              )}
            </div>
          ))}
        </section>
      )}

      {tab === 'profile' && (
        <section class="card">
          <ProfileEditor scope="global" />
        </section>
      )}
      {tab === 'env' && (
        <section class="card col gap">
          <h3>Global environment</h3>
          <p class="dim small">KEY=VALUE lines exported in every user’s containers. Personal and project environments override these.</p>
          <EnvEditor url="/api/shared/env" note="Saved — containers pick it up on their next start" />
        </section>
      )}
      {showImage && <ImageLog onClose={() => setShowImage(false)} />}
    </div>
  )
}
