import { useState } from 'preact/hooks'
import { api } from '../api'
import { t, ts } from '../i18n'
import { renderMarkdown } from '../markdown'
import { go, toastError, updateStatus } from '../store'
import type { UpdateStatus } from '../types'
import { ago, cls } from '../util'
import { IRefresh, Spinner } from './icons'
import { Toggle } from './ui'

/** A release's page on GitHub, built here: only ever a github.com link, whatever the server passes on. */
function releaseURL(repo: string, version: string): string {
  const path = repo.split('/').map(encodeURIComponent).join('/')
  return `https://github.com/${path}/releases/tag/v${encodeURIComponent(version)}`
}

function phaseText(phase?: string): string {
  switch (phase) {
    case 'downloading':
      return t('Downloading and verifying…')
    case 'preparing':
      return t('Building the new version’s agent image (can take several minutes)…')
    case 'waiting':
      return t('Will update once nothing is running')
    case 'restarting':
      return t('Restarting — this page reloads by itself')
  }
  return ''
}

/** Admin → Updates. */
export function UpdatesPanel() {
  const u = updateStatus.value
  const [busy, setBusy] = useState('')
  if (!u) return <div class="dim small">{t('Updates are not available on this server.')}</div>

  const run = (key: string, fn: () => Promise<UpdateStatus | void>) => async () => {
    setBusy(key)
    try {
      const st = await fn()
      if (st) updateStatus.value = st
    } catch (e) {
      toastError(e)
    } finally {
      setBusy('')
    }
  }
  const check = run('check', () => api<UpdateStatus>('POST', '/api/admin/update/check'))
  const cancel = run('cancel', async () => void (await api('POST', '/api/admin/update/cancel')))
  const install = () => {
    const msg =
      u.busy > 0
        ? t('{n} running session(s) or terminal(s) will be interrupted (conversations resume with the next message). Update to v{version} now?', { n: u.busy, version: u.latest })
        : t('Collie restarts for a moment. Update to v{version} now?', { version: u.latest })
    if (confirm(msg)) run('apply', () => api<UpdateStatus>('POST', '/api/admin/update/apply'))()
  }
  const setAuto = (on: boolean) => run('auto', () => api<UpdateStatus>('PATCH', '/api/admin/update/settings', { auto: on }))()
  const inFlight = u.phase === 'downloading' || u.phase === 'preparing'

  return (
    <div class="col gap-l">
      <section class="card col gap">
        <div class="row gap wrap">
          <h3 class="grow">{t('Version')}</h3>
          <button class="btn small" onClick={check} disabled={!!busy || !u.enabled}>
            {busy === 'check' ? <Spinner size={12} /> : <IRefresh size={14} />} {t('Check now')}
          </button>
        </div>
        <div class="kv">
          <span>{t('This server')}</span>
          <span>
            <span class="mono">v{u.current}</span>
            {u.devBuild && <span class="dim"> · {t('development build')}</span>}
          </span>
          <span>{t('Latest release')}</span>
          <span>
            {u.latest ? (
              <>
                <a class="mono" href={releaseURL(u.repo || '', u.latest)} target="_blank" rel="noopener noreferrer">
                  v{u.latest}
                </a>
                {u.published ? <span class="dim"> · {ago(u.published)}</span> : null}
                {!u.available && <span class="ok"> · {t('up to date')}</span>}
              </>
            ) : (
              <span class="dim">{t('not checked yet')}</span>
            )}
          </span>
          {u.checkedAt ? (
            <>
              <span>{t('Last checked')}</span>
              <span>{ago(u.checkedAt)}</span>
            </>
          ) : null}
        </div>
        {!u.enabled && <p class="dim small">{t('Update checks are turned off (--update-repo off).')}</p>}
        {u.checkError && <div class="error-box">{ts(u.checkError)}</div>}
        {u.last && (
          <p class={cls('small', u.last.ok ? 'ok' : 'err')}>
            {u.last.ok
              ? t('Updated from v{from} to v{to} · {time}', { from: u.last.from, to: u.last.to, time: ago(u.last.at) })
              : t('The update to v{to} failed and Collie went back to v{from} · {time}: {error}', {
                  to: u.last.to,
                  from: u.last.from,
                  time: ago(u.last.at),
                  error: ts(u.last.error || ''),
                })}
          </p>
        )}
      </section>

      {u.available && (
        <section class="card col gap">
          <div class="row gap wrap">
            <h3 class="grow">{t('v{version} is available', { version: u.latest })}</h3>
            {(inFlight || u.phase === 'waiting') && (
              <button class="btn small" onClick={cancel} disabled={!!busy}>
                {t('Cancel')}
              </button>
            )}
            {u.canApply && (!u.phase || u.phase === 'waiting') && (
              <button class="btn primary small" onClick={install} disabled={!!busy}>
                {u.phase === 'waiting' ? t('Update now') : t('Update to v{version}', { version: u.latest })}
              </button>
            )}
          </div>
          {u.phase && (
            <div class="row gap small">
              {u.phase !== 'waiting' && <Spinner size={12} />}
              <span>{phaseText(u.phase)}</span>
            </div>
          )}
          {u.phase === 'waiting' && (
            <p class="dim small">
              {u.busy > 0 ? t('{n} session(s) or terminal(s) in use right now.', { n: u.busy }) : t('Nothing is running; the update starts within about 10 minutes.')}
            </p>
          )}
          {u.error && <div class="error-box">{ts(u.error)}</div>}
          {!u.canApply && (
            <div class="col gap small">
              <p class="warn">{t('This server cannot update itself: {why}', { why: ts(u.why || '') })}</p>
              <p class="dim">{t('To update by hand, see “Update” in docs/setup.md.')}</p>
            </div>
          )}
          {u.notes && <div class="md small" dangerouslySetInnerHTML={{ __html: renderMarkdown(u.notes) }} />}
        </section>
      )}

      <section class="card col gap">
        <h3>{t('Settings')}</h3>
        <Toggle
          checked={u.auto}
          disabled={!!busy || !u.canApply}
          onChange={setAuto}
          label={t('Install updates automatically')}
          hint={t('Off: admins are told about new releases and update with one tap. On: a new release installs itself once no agent has been working and no terminal has been used for 10 minutes.')}
        />
        {u.auto && u.devBuild && <p class="warn small">{t('This server runs a development build, so releases are not installed automatically.')}</p>}
      </section>
    </div>
  )
}

/** Sidebar footer: the version, or for admins a link when a release is out. */
export function VersionBadge({ version }: { version: string }) {
  const u = updateStatus.value
  if (!u || (!u.available && !u.phase)) return <span class="dim small">v{version}</span>
  return (
    <button class="foot-btn on" onClick={() => go('#/admin/updates')} title={t('A new version is available')}>
      {u.phase && <Spinner size={12} />}
      {u.phase ? t('Updating…') : `v${version} → v${u.latest}`}
    </button>
  )
}
