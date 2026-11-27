// Notifications on this device. While Collie is open, the page shows them
// itself from the live updates (store.ts). For while it is closed, the
// browser is registered for Web Push: the server sends, public/sw.js shows.
import { signal } from '@preact/signals'
import { api } from './api'
import { lang, t, ts } from './i18n'
import { notifyEnabled, pushID, toast } from './store'

/** Why this device can't get notifications while Collie is closed ('' = it can). */
export type PushBlock = '' | 'insecure' | 'ios-install' | 'ios-old' | 'unsupported'

function isIOS(): boolean {
  // iPadOS presents itself as a Mac, but Macs have no touch screen.
  return /iPhone|iPad|iPod/.test(navigator.userAgent) || (navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1)
}

function installed(): boolean {
  return matchMedia('(display-mode: standalone)').matches || (navigator as any).standalone === true
}

export function pushBlock(): PushBlock {
  if (!window.isSecureContext) return 'insecure'
  const ok = 'serviceWorker' in navigator && 'PushManager' in window && 'Notification' in window
  // iOS offers Web Push only to web apps added to the Home Screen (16.4+).
  if (isIOS() && !ok) return installed() ? 'ios-old' : 'ios-install'
  return ok ? '' : 'unsupported'
}

/** The last failure registering this device, shown on the account page. */
export const pushError = signal('')

function errText(e: unknown): string {
  return ts(e instanceof Error ? e.message : String(e))
}

function fromB64url(s: string): Uint8Array<ArrayBuffer> {
  const bin = atob(s.replace(/-/g, '+').replace(/_/g, '/'))
  const out = new Uint8Array(bin.length)
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i)
  return out
}

function sameKey(sub: PushSubscription, key: Uint8Array): boolean {
  const k = sub.options.applicationServerKey
  if (!k) return false
  const a = new Uint8Array(k)
  return a.length === key.length && a.every((v, i) => v === key[i])
}

/** The service worker, or an error if it doesn't come up (it never does on plain HTTP). */
function worker(): Promise<ServiceWorkerRegistration> {
  return Promise.race([
    navigator.serviceWorker.ready,
    new Promise<never>((_, reject) => setTimeout(() => reject(new Error(t('The service worker did not start. Reload the page and try again.'))), 10_000)),
  ])
}

let registeredLang = ''

/** Registers this browser for Web Push (notification permission is granted). */
async function subscribe() {
  const { key } = await api<{ key: string }>('GET', '/api/push')
  const appKey = fromB64url(key)
  const reg = await worker()
  let sub = await reg.pushManager.getSubscription()
  if (sub && !sameKey(sub, appKey)) {
    // Made for another server key (e.g. Collie's data was reset).
    await sub.unsubscribe().catch(() => {})
    sub = null
  }
  if (!sub) sub = await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: appKey })
  const j = sub.toJSON()
  const l = lang.peek()
  const r = await api<{ id: string }>('POST', '/api/push/subscriptions', { endpoint: j.endpoint, keys: j.keys, lang: l })
  registeredLang = l
  pushError.value = ''
  pushID.value = r.id
}

async function unsubscribe() {
  pushID.value = ''
  pushError.value = ''
  const reg = await navigator.serviceWorker?.getRegistration().catch(() => undefined)
  const sub = await reg?.pushManager?.getSubscription().catch(() => null)
  if (!sub) return
  await api('POST', '/api/push/unsubscribe', { endpoint: sub.endpoint }).catch(() => {})
  await sub.unsubscribe().catch(() => {})
}

/** Turns notifications on this device on or off. Call it from a tap: browsers only ask for permission then. */
export async function setNotify(on: boolean) {
  const block = pushBlock()
  if (on && block === 'ios-install') {
    toast(t('On iPhone and iPad, notifications need Collie on the Home Screen: Share → Add to Home Screen, then open it from there.'), 'error')
    return
  }
  if (on && 'Notification' in window && Notification.permission !== 'granted') {
    const p = await Notification.requestPermission()
    if (p !== 'granted') {
      toast(t('Notifications need HTTPS and permission'), 'error')
      on = false
    }
  }
  notifyEnabled.value = on
  localStorage.setItem('collie.notify', on ? '1' : '0')
  if (!on) return unsubscribe()
  if (block) return // only while Collie is open
  try {
    await subscribe()
  } catch (e) {
    pushError.value = errText(e)
    toast(t('Couldn’t set up notifications for while Collie is closed: {error}', { error: pushError.value }), 'error')
  }
}

/** On start-up: renews this device's registration (it follows the sign-in and the server's key). */
export async function syncPush() {
  if (!notifyEnabled.peek() || pushBlock() || Notification.permission !== 'granted') return
  try {
    await subscribe()
  } catch (e) {
    pushError.value = errText(e)
  }
}

/** Asks the server to push a test notification to this device. */
export async function testPush() {
  const reg = await worker()
  const sub = await reg.pushManager.getSubscription()
  if (!sub) throw new Error(t('This device is not registered for notifications'))
  await api('POST', '/api/push/test', { endpoint: sub.endpoint })
}

// A tapped notification, in browsers where the service worker can't
// navigate the page.
navigator.serviceWorker?.addEventListener('message', (e) => {
  const url = e.data?.t === 'open' && typeof e.data.url === 'string' ? e.data.url : ''
  if (url.startsWith('/#/')) location.hash = url.slice(1)
})

// The texts the service worker shows are in the registered language.
lang.subscribe((l) => {
  if (pushID.peek() && registeredLang && l !== registeredLang) syncPush()
})
