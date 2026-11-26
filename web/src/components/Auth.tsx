import { useEffect, useState } from 'preact/hooks'
import { acceptInvite, addPasskey, authStatus, deviceName, inviteCode, signInWithPasskey } from '../authflow'
import { login, boot } from '../boot'
import { go, legacyLogin, me } from '../store'
import { passkeyError, passkeysSupported } from '../webauthn'
import { IAlert, IShield, Spinner } from './icons'

function Shell({ children }: { children: any }) {
  return (
    <div class="login">
      <div class="card login-card">
        <img src="/icon.svg" width={48} height={48} alt="" />
        <h1>Collie</h1>
        {children}
      </div>
    </div>
  )
}

export function LoginPage() {
  const st = authStatus.value
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const [token, setToken] = useState('')

  const passkey = async () => {
    setBusy(true)
    setErr('')
    try {
      await signInWithPasskey()
      await boot()
    } catch (e) {
      setErr(passkeyError(e))
    } finally {
      setBusy(false)
    }
  }
  const tokenLogin = async (e: Event) => {
    e.preventDefault()
    setBusy(true)
    setErr('')
    if (!(await login(token.trim()))) setErr('That token did not work.')
    setBusy(false)
  }

  if (st?.passkeys) {
    return (
      <Shell>
        <p class="dim small">Sign in with the passkey on this device (Face ID, fingerprint or security key).</p>
        {st.rpError && <div class="err small">{st.rpError}</div>}
        {!passkeysSupported() && <div class="err small">This browser cannot use passkeys here (HTTPS is required).</div>}
        <button class="btn primary big" onClick={passkey} disabled={busy || !!st.rpError}>
          {busy ? <Spinner size={12} /> : <IShield size={16} />} Sign in with passkey
        </button>
        {err && <div class="err small">{err}</div>}
        <p class="dim small">
          New device? Open “Add another device” from your account on a signed-in device, ask an admin for a link, or run{' '}
          <code>collie link</code> on the server.
        </p>
      </Shell>
    )
  }
  return (
    <Shell>
      <p class="dim small">
        First-time setup: sign in with the access token printed by the server
        <br />
        (<code>collie --print-token</code>), then create your passkey.
      </p>
      <form class="col gap" onSubmit={tokenLogin}>
        <input class="input mono" type="password" autoComplete="current-password" value={token} placeholder="Access token" onInput={(e) => setToken((e.target as HTMLInputElement).value)} autoFocus />
        <button class="btn primary big" disabled={!token || busy}>
          {busy && <Spinner size={12} />} Sign in
        </button>
      </form>
      {err && <div class="err small">{err}</div>}
    </Shell>
  )
}

export function InvitePage() {
  const code = inviteCode.value!
  const [info, setInfo] = useState<any>(null)
  const [err, setErr] = useState('')
  const [name, setName] = useState('')
  const [display, setDisplay] = useState('')
  const [busy, setBusy] = useState(false)
  const st = authStatus.value

  useEffect(() => {
    fetch(`/api/auth/invite?code=${encodeURIComponent(code)}`, { credentials: 'same-origin' })
      .then(async (r) => {
        const d = await r.json()
        if (!r.ok) throw new Error(d.error)
        setInfo(d)
      })
      .catch((e) => setErr(e.message))
  }, [code])

  const done = async () => {
    inviteCode.value = null
    history.replaceState(null, '', '/')
    await boot()
  }
  const submit = async (e: Event) => {
    e.preventDefault()
    setBusy(true)
    setErr('')
    try {
      await acceptInvite(code, name.trim().toLowerCase(), display.trim(), deviceName())
      await done()
    } catch (e) {
      setErr(passkeyError(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Shell>
      {!info && !err && <Spinner />}
      {err && !info && (
        <>
          <div class="err small">{err}</div>
          <button class="btn" onClick={done}>
            Go to sign-in
          </button>
        </>
      )}
      {info && (
        <form class="col gap" onSubmit={submit}>
          {info.kind === 'device' ? (
            <p class="small">
              Add a passkey on this device for <b>{info.user?.displayName}</b> ({info.user?.name}).
            </p>
          ) : (
            <>
              <p class="small">You’ve been invited{info.admin ? ' as an admin' : ''}. Choose your user name and create a passkey on this device.</p>
              <label class="field">
                <span class="field-l">User name</span>
                <input class="input mono" value={name} placeholder="alice" autoCapitalize="off" autoCorrect="off" spellcheck={false} onInput={(e) => setName((e.target as HTMLInputElement).value)} />
                <span class="field-h">a-z, 0-9, - and _ · also the name of your project folder</span>
              </label>
              <label class="field">
                <span class="field-l">Display name</span>
                <input class="input" value={display} placeholder="Alice" onInput={(e) => setDisplay((e.target as HTMLInputElement).value)} />
              </label>
            </>
          )}
          {st?.rpError && <div class="err small">{st.rpError}</div>}
          <button class="btn primary big" disabled={busy || (info.kind === 'new' && !name.trim()) || !!st?.rpError}>
            {busy ? <Spinner size={12} /> : <IShield size={16} />} Create passkey
          </button>
          {err && <div class="err small">{err}</div>}
        </form>
      )}
    </Shell>
  )
}

/** Shown while signed in with the setup token: Collie is not protected by
 * passkeys yet. */
export function PasskeyBanner() {
  const [busy, setBusy] = useState(false)
  if (!legacyLogin.value) return null
  const st = authStatus.value
  const setup = async () => {
    setBusy(true)
    try {
      await addPasskey(deviceName())
      legacyLogin.value = false
      await boot()
      go('#/account')
    } catch (e) {
      alert(passkeyError(e))
    } finally {
      setBusy(false)
    }
  }
  return (
    <div class="banner warn">
      <IAlert size={15} />
      <span class="grow">
        Collie is not protected by passkeys yet{me.value ? ` (signed in as ${me.value.name} with the setup token)` : ''}.
        {st?.rpError ? ` ${st.rpError}` : ' Create one to turn token sign-in off.'}
      </span>
      <button class="btn small primary" onClick={setup} disabled={busy || !!st?.rpError || !passkeysSupported()}>
        {busy && <Spinner size={12} />} Create passkey
      </button>
    </div>
  )
}
