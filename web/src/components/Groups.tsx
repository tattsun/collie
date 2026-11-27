import { useState } from 'preact/hooks'
import { api } from '../api'
import { t } from '../i18n'
import { go, groupById, groups, me, projects, route, sandbox, toastError, userName, users } from '../store'
import { cls } from '../util'
import { IBack, IPlus, ITrash, IUsers, Spinner } from './icons'
import { PluginsEditor } from './Plugins'

/** Display label of a group role (the value sent to the API stays 'member'/'owner'). */
function roleLabel(role: string) {
  return role === 'owner' ? t('owner') : role === 'member' ? t('member') : role
}

export function GroupsPage() {
  const r = route.value
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  if (r.id) return <GroupDetail id={r.id} />
  const create = async (e: Event) => {
    e.preventDefault()
    setBusy(true)
    try {
      const g = await api<{ id: string }>('POST', '/api/groups', { name: name.trim().toLowerCase() })
      setName('')
      go(`#/groups/${g.id}`)
    } catch (err) {
      toastError(err)
    } finally {
      setBusy(false)
    }
  }
  return (
    <div class="page page-pad narrow">
      <div class="page-head">
        <button class="icon-btn back" onClick={() => go('#/')} aria-label={t('Back')}>
          <IBack size={20} />
        </button>
        <h1>{t('Groups')}</h1>
      </div>
      <p class="dim small">{t('Members of a group share its projects and the sessions started in them (unless made private). Each member’s agents still run with their own accounts.')}</p>
      <form class="row gap form-inline" onSubmit={create}>
        <input class="input mono" value={name} placeholder={t('new group name (a-z, 0-9, -)')} onInput={(e) => setName((e.target as HTMLInputElement).value)} />
        <button class="btn primary" disabled={!name.trim() || busy}>
          {busy ? <Spinner size={12} /> : <IPlus size={14} />} {t('Create')}
        </button>
      </form>
      <div class="plist">
        {groups.value.map((g) => (
          <a key={g.id} href={`#/groups/${g.id}`} class="card prow-card">
            <IUsers size={18} />
            <div class="grow">
              <div class="b">{g.name}</div>
              <div class="dim small">
                {t('{n} member(s)', { n: Object.keys(g.members).length })}
                {g.myRole ? ' · ' + t('you are {role}', { role: roleLabel(g.myRole) }) : ' · ' + t('(admin view)')}
              </div>
            </div>
          </a>
        ))}
        {groups.value.length === 0 && <div class="dim pad">{t('You are not in any group yet.')}</div>}
      </div>
    </div>
  )
}

function GroupDetail({ id }: { id: string }) {
  const g = groupById.value.get(id)
  const [add, setAdd] = useState('')
  const [busy, setBusy] = useState(false)
  if (!g) {
    return (
      <div class="page center-page dim">
        <p>{t('Group not found.')}</p>
        <button class="btn" onClick={() => go('#/groups')}>
          {t('Groups')}
        </button>
      </div>
    )
  }
  const manage = g.myRole === 'owner' || !!me.value?.admin
  const run = async (fn: () => Promise<unknown>) => {
    setBusy(true)
    try {
      await fn()
    } catch (e) {
      toastError(e)
    } finally {
      setBusy(false)
    }
  }
  const candidates = users.value.filter((u) => !(u.id in g.members))
  const gprojects = projects.value.filter((p) => p.ownerGroup === id)
  return (
    <div class="page page-pad narrow">
      <div class="page-head">
        <button class="icon-btn back" onClick={() => go('#/groups')} aria-label={t('Back')}>
          <IBack size={20} />
        </button>
        <h1>{g.name}</h1>
        <span class="grow" />
        <button class="btn primary small" onClick={() => go(`#/projects?new=1&group=${id}`)}>
          <IPlus size={14} /> {t('Group project')}
        </button>
      </div>
      <section class="card col gap">
        <h3>{t('Members')}</h3>
        {Object.entries(g.members).map(([uid, role]) => (
          <div key={uid} class="row gap list-row">
            <span class="grow">
              {userName(uid)} {uid === me.value?.id && <span class="chip">{t('you')}</span>}
            </span>
            {manage ? (
              <select class="select tiny" value={role} disabled={busy} onChange={(e) => run(() => api('POST', `/api/groups/${id}/members`, { user: uid, role: (e.target as HTMLSelectElement).value }))}>
                <option value="member">{t('member')}</option>
                <option value="owner">{t('owner')}</option>
              </select>
            ) : (
              <span class="dim small">{roleLabel(role)}</span>
            )}
            {(manage || uid === me.value?.id) && (
              <button
                class="icon-btn"
                aria-label={uid === me.value?.id ? t('Leave') : t('Remove')}
                disabled={busy}
                onClick={() => confirm(uid === me.value?.id ? t('Leave {group}?', { group: g.name }) : t('Remove {user} from {group}?', { user: userName(uid), group: g.name })) && run(() => api('DELETE', `/api/groups/${id}/members/${uid}`))}
              >
                <ITrash size={14} />
              </button>
            )}
          </div>
        ))}
        {manage && candidates.length > 0 && (
          <div class="row gap">
            <select class="select" value={add} onChange={(e) => setAdd((e.target as HTMLSelectElement).value)}>
              <option value="">{t('Add a member…')}</option>
              {candidates.map((u) => (
                <option key={u.id} value={u.id}>
                  {u.displayName} ({u.name})
                </option>
              ))}
            </select>
            <button class="btn" disabled={!add || busy} onClick={() => run(async () => (await api('POST', `/api/groups/${id}/members`, { user: add, role: 'member' }), setAdd('')))}>
              {t('Add')}
            </button>
          </div>
        )}
      </section>
      {sandbox.value.available && (
        <section class="card col gap">
          <h3>{t('Plugins')}</h3>
          <p class="dim small">{t('Claude Code plugins installed for every member’s agents in this group’s projects.')}</p>
          <PluginsEditor url={`/api/groups/${id}/plugins`} readOnlyNote={t('Group owners can change these.')} />
        </section>
      )}
      <section class="card col gap">
        <h3>{t('Projects')}</h3>
        {gprojects.length === 0 && <div class="dim small">{t('No group projects yet.')}</div>}
        {gprojects.map((p) => (
          <a key={p.id} href={`#/p/${p.id}`} class={cls('list-row', 'link-row')}>
            {p.name}
          </a>
        ))}
      </section>
      {manage && (
        <div class="danger-zone">
          <button class="btn ghost danger small" disabled={busy || gprojects.length > 0} title={gprojects.length ? t('Delete its projects first') : ''} onClick={() => confirm(t('Delete group {name}?', { name: g.name })) && run(async () => (await api('DELETE', `/api/groups/${id}`), go('#/groups')))}>
            <ITrash size={14} /> {t('Delete group')}
          </button>
        </div>
      )}
    </div>
  )
}
