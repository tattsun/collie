import { useState } from 'preact/hooks'
import { api } from '../api'
import { go, groupById, groups, me, projects, route, toastError, userName, users } from '../store'
import { cls } from '../util'
import { IBack, IPlus, ITrash, IUsers, Spinner } from './icons'

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
        <button class="icon-btn back" onClick={() => go('#/')} aria-label="Back">
          <IBack size={20} />
        </button>
        <h1>Groups</h1>
      </div>
      <p class="dim small">Members of a group share its projects and the sessions started in them (unless made private). Each member’s agents still run with their own accounts.</p>
      <form class="row gap form-inline" onSubmit={create}>
        <input class="input mono" value={name} placeholder="new group name (a-z, 0-9, -)" onInput={(e) => setName((e.target as HTMLInputElement).value)} />
        <button class="btn primary" disabled={!name.trim() || busy}>
          {busy ? <Spinner size={12} /> : <IPlus size={14} />} Create
        </button>
      </form>
      <div class="plist">
        {groups.value.map((g) => (
          <a key={g.id} href={`#/groups/${g.id}`} class="card prow-card">
            <IUsers size={18} />
            <div class="grow">
              <div class="b">{g.name}</div>
              <div class="dim small">
                {Object.keys(g.members).length} member(s){g.myRole ? ` · you are ${g.myRole}` : ' · (admin view)'}
              </div>
            </div>
          </a>
        ))}
        {groups.value.length === 0 && <div class="dim pad">You are not in any group yet.</div>}
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
        <p>Group not found.</p>
        <button class="btn" onClick={() => go('#/groups')}>
          Groups
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
        <button class="icon-btn back" onClick={() => go('#/groups')} aria-label="Back">
          <IBack size={20} />
        </button>
        <h1>{g.name}</h1>
        <span class="grow" />
        <button class="btn primary small" onClick={() => go(`#/projects?new=1&group=${id}`)}>
          <IPlus size={14} /> Group project
        </button>
      </div>
      <section class="card col gap">
        <h3>Members</h3>
        {Object.entries(g.members).map(([uid, role]) => (
          <div key={uid} class="row gap list-row">
            <span class="grow">
              {userName(uid)} {uid === me.value?.id && <span class="chip">you</span>}
            </span>
            {manage ? (
              <select class="select tiny" value={role} disabled={busy} onChange={(e) => run(() => api('POST', `/api/groups/${id}/members`, { user: uid, role: (e.target as HTMLSelectElement).value }))}>
                <option value="member">member</option>
                <option value="owner">owner</option>
              </select>
            ) : (
              <span class="dim small">{role}</span>
            )}
            {(manage || uid === me.value?.id) && (
              <button
                class="icon-btn"
                aria-label={uid === me.value?.id ? 'Leave' : 'Remove'}
                disabled={busy}
                onClick={() => confirm(uid === me.value?.id ? `Leave ${g.name}?` : `Remove ${userName(uid)} from ${g.name}?`) && run(() => api('DELETE', `/api/groups/${id}/members/${uid}`))}
              >
                <ITrash size={14} />
              </button>
            )}
          </div>
        ))}
        {manage && candidates.length > 0 && (
          <div class="row gap">
            <select class="select" value={add} onChange={(e) => setAdd((e.target as HTMLSelectElement).value)}>
              <option value="">Add a member…</option>
              {candidates.map((u) => (
                <option key={u.id} value={u.id}>
                  {u.displayName} ({u.name})
                </option>
              ))}
            </select>
            <button class="btn" disabled={!add || busy} onClick={() => run(async () => (await api('POST', `/api/groups/${id}/members`, { user: add, role: 'member' }), setAdd('')))}>
              Add
            </button>
          </div>
        )}
      </section>
      <section class="card col gap">
        <h3>Projects</h3>
        {gprojects.length === 0 && <div class="dim small">No group projects yet.</div>}
        {gprojects.map((p) => (
          <a key={p.id} href={`#/p/${p.id}`} class={cls('list-row', 'link-row')}>
            {p.name}
          </a>
        ))}
      </section>
      {manage && (
        <div class="danger-zone">
          <button class="btn ghost danger small" disabled={busy || gprojects.length > 0} title={gprojects.length ? 'Delete its projects first' : ''} onClick={() => confirm(`Delete group ${g.name}?`) && run(async () => (await api('DELETE', `/api/groups/${id}`), go('#/groups')))}>
            <ITrash size={14} /> Delete group
          </button>
        </div>
      )}
    </div>
  )
}
