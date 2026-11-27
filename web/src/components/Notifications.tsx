import { useState } from 'preact/hooks'
import { t } from '../i18n'
import { pushBlock, pushError, setNotify, testPush } from '../notify'
import { notifyEnabled, pushID, toast, toastError } from '../store'
import { IBell, ICheck, Spinner } from './icons'

/** Notifications on this device: on/off, a test, and why they can't work here. */
export function NotificationsCard() {
  const [busy, setBusy] = useState('')
  const block = pushBlock()
  const on = notifyEnabled.value
  const denied = 'Notification' in window && Notification.permission === 'denied'
  const run = (key: string, fn: () => Promise<unknown>) => async () => {
    setBusy(key)
    try {
      await fn()
    } catch (e) {
      toastError(e)
    } finally {
      setBusy('')
    }
  }
  const hint: Record<string, string> = {
    'ios-install': t('On iPhone and iPad, notifications need Collie on the Home Screen: Share → Add to Home Screen, then open it from there (iOS 16.4 or later).'),
    'ios-old': t('This iOS version can’t deliver notifications from web apps. Update to iOS 16.4 or later.'),
    insecure: t('Notifications need the HTTPS address of Collie.'),
    unsupported: t('This browser can’t receive notifications while Collie is closed; you get them only while it is open.'),
  }
  return (
    <section class="card col gap">
      <h3>
        <IBell size={15} /> {t('Notifications')}
        {on && pushID.value && (
          <span class="chip ok-chip">
            <ICheck size={11} /> {t('on for this device')}
          </span>
        )}
      </h3>
      <p class="dim small">{t('When a session needs your approval or an answer, finishes, or stops with an error — also while Collie is closed. They show only the project, the session’s title and its state, never what was said.')}</p>
      {block && <p class="warn small">{hint[block]}</p>}
      {denied && <p class="warn small">{t('Notifications for Collie are blocked in this browser’s settings. Allow them there, then turn them on here.')}</p>}
      {on && !block && !pushID.value && pushError.value && (
        <p class="warn small">{t('Couldn’t set up notifications for while Collie is closed: {error}', { error: pushError.value })}</p>
      )}
      <div class="row gap wrap">
        <button
          class={on ? 'btn' : 'btn primary'}
          disabled={!!busy || block === 'ios-install' || block === 'ios-old' || (!on && denied)}
          onClick={run('toggle', () => setNotify(!on))}
        >
          {busy === 'toggle' && <Spinner size={12} />} {on ? t('Turn off on this device') : t('Turn on on this device')}
        </button>
        <button
          class="btn"
          disabled={!!busy || !on || !pushID.value}
          onClick={run('test', async () => {
            await testPush()
            toast(t('Test notification sent. It should arrive in a few seconds.'))
          })}
        >
          {busy === 'test' && <Spinner size={12} />} {t('Send a test notification')}
        </button>
      </div>
    </section>
  )
}
