import { signal } from '@preact/signals'
import { createPasskey, getPasskey } from './webauthn'

export interface AuthStatus {
  passkeys: boolean
  noAuth?: boolean
  rpId?: string
  rpError?: string
  user?: { id: string; name: string; displayName: string; admin?: boolean }
  legacy?: boolean
}

export const authStatus = signal<AuthStatus | null>(null)
export const inviteCode = signal<string | null>(new URLSearchParams(location.search).get('invite'))

async function post(path: string, body?: unknown, raw = false): Promise<any> {
  const r = await fetch(path, {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : raw ? (body as string) : JSON.stringify(body),
  })
  const data = await r.json().catch(() => ({}))
  if (!r.ok) throw new Error(data.error || r.statusText)
  return data
}

export async function fetchAuthStatus(): Promise<AuthStatus> {
  const r = await fetch('/api/auth/status', { credentials: 'same-origin' })
  const st = (await r.json()) as AuthStatus
  authStatus.value = st
  return st
}

export async function signInWithPasskey() {
  const { id, options } = await post('/api/auth/login/begin')
  const cred = await getPasskey(options)
  await post(`/api/auth/login/finish?c=${encodeURIComponent(id)}`, JSON.stringify(cred), true)
}

/** Adds a passkey to the signed-in user (first one: leaves token sign-in). */
export async function addPasskey(name: string) {
  const { id, options } = await post('/api/me/passkeys/begin', { name })
  const cred = await createPasskey(options)
  await post(`/api/me/passkeys/finish?c=${encodeURIComponent(id)}`, JSON.stringify(cred), true)
}

export async function acceptInvite(code: string, name: string, displayName: string, passkey: string) {
  const { id, options } = await post('/api/auth/invite/begin', { code, name, displayName, passkey })
  const cred = await createPasskey(options)
  await post(`/api/auth/invite/finish?c=${encodeURIComponent(id)}`, JSON.stringify(cred), true)
}

export async function signOut() {
  await post('/api/auth/logout').catch(() => {})
  location.href = '/'
}

export function deviceName(): string {
  const ua = navigator.userAgent
  if (/iPhone/.test(ua)) return 'iPhone'
  if (/iPad/.test(ua)) return 'iPad'
  if (/Android/.test(ua)) return 'Android'
  if (/Macintosh/.test(ua)) return 'Mac'
  if (/Windows/.test(ua)) return 'Windows PC'
  return 'This device'
}
