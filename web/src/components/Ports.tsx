import { useEffect, useState } from 'preact/hooks'
import type { Session } from '../types'
import { api } from '../api'
import { me, projectById, sessionById, toastError } from '../store'
import { t, ts } from '../i18n'
import { IExternal, IGlobe, Spinner } from './icons'

/** A listening port, with the sessions working in the folder its server
 * runs in (none: not known) and whether that is a worktree or the
 * project's folder (which sessions without a worktree share). */
type Port = { port: number; addr: string; process?: string; sessions: string[]; folder?: 'worktree' | 'project' }
type Ports = { enabled: boolean; shared: boolean; slots: number; running: boolean; ports: Port[]; error?: string }

/** Whose a server is, as a label for a session other than this one. */
function whose(p: Port): string {
  if (!p.sessions.length) return p.folder === 'project' ? t('Project folder') : t('Unknown')
  const titles = p.sessions.map((id) => sessionById.value.get(id)?.title || t('Untitled session'))
  return titles.length > 1 ? t('{title} +{n}', { title: titles[0], n: titles.length - 1 }) : titles[0]
}

/** The servers listening in the session's container (or on the server, for
 * a host-run session), each opened in a new tab on Collie's preview origin. */
export function PortsView({ s }: { s: Session }) {
  const [data, setData] = useState<Ports | null>(null)
  const [err, setErr] = useState('')
  const [manual, setManual] = useState('')
  // When the browser blocked the new tab: a link to tap instead.
  const [link, setLink] = useState<{ port: number; url: string } | null>(null)

  useEffect(() => {
    let stop = false
    let timer = 0
    const load = async () => {
      try {
        const d = await api<Ports>('GET', `/api/sessions/${s.id}/ports`)
        if (!stop) {
          setData(d)
          setErr('')
        }
      } catch (e) {
        if (!stop) setErr((e as Error).message)
      }
      if (!stop) timer = window.setTimeout(load, document.hidden ? 15000 : 3000)
    }
    load()
    return () => {
      stop = true
      clearTimeout(timer)
    }
  }, [s.id])

  const open = async (port: number) => {
    setLink(null)
    // Open the tab while the tap still counts: browsers block tabs opened
    // after waiting for the server.
    const w = window.open('', '_blank')
    try {
      const { url } = await api<{ url: string }>('POST', `/api/sessions/${s.id}/preview`, { port })
      if (w && !w.closed) {
        // The preview's page must not reach back to Collie's window.
        w.opener = null
        w.location.href = url
      } else {
        setLink({ port, url })
      }
    } catch (e) {
      w?.close()
      toastError(e)
    }
  }

  const n = Number(manual)
  const manualOK = Number.isInteger(n) && n > 0 && n <= 65535

  if (s.owner && s.owner !== me.value?.id) {
    return <p class="pad dim">{t("Only the session's owner can open its servers.")}</p>
  }
  if (!data) {
    return <div class="center pad">{err ? <p class="err">{ts(err)}</p> : <Spinner />}</div>
  }
  const off = !data.enabled
  const hostRun = projectById.value.get(s.projectId)?.runtime !== 'container'
  const mine = data.ports.filter((p) => p.sessions.includes(s.id))
  const others = data.ports.filter((p) => !p.sessions.includes(s.id) && (p.sessions.length || p.folder === 'project'))
  const unknown = data.ports.filter((p) => !p.sessions.length && p.folder !== 'project')
  const row = (p: Port, label: string) => (
    <li key={p.port}>
      <button class="port-row" disabled={off} onClick={() => open(p.port)}>
        <IGlobe size={16} />
        <span class="port-num mono">{p.port}</span>
        <span class="port-main">
          <span class="port-proc mono">{p.process || ''}</span>
          {label && <span class="port-who">{label}</span>}
        </span>
        <span class="port-open">
          <IExternal size={14} /> {t('Open')}
        </span>
      </button>
    </li>
  )
  const group = (title: string, ports: Port[], label: (p: Port) => string) =>
    ports.length > 0 && (
      <section class="ports-group">
        <h4 class="ports-h">{title}</h4>
        <ul class="ports-list">{ports.map((p) => row(p, label(p)))}</ul>
      </section>
    )
  return (
    <div class="ports">
      {off && (
        <div class="card ports-note">
          <p>{t("Previews aren't set up on this server yet.")}</p>
          <p class="dim small">
            {me.value?.admin
              ? t('Collie serves them on their own port or host name. Behind a reverse proxy or tailscale serve, give it --preview-url (see Setup → Previews in the docs).')
              : t('Ask an admin to set them up.')}
          </p>
        </div>
      )}
      {link && (
        <div class="card ports-note">
          <p class="small">{t('Your browser blocked the new tab. Tap to open port {port}:', { port: link.port })}</p>
          <a class="btn primary" href={link.url} target="_blank" rel="noopener noreferrer" onClick={() => setLink(null)}>
            <IExternal size={14} /> {t('Open port {port}', { port: link.port })}
          </a>
        </div>
      )}
      {!data.running ? (
        <p class="dim ports-empty">{t("The project's container isn't running, so nothing in it is listening. Ask the agent to start the server again.")}</p>
      ) : data.error ? (
        <p class="err ports-empty">{ts(data.error)}</p>
      ) : data.ports.length === 0 ? (
        <p class="dim ports-empty">{t('Nothing is listening yet. Ask the agent to start a dev server (npm run dev, for example); it shows up here.')}</p>
      ) : (
        <>
          {group(t('This session'), mine, (p) => (p.sessions.length > 1 ? t('Folder shared with {n} other sessions', { n: p.sessions.length - 1 }) : ''))}
          {group(t('Other sessions'), others, whose)}
          {group(t('Unknown'), unknown, () => '')}
        </>
      )}
      <form
        class="row gap ports-manual"
        onSubmit={(e) => {
          e.preventDefault()
          if (manualOK) open(n)
        }}
      >
        <input
          class="input mono"
          type="number"
          inputMode="numeric"
          min={1}
          max={65535}
          placeholder={t('Port, e.g. 5173')}
          aria-label={t('Port number')}
          value={manual}
          onInput={(e) => setManual((e.target as HTMLInputElement).value)}
        />
        <button class="btn" type="submit" disabled={off || !manualOK}>
          {t('Open')}
        </button>
      </form>
      <p class="hint ports-hint">
        {t('Opens in a new tab, on an address of its own: the page can’t see your Collie sign-in. Only you can open it.')}
        {data.shared &&
          ' ' +
            (data.slots > 1
              ? t('Up to {n} previews can be open at once in your browser; opening more replaces the one used longest ago.', { n: data.slots })
              : t('Previews share that address in your browser: opening another one replaces this one.'))}
        {hostRun && ' ' + t('This project runs on the server itself, where every host-run agent works as the same user: only servers working in this session’s folder are shown and can be opened.')}
      </p>
    </div>
  )
}
