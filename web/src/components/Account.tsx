import { rich } from './ui'
import { useEffect, useState } from 'preact/hooks'
import { api, saveClaudeToken } from '../api'
import { addPasskey, deviceName, signOut } from '../authflow'
import { go, me, route, sandbox, toast, toastError } from '../store'
import { ago, cls, copyText } from '../util'
import { lang, setLang, t, type Lang } from '../i18n'
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

/** Renders a translated sentence, replacing {key} placeholders with elements. */

export function LinkBox({ url, onClose }: { url: string; onClose: () => void }) {
  return (
    <div class="card link-box">
      <div class="row gap">
        <input class="input mono small grow" readOnly value={url} onFocus={(e) => (e.target as HTMLInputElement).select()} />
        <button class="btn small" onClick={() => copyText(url).then((ok) => toast(ok ? t('Copied') : t('Copy failed')))}>
          {t('Copy')}
        </button>
        <button class="icon-btn" onClick={onClose} aria-label={t('Close')}>
          <IX size={14} />
        </button>
      </div>
      <p class="dim small">{t('One-time link. Open it on the other device; anyone with it can use it until it expires.')}</p>
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
      toast(t('Passkey added'))
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
    if (!claude.trim()) return
    setBusy('claude')
    try {
      await saveClaudeToken(claude)
      setClaude('')
      toast(t('Saved — used by your agents from their next start'))
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
        <button class="icon-btn back" onClick={() => go('#/')} aria-label={t('Back')}>
          <IBack size={20} />
        </button>
        <h1>{me.value?.displayName || t('Account')}</h1>
        <span class="grow" />
        <button class="btn small" onClick={signOut}>
          {t('Sign out')}
        </button>
      </div>
      <nav class="tabs flat">
        <a href="#/account" class={cls('tab', tab === 'account' && 'on')}>
          {t('Account')}
        </a>
        <a href="#/account/agents" class={cls('tab', tab === 'agents' && 'on')}>
          {t('Agent accounts')}
        </a>
        <a href="#/account/profile" class={cls('tab', tab === 'profile' && 'on')}>
          {t('My skills & MCP')}
        </a>
      </nav>

      {!info && <Spinner />}

      {info && tab === 'account' && (
        <div class="col gap-l">
          <section class="card col gap">
            <h3>{t('Profile')}</h3>
            <div class="kv">
              <span>{t('User name')}</span>
              <span class="mono">{info.user.name}</span>
              <span>{t('Role')}</span>
              <span>{info.user.admin ? t('Administrator') : t('Member')}</span>
            </div>
            <label class="field">
              <span class="field-l">{t('Display name')}</span>
              <div class="row gap">
                <input class="input" value={display} onInput={(e) => setDisplay((e.target as HTMLInputElement).value)} />
                <button class="btn" disabled={display === info.user.displayName || !!busy} onClick={act('name', () => api('PATCH', '/api/me', { displayName: display }))}>
                  {t('Save')}
                </button>
              </div>
            </label>
            <label class="field">
              <span class="field-l">{t('Language')}</span>
              <select class="select" value={lang.value} onChange={(e) => setLang((e.target as HTMLSelectElement).value as Lang)}>
                <option value="en">English</option>
                <option value="ja">日本語</option>
              </select>
            </label>
          </section>

          <section class="card col gap">
            <h3>
              <IShield size={15} /> {t('Passkeys')}
            </h3>
            {info.passkeys.length === 0 && <p class="warn small">{t('No passkey yet — create one to protect your account.')}</p>}
            {info.passkeys.map((p) => (
              <div key={p.id} class="row gap list-row">
                <span class="grow">
                  <b>{p.name}</b>
                  <span class="dim small">
                    {' · '}
                    {t('added {time} ago', { time: ago(p.createdAt) })}
                    {p.lastUsed ? ' · ' + t('used {time} ago', { time: ago(p.lastUsed) }) : ''}
                  </span>
                </span>
                <button
                  class="icon-btn"
                  aria-label={t('Rename')}
                  onClick={() => {
                    const n = prompt(t('Name'), p.name)
                    if (n) act('rn', () => api('PATCH', `/api/me/passkeys/${encodeURIComponent(p.id)}`, { name: n }))()
                  }}
                >
                  ✎
                </button>
                <button
                  class="icon-btn"
                  aria-label={t('Delete')}
                  onClick={() => confirm(t('Delete passkey “{name}”?', { name: p.name })) && act('del', () => api('DELETE', `/api/me/passkeys/${encodeURIComponent(p.id)}`))()}
                >
                  <ITrash size={14} />
                </button>
              </div>
            ))}
            <div class="row gap wrap">
              <button class="btn primary" onClick={add} disabled={!!busy || !passkeysSupported()}>
                {busy === 'add' ? <Spinner size={12} /> : <IPlus size={14} />} {t('Add a passkey on this device')}
              </button>
              <button
                class="btn"
                disabled={!!busy}
                onClick={act('link', async () => {
                  const r = await api<{ url: string }>('POST', '/api/me/link')
                  setLink(r.url)
                })}
              >
                {t('Add another device…')}
              </button>
            </div>
            {link && <LinkBox url={link} onClose={() => setLink('')} />}
          </section>

          <section class="card col gap">
            <h3>{t('Signed-in devices')}</h3>
            {info.sessions.map((s, i) => (
              <div key={i} class="row gap list-row">
                <span class="grow">
                  {s.agent}
                  {s.current && <span class="chip">{t('this device')}</span>}
                  <span class="dim small">
                    {' · '}
                    {t('since {time}', { time: ago(s.createdAt) })}
                    {' · '}
                    {t('active {time}', { time: ago(s.lastSeen) })}
                  </span>
                </span>
              </div>
            ))}
            {info.sessions.length > 1 && (
              <button class="btn small" onClick={act('out', () => api('POST', '/api/me/signout-others'))}>
                {t('Sign out all other devices')}
              </button>
            )}
          </section>
        </div>
      )}

      {info && tab === 'agents' && (
        <div class="col gap-l">
          <section class="card col gap">
            <h3>
              Claude Code {info.claudeToken ? <span class="chip ok-chip"><ICheck size={11} /> {t('token set')}</span> : <span class="chip">{t('not set')}</span>}
            </h3>
            <p class="dim small">
              {rich(t('Your agents use your own Claude account. Run {cmd} in any project’s Terminal tab, sign in with your Claude account, and paste the token here.'), { cmd: <code>claude setup-token</code> })}
            </p>
            <div class="row gap">
              <input class="input mono" type="password" value={claude} placeholder="sk-ant-oat01-…" onInput={(e) => setClaude((e.target as HTMLInputElement).value)} />
              <button class="btn primary" disabled={!claude.trim() || !!busy} onClick={saveClaude}>
                {t('Save')}
              </button>
            </div>
          </section>
          <section class="card col gap">
            <h3>
              Codex {info.codexLoggedIn ? <span class="chip ok-chip"><ICheck size={11} /> {t('logged in')}</span> : <span class="chip">{t('not logged in')}</span>}
            </h3>
            <p class="dim small">
              {rich(t('Open any of your isolated projects → Terminal, run {cmd} and follow the link. The login is kept for all of your projects.'), { cmd: <code>codex login --device-auth</code> })}
            </p>
          </section>
          <section class="card col gap">
            <h3>{t('My environment')}</h3>
            <p class="dim small">
              {rich(t('KEY=VALUE lines exported in all of your containers (e.g. {var} for pushing, which also lets you clone private GitHub repositories). Applies to agents and shells started from now on.'), { var: <code>GH_TOKEN</code> })}
            </p>
            <EnvEditor url="/api/me/env" placeholder={'GH_TOKEN=github_pat_…'} />
          </section>
        </div>
      )}

      {tab === 'profile' && (
        <section class="card">{sandbox.value.available ? <ProfileEditor scope="me" /> : <p class="dim">{t('Isolation is not available on this server.')}</p>}</section>
      )}
    </div>
  )
}
