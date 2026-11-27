// Minimal service worker: shows notifications (the page's own, and Web Push
// ones while Collie is closed) and focuses the app when one is tapped.
// It deliberately does not cache anything, so the UI is never stale.
self.addEventListener('install', () => self.skipWaiting())
self.addEventListener('activate', (e) => e.waitUntil(self.clients.claim()))

// What a push says, by its status. This file is not bundled, so the texts
// can't come from src/locales: keep them in line with the page's.
const TEXTS = {
  en: {
    approval: 'Needs your approval',
    question: 'Has a question for you',
    done: 'Finished',
    error: 'Stopped with an error',
    test: 'Test notification: this device gets notifications from Collie.',
  },
  ja: {
    approval: '承認待ちです',
    question: '質問への回答を待っています',
    done: '完了しました',
    error: 'エラーで停止しました',
    test: 'テスト通知です。この端末で Collie の通知を受け取れます。',
  },
}

// The server sends only {sid, project, title, status, lang}: never what was
// said in the session. Every push shows a notification (browsers require it).
self.addEventListener('push', (e) => {
  let d = {}
  try {
    d = (e.data && e.data.json()) || {}
  } catch (_) {
    /* not JSON: show the generic text */
  }
  const texts = TEXTS[d.lang] || TEXTS.en
  const state = texts[d.status] || texts.done
  const title = d.status === 'test' ? 'Collie' : d.title || 'Collie'
  e.waitUntil(
    self.registration.showNotification(title, {
      body: d.project ? `${d.project} · ${state}` : state,
      // One notification per session: a newer state replaces the older.
      tag: d.sid || 'collie-' + (d.status || 'push'),
      renotify: true,
      icon: '/icon-192.png',
      data: { url: d.sid ? `/#/s/${d.sid}` : '/' },
    }),
  )
})

// The browser renewed the subscription: register the new one in place of the old.
self.addEventListener('pushsubscriptionchange', (e) => {
  e.waitUntil(
    (async () => {
      const old = e.oldSubscription
      const sub = e.newSubscription || (old && (await self.registration.pushManager.subscribe(old.options)))
      if (!sub) return
      const j = sub.toJSON()
      await fetch('/api/push/subscriptions', {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ endpoint: j.endpoint, keys: j.keys, replaces: old ? old.endpoint : '' }),
      })
    })().catch(() => {}),
  )
})

self.addEventListener('notificationclick', (e) => {
  e.notification.close()
  const url = (e.notification.data && e.notification.data.url) || '/'
  e.waitUntil(
    self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then((list) => {
      for (const c of list) {
        if ('focus' in c) {
          if ('navigate' in c) c.navigate(url).catch(() => {})
          else c.postMessage({ t: 'open', url }) // the page goes there itself (notify.ts)
          return c.focus()
        }
      }
      return self.clients.openWindow(url)
    }),
  )
})
