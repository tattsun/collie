import { Modal, rich } from './ui'
import { useEffect, useState } from 'preact/hooks'
import { acceptInvite, addPasskey, authStatus, deviceName, inviteCode, reauthRequest, signInWithPasskey, verifyWithPasskey } from '../authflow'
import { login, boot } from '../boot'
import { lang, setLang, t, ts } from '../i18n'
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

/** Renders a translated sentence, replacing {key} placeholders with elements. */

/** Unobtrusive English / 日本語 switch for the sign-in screen. */
function LangToggle() {
  const ja = lang.value === 'ja'
  return (
    <button type="button" class="btn ghost small" style={{ width: 'auto' }} onClick={() => setLang(ja ? 'en' : 'ja')}>
      {ja ? 'English' : '日本語'}
    </button>
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
    if (!(await login(token.trim()))) setErr(t('That token did not work.'))
    setBusy(false)
  }

  if (st?.passkeys) {
    return (
      <Shell>
        <p class="dim small">{t('Sign in with the passkey on this device (Face ID, fingerprint or security key).')}</p>
        {st.rpError && <div class="err small">{ts(st.rpError)}</div>}
        {!passkeysSupported() && <div class="err small">{t('This browser cannot use passkeys here (HTTPS is required).')}</div>}
        <button class="btn primary big" onClick={passkey} disabled={busy || !!st.rpError}>
          {busy ? <Spinner size={12} /> : <IShield size={16} />} {t('Sign in with passkey')}
        </button>
        {err && <div class="err small">{ts(err)}</div>}
        <p class="dim small">
          {rich(t('New device? Open “Add another device” from your account on a signed-in device, ask an admin for a link, or run {cmd} on the server.'), {
            cmd: <code>collie link</code>,
          })}
        </p>
        <LangToggle />
      </Shell>
    )
  }
  return (
    <Shell>
      <p class="dim small">
        {rich(t('First-time setup: sign in with the access token printed by the server{br}({cmd}), then create your passkey.'), {
          br: <br />,
          cmd: <code>collie --print-token</code>,
        })}
      </p>
      <form class="col gap" onSubmit={tokenLogin}>
        <input class="input mono" type="password" autoComplete="current-password" value={token} placeholder={t('Access token')} onInput={(e) => setToken((e.target as HTMLInputElement).value)} autoFocus />
        <button class="btn primary big" disabled={!token || busy}>
          {busy && <Spinner size={12} />} {t('Sign in')}
        </button>
      </form>
      {err && <div class="err small">{ts(err)}</div>}
      <LangToggle />
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
          <div class="err small">{ts(err)}</div>
          <button class="btn" onClick={done}>
            {t('Go to sign-in')}
          </button>
        </>
      )}
      {info && (
        <form class="col gap" onSubmit={submit}>
          {info.kind === 'device' ? (
            <p class="small">
              {rich(t('Add a passkey on this device for {display} ({name}).'), { display: <b>{info.user?.displayName}</b>, name: info.user?.name })}
            </p>
          ) : (
            <>
              <p class="small">
                {info.admin
                  ? t('You’ve been invited as an admin. Choose your user name and create a passkey on this device.')
                  : t('You’ve been invited. Choose your user name and create a passkey on this device.')}
              </p>
              <label class="field">
                <span class="field-l">{t('User name')}</span>
                <input class="input mono" value={name} placeholder="alice" autoCapitalize="off" autoCorrect="off" spellcheck={false} onInput={(e) => setName((e.target as HTMLInputElement).value)} />
                <span class="field-h">{t('a-z, 0-9, - and _ · also the name of your project folder')}</span>
              </label>
              <label class="field">
                <span class="field-l">{t('Display name')}</span>
                <input class="input" value={display} placeholder="Alice" onInput={(e) => setDisplay((e.target as HTMLInputElement).value)} />
              </label>
            </>
          )}
          {st?.rpError && <div class="err small">{ts(st.rpError)}</div>}
          <button class="btn primary big" disabled={busy || (info.kind === 'new' && !name.trim()) || !!st?.rpError}>
            {busy ? <Spinner size={12} /> : <IShield size={16} />} {t('Create passkey')}
          </button>
          {err && <div class="err small">{ts(err)}</div>}
        </form>
      )}
    </Shell>
  )
}

/** Asks for the passkey again before a sensitive change (the server answered
 * "reauth"); the change is retried once confirmed. */
export function VerifyDialog() {
  const req = reauthRequest.value
  return req ? <VerifyPrompt key={req.id} resolve={req.resolve} reject={req.reject} /> : null
}

function VerifyPrompt({ resolve, reject }: { resolve: () => void; reject: (e: Error) => void }) {
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')
  const cancel = () => reject(new Error(t('Not confirmed — nothing was changed.')))
  const confirm = async () => {
    setBusy(true)
    setErr('')
    try {
      await verifyWithPasskey()
      resolve()
    } catch (e) {
      setErr(passkeyError(e))
    } finally {
      setBusy(false)
    }
  }
  return (
    <Modal title={t('Confirm it’s you')} onClose={cancel}>
      <p class="small">{t('Use your passkey to make this change. Collie asks again before changes that could let someone in or reach beyond your own projects.')}</p>
      {err && <div class="err small">{ts(err)}</div>}
      <div class="modal-actions">
        <button class="btn" onClick={cancel}>
          {t('Cancel')}
        </button>
        <button class="btn primary" onClick={confirm} disabled={busy || !passkeysSupported()} autoFocus>
          {busy ? <Spinner size={12} /> : <IShield size={14} />} {t('Use passkey')}
        </button>
      </div>
    </Modal>
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
        {me.value
          ? t('Collie is not protected by passkeys yet (signed in as {name} with the setup token).', { name: me.value.name })
          : t('Collie is not protected by passkeys yet.')}
        {st?.rpError ? ` ${ts(st.rpError)}` : ' ' + t('Create one to turn token sign-in off.')}
      </span>
      <button class="btn small primary" onClick={setup} disabled={busy || !!st?.rpError || !passkeysSupported()}>
        {busy && <Spinner size={12} />} {t('Create passkey')}
      </button>
    </div>
  )
}
