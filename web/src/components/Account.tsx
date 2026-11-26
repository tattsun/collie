import { useEffect, useState } from 'preact/hooks'
import { api } from '../api'
import { addPasskey, deviceName, signOut } from '../authflow'
import { go, me, route, sandbox, toast, toastError } from '../store'
import { ago, cls, copyText } from '../util'
import { passkeyError, passkeysSupported } from '../webauthn'
import { EnvEditor, ProfileEditor } from './Editors'
import { IBack, ICheck, IPlus, IShield, ITrash, IX, Spinner } from './icons'

interface MeInfo {
  user: { id: string; name: string; displayName: string; admin?: boolean }
  passkeys: { id: string; name: string; createdAt: number; lastUsed?: number }[]
  sessions: { current: boolean; createdAt: number; lastSeen: number; agent: string }[]
  codexLoggedIn: boolean
  claudeToken: boolean
}

export function LinkBox({ url, onClose }: { url: string; onClose: () => void }) {
  return (
    <div class="card link-box">
      <div class="row gap">
        <input class="input mono small grow" readOnly value={url} onFocus={(e) => (e.target as HTMLInputElement).select()} />
        <button class="btn small" onClick={() => copyText(url).then((ok) => toast(ok ? 'Copied' : 'Copy failed'))}>
          Copy
        </button>
        <button class="icon-btn" onClick={onClose} aria-label="Close">
          <IX size={14} />
        </button>
      </div>
      <p class="dim small">One-time link. Open it on the other device; anyone with it can use it until it expires.</p>
    </div>
  )
}

