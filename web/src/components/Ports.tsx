import { useEffect, useState } from 'preact/hooks'
import type { Session } from '../types'
import { api } from '../api'
import { me, toastError } from '../store'
import { t, ts } from '../i18n'
import { IExternal, IGlobe, Spinner } from './icons'

type Port = { port: number; addr: string; process?: string }
type Ports = { enabled: boolean; running: boolean; ports: Port[]; error?: string }

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
        <ul class="ports-list">
          {data.ports.map((p) => (
            <li key={p.port}>
              <button class="port-row" disabled={off} onClick={() => open(p.port)}>
                <IGlobe size={16} />
                <span class="port-num mono">{p.port}</span>
                <span class="port-proc mono">{p.process || ''}</span>
                <span class="port-open">
                  <IExternal size={14} /> {t('Open')}
                </span>
              </button>
            </li>
          ))}
        </ul>
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
        {t('Opens in a new tab, on an address of its own: the page can’t see your Collie sign-in. Only you can open it. A browser shows one preview at a time; opening another replaces it.')}
      </p>
    </div>
  )
}
