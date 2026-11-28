import { useEffect, useState } from 'preact/hooks'
import { ContainerUsage, TabLink } from './ui'
import { api } from '../api'
import { t } from '../i18n'
import { back, me, route, toast, toastError } from '../store'
import { ago, cls, until } from '../util'
import { LinkBox } from './Account'
import { EnvEditor, ProfileEditor } from './Editors'
import { PluginsEditor } from './Plugins'
import { AgentImages } from './Images'
import { UpdatesPanel } from './Updates'
import { IBack, IPlus, IRefresh, ITrash, Spinner } from './icons'

interface Overview {
  users: { id: string; name: string; displayName: string; admin?: boolean; disabled?: boolean; passkeys: number; createdAt: number; lastSeen?: number }[]
  invites: { hash: string; admin?: boolean; userId?: string; note?: string; expiresAt: number }[]
  containers: { projectId: string; userId: string; project: string; user: string; running: boolean; outdated?: boolean; mem?: string; cpu?: string; pids?: string; limitsEnforced?: boolean }[]
}

export function AdminPage() {
  const tab = route.value.tab || 'users'
  const [d, setD] = useState<Overview | null>(null)
  const [busy, setBusy] = useState('')
  const [link, setLink] = useState('')
  const load = () => api<Overview>('GET', '/api/admin/overview').then(setD).catch(toastError)
  useEffect(() => {
    load()
  }, [tab])
  if (!me.value?.admin) return <div class="page center-page dim">{t('Admins only.')}</div>
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
      const note = prompt(admin ? t('Who is this admin invite for? (note)') : t('Who is this invite for? (note)')) ?? ''
      const r = await api<{ url: string }>('POST', '/api/admin/invites', { admin, note })
      setLink(r.url)
    })

  return (
    <div class="page page-pad admin">
      <div class="page-head">
        <button class="icon-btn back" onClick={() => back()} aria-label={t('Back')}>
          <IBack size={20} />
        </button>
        <h1>{t('Admin')}</h1>
        <span class="grow" />
        <button class="btn small" onClick={load}>
          <IRefresh size={14} /> {t('Refresh')}
        </button>
      </div>
      <nav class="tabs flat">
        {[
          ['users', t('Users')],
          ['containers', t('Containers')],
          ['profile', t('Global skills & MCP')],
          ['plugins', t('Global plugins')],
          ['env', t('Global environment')],
          ['updates', t('Updates')],
        ].map(([k, label]) => (
          <TabLink key={k} href={`#/admin/${k}`} on={tab === k}>
            {label}
          </TabLink>
        ))}
      </nav>
      {!d && tab !== 'profile' && tab !== 'plugins' && tab !== 'env' && tab !== 'updates' && <Spinner />}

      {d && tab === 'users' && (
        <div class="col gap-l">
          <section class="card col gap">
            <div class="row gap wrap">
              <h3 class="grow">{t('Users')}</h3>
              <button class="btn primary small" onClick={invite(false)} disabled={!!busy}>
                <IPlus size={14} /> {t('Invite member')}
              </button>
              <button class="btn small" onClick={invite(true)} disabled={!!busy}>
                <IPlus size={14} /> {t('Invite admin')}
              </button>
            </div>
            {link && <LinkBox url={link} onClose={() => setLink('')} />}
            <div class="table">
              {d.users.map((u) => (
                <div key={u.id} class={cls('row gap list-row', u.disabled && 'muted')}>
                  <span class="grow">
                    <b>{u.displayName}</b> <span class="mono dim small">{u.name}</span>
                    {u.admin && <span class="chip">{t('admin')}</span>}
                    {u.disabled && <span class="chip">{t('disabled')}</span>}
                    <span class="dim small">
                      {' '}
                      · {t('{n} passkey(s)', { n: u.passkeys })}
                      {u.lastSeen ? ' · ' + t('seen {time}', { time: ago(u.lastSeen) }) : ''}
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
                      if (v === 'delete' && confirm(t('Delete {name}?\n\nTheir personal projects (registrations, sessions, containers) are removed; files on disk stay.', { name: u.name })))
                        run('u', () => api('DELETE', `/api/admin/users/${u.id}`))()
                    }}
                  >
                    <option value="">{t('Actions…')}</option>
                    <option value="admin">{u.admin ? t('Remove admin') : t('Make admin')}</option>
                    <option value="link">{t('Passkey recovery link')}</option>
                    <option value="disable">{u.disabled ? t('Enable') : t('Disable')}</option>
                    {u.id !== me.value?.id && <option value="delete">{t('Delete…')}</option>}
                  </select>
                </div>
              ))}
            </div>
          </section>
          {d.invites.length > 0 && (
            <section class="card col gap">
              <h3>{t('Open invitations & links')}</h3>
              {d.invites.map((i) => (
                <div key={i.hash} class="row gap list-row">
                  <span class="grow small">
                    {i.userId ? t('Device / recovery link') : i.admin ? t('Admin invite') : t('Member invite')}
                    {i.note && ` · ${i.note}`} <span class="dim">· {t('expires in {time}', { time: until(i.expiresAt) })}</span>
                  </span>
                  <button class="icon-btn" aria-label={t('Revoke')} onClick={run('rv', () => api('DELETE', `/api/admin/invites/${i.hash}`))}>
                    <ITrash size={14} />
                  </button>
                </div>
              ))}
            </section>
          )}
        </div>
      )}

      {d && tab === 'containers' && (
        <div class="col gap-l">
          <AgentImages />
          <section class="card col gap">
            <h3>{t('Containers')}</h3>
            <p class="dim small">{t('Every (project, user) pair has its own container. Admins see usage, not content.')}</p>
            {d.containers.length === 0 && <div class="dim small">{t('No containers.')}</div>}
            {d.containers.map((c) => (
              <div key={c.projectId + c.userId} class="row gap list-row">
                <span class={cls('dot', c.running ? 'running' : 'stopped')} />
                <span class="grow small">
                  <b>{c.project || c.projectId}</b> · {c.user}
                  {c.running && (
                    <span>
                      {' · '}
                      <ContainerUsage mem={c.mem} cpu={c.cpu} pids={c.pids} limited={c.limitsEnforced} />
                      {c.limitsEnforced === false && <span class="warn">{' · ' + t('limits NOT enforced')}</span>}
                      {c.outdated && <span class="warn">{' · ' + t('waiting to restart')}</span>}
                    </span>
                  )}
                </span>
                {c.running && (
                  <button
                    class="btn small danger"
                    disabled={!!busy}
                    onClick={() =>
                      confirm(t("Kill {user}'s container for {project}?", { user: c.user, project: c.project })) &&
                      run('k', async () => (await api('POST', '/api/admin/containers/kill', { projectId: c.projectId, userId: c.userId }), toast(t('Killed'))))()
                    }
                  >
                    {t('Kill')}
                  </button>
                )}
              </div>
            ))}
          </section>
        </div>
      )}

      {tab === 'profile' && (
        <section class="card">
          <ProfileEditor scope="global" />
        </section>
      )}
      {tab === 'plugins' && (
        <section class="card col gap">
          <h3>{t('Global plugins')}</h3>
          <p class="dim small">{t('Claude Code plugins installed for every user’s agents in isolated projects. Groups, users and projects can add more, or turn these off for themselves.')}</p>
          <PluginsEditor url="/api/shared/plugins" />
        </section>
      )}
      {tab === 'env' && (
        <section class="card col gap">
          <h3>{t('Global environment')}</h3>
          <p class="dim small">{t('KEY=VALUE lines exported in every user’s containers. Personal and project environments override these.')}</p>
          <EnvEditor url="/api/shared/env" note={t('Saved — applies to agents and shells started from now on')} />
        </section>
      )}
      {tab === 'updates' && <UpdatesPanel />}
    </div>
  )
}
