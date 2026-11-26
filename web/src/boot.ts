import { t } from './i18n'
import { api, ApiError } from './api'
import { applyState, authed, connect, toast } from './store'
import { fetchAuthStatus, inviteCode } from './authflow'

/** Legacy token sign-in (only before the first passkey exists). */
export async function login(token: string): Promise<boolean> {
  const r = await fetch('/api/login', {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ token }),
  }).catch(() => null)
  if (!r || !r.ok) return false
  await boot()
  return authed.value === true
}

export async function boot() {
  try {
    const st = await fetchAuthStatus()
    if (!st.user) {
      authed.value = false
      return
    }
    applyState(await api('GET', '/api/state'))
    authed.value = true
    connect()
  } catch (e) {
    if (e instanceof ApiError && e.status === 401) {
      authed.value = false
      return
    }
    toast(t('Cannot reach the server — retrying…'), 'error')
    setTimeout(boot, 3000)
  }
}

export async function start() {
  const params = new URLSearchParams(location.search)
  // Opening the URL printed by the server (…/?token=…) signs the owner in
  // during setup (before passkeys exist).
  const token = params.get('token')
  if (token) {
    history.replaceState(null, '', location.pathname + location.hash)
    if (await login(token)) return
  }
  if (inviteCode.value) {
    await fetchAuthStatus().catch(() => {})
    authed.value = false
    return
  }
  await boot()
}