export function AccountPage() {
  const tab = route.value.tab || 'account'
  const [info, setInfo] = useState<MeInfo | null>(null)
  const [busy, setBusy] = useState('')
  const [link, setLink] = useState('')
  const [display, setDisplay] = useState('')
  const [claude, setClaude] = useState('')

  const load = () =>
    api<MeInfo>('GET', '/api/me')
      .then((d) => {
        setInfo(d)
        setDisplay(d.user.displayName)
      })
      .catch(toastError)
  useEffect(() => {
    load()
  }, [])

  const add = async () => {
    setBusy('add')
    try {
      await addPasskey(deviceName())
      toast('Passkey added')
      load()
    } catch (e) {
      toastError(passkeyError(e))
    } finally {
      setBusy('')
    }
  }
  const act = (key: string, fn: () => Promise<unknown>) => async () => {
    setBusy(key)
    try {
      await fn()
      load()
    } catch (e) {
      toastError(e)
    } finally {
      setBusy('')
    }
  }
  const saveClaude = async () => {
    const tok = claude.trim()
    if (!tok) return
    setBusy('claude')
    try {
      const cur = (await api<{ env: string }>('GET', '/api/me/env')).env || ''
      const lines = cur.split('\n').filter((l) => !l.trim().startsWith('CLAUDE_CODE_OAUTH_TOKEN='))
      lines.push(`CLAUDE_CODE_OAUTH_TOKEN=${tok}`)
      await api('PUT', '/api/me/env', { env: lines.filter((l, i) => l.trim() || i < lines.length - 1).join('\n') + '\n' })
      setClaude('')
      toast('Saved — used by your agents from their next start')
      load()
    } catch (e) {
      toastError(e)
    } finally {
      setBusy('')
    }
  }

  return (
    <div class="page page-pad narrow account">
      <div class="page-head">
        <button class="icon-btn back" onClick={() => go('#/')} aria-label="Back">
          <IBack size={20} />
        </button>
        <h1>{me.value?.displayName || 'Account'}</h1>
        <span class="grow" />
        <button class="btn small" onClick={signOut}>
          Sign out
        </button>
      </div>
      <nav class="tabs flat">
        <a href="#/account" class={cls('tab', tab === 'account' && 'on')}>
          Account
        </a>
        <a href="#/account/agents" class={cls('tab', tab === 'agents' && 'on')}>
          Agent accounts
        </a>
        <a href="#/account/profile" class={cls('tab', tab === 'profile' && 'on')}>
          My skills & MCP
        </a>
      </nav>

      {!info && <Spinner />}

      {info && tab === 'account' && (
        <div class="col gap-l">
          <section class="card col gap">
            <h3>Profile</h3>
            <div class="kv">
              <span>User name</span>
              <span class="mono">{info.user.name}</span>
              <span>Role</span>
              <span>{info.user.admin ? 'Admin' : 'Member'}</span>
            </div>
            <label class="field">
              <span class="field-l">Display name</span>
              <div class="row gap">
                <input class="input" value={display} onInput={(e) => setDisplay((e.target as HTMLInputElement).value)} />
                <button class="btn" disabled={display === info.user.displayName || !!busy} onClick={act('name', () => api('PATCH', '/api/me', { displayName: display }))}>
                  Save
                </button>
              </div>
            </label>
          </section>

          <section class="card col gap">
            <h3>
              <IShield size={15} /> Passkeys
            </h3>
            {info.passkeys.length === 0 && <p class="warn small">No passkey yet — create one to protect your account.</p>}
            {info.passkeys.map((p) => (
              <div key={p.id} class="row gap list-row">
                <span class="grow">
                  <b>{p.name}</b>
                  <span class="dim small"> · added {ago(p.createdAt)} ago{p.lastUsed ? ` · used ${ago(p.lastUsed)} ago` : ''}</span>
                </span>
                <button
                  class="icon-btn"
                  aria-label="Rename"
                  onClick={() => {
                    const n = prompt('Name', p.name)
                    if (n) act('rn', () => api('PATCH', `/api/me/passkeys/${encodeURIComponent(p.id)}`, { name: n }))()
                  }}
                >
                  ✎
                </button>
                <button
                  class="icon-btn"
                  aria-label="Delete"
                  onClick={() => confirm(`Delete passkey “${p.name}”?`) && act('del', () => api('DELETE', `/api/me/passkeys/${encodeURIComponent(p.id)}`))()}
                >
                  <ITrash size={14} />
                </button>
              </div>
            ))}
            <div class="row gap wrap">
              <button class="btn primary" onClick={add} disabled={!!busy || !passkeysSupported()}>
                {busy === 'add' ? <Spinner size={12} /> : <IPlus size={14} />} Add a passkey on this device
              </button>
              <button
                class="btn"
                disabled={!!busy}
                onClick={act('link', async () => {
                  const r = await api<{ url: string }>('POST', '/api/me/link')
                  setLink(r.url)
                })}
              >
                Add another device…
              </button>
            </div>
            {link && <LinkBox url={link} onClose={() => setLink('')} />}
          </section>

          <section class="card col gap">
            <h3>Signed-in devices</h3>
            {info.sessions.map((s, i) => (
              <div key={i} class="row gap list-row">
                <span class="grow">
                  {s.agent}
                  {s.current && <span class="chip">this device</span>}
                  <span class="dim small"> · since {ago(s.createdAt)} · active {ago(s.lastSeen)}</span>
                </span>
              </div>
            ))}
            {info.sessions.length > 1 && (
              <button class="btn small" onClick={act('out', () => api('POST', '/api/me/signout-others'))}>
                Sign out all other devices
              </button>
            )}
          </section>
        </div>
      )}

      {info && tab === 'agents' && (
        <div class="col gap-l">
          <section class="card col gap">
            <h3>
              Claude Code {info.claudeToken ? <span class="chip ok-chip"><ICheck size={11} /> token set</span> : <span class="chip">not set</span>}
            </h3>
            <p class="dim small">
              Your agents use your own Claude account. Run <code>claude setup-token</code> in any project’s Terminal tab, sign in with your Claude account, and paste the token here.
            </p>
            <div class="row gap">
              <input class="input mono" type="password" value={claude} placeholder="sk-ant-oat01-…" onInput={(e) => setClaude((e.target as HTMLInputElement).value)} />
              <button class="btn primary" disabled={!claude.trim() || !!busy} onClick={saveClaude}>
                Save
              </button>
            </div>
          </section>
          <section class="card col gap">
            <h3>
              Codex {info.codexLoggedIn ? <span class="chip ok-chip"><ICheck size={11} /> logged in</span> : <span class="chip">not logged in</span>}
            </h3>
            <p class="dim small">
              Open any of your isolated projects → Terminal, run <code>codex login --device-auth</code> and follow the link. The login is kept for all of your projects.
            </p>
          </section>
          <section class="card col gap">
            <h3>My environment</h3>
            <p class="dim small">
              KEY=VALUE lines exported in all of your containers (e.g. <code>GH_TOKEN</code> for pushing, which also lets you clone private GitHub repositories). Applies when your containers next start.
            </p>
            <EnvEditor url="/api/me/env" placeholder={'GH_TOKEN=github_pat_…'} />
          </section>
        </div>
      )}

      {tab === 'profile' && (
        <section class="card">{sandbox.value.available ? <ProfileEditor scope="me" /> : <p class="dim">Isolation is not available on this server.</p>}</section>
      )}
    </div>
  )
}
