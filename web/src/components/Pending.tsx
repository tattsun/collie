import { useState } from 'preact/hooks'
import type { Project } from '../types'
import { api } from '../api'
import { me, toast, toastError, userName } from '../store'
import { IAlert, Spinner } from './icons'
import { t } from '../i18n'

/**
 * Says that the project's containers (only `user`'s, if given) still run
 * with older settings or an older image because they were in use, and
 * offers to restart them now.
 */
export function PendingBanner({ p, user }: { p: Project; user?: string }) {
  const [busy, setBusy] = useState(false)
  const pending = (p.pending || []).filter((u) => !user || u === user)
  if (!pending.length) return null
  const others = pending.filter((u) => u !== me.value?.id)
  const whose = !user && others.length > 0 ? pending.map((u) => (u === me.value?.id ? t('You') : userName(u))).join(', ') : ''
  const restart = async () => {
    const q = whose ? t('Restart the containers of {names} now?', { names: whose }) : t('Restart the container now?')
    if (!confirm(q + '\n\n' + t('Agents at work in it stop, and its terminals close, with anything running in them. Conversations resume on the next message.'))) return
    setBusy(true)
    try {
      await api('POST', `/api/projects/${p.id}/restart`, user ? { user } : {})
      toast(t('Restarted — agents use the new settings from their next message'))
    } catch (e) {
      toastError(e)
    } finally {
      setBusy(false)
    }
  }
  return (
    <div class="banner warn pending-banner">
      <IAlert size={15} />
      <span class="grow">
        {t('Changes to the settings or image apply at the next restart, as the container is in use. It restarts by itself once no agent is at work and no terminal is open.')}
        {whose && <span class="dim"> ({whose})</span>}
      </span>
      <button class="btn small" disabled={busy} onClick={restart}>
        {busy && <Spinner size={12} />} {t('Restart now')}
      </button>
    </div>
  )
}
