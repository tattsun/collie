import { useEffect, useRef, useState } from 'preact/hooks'
import type { NetEntry, Project } from '../types'
import { api } from '../api'
import { sandbox, toast, toastError } from '../store'
import { ago, cls } from '../util'
import { IAlert, ICheck, IGlobe, IX, Spinner } from './icons'
import { Segmented, rich } from './ui'
import { t } from '../i18n'

type Mode = '' | 'allowlist'

const lines = (s: string) =>
  s
    .split('\n')
    .map((l) => l.trim())
    .filter(Boolean)

const NoFirewall = () => (
  <p class="warn small">
    <IAlert size={12} /> t('This Mac’s Podman machine has no firewall from Collie, so containers on the internet can reach your LAN and this Mac. Run deploy/setup-mac.sh, or use an allowlist.')
  </p>
)

/** What an isolated project's containers can connect to: the internet, or only an allowlist. */
export function NetworkSettings({ p, manager, active }: { p: Project; manager: boolean; active: boolean }) {
  const sb = sandbox.value
  const listed = p.network === 'allowlist'
  const saved = (p.netAllow || []).join('\n')
  const [mode, setMode] = useState<Mode>(p.network || '')
  const [text, setText] = useState(saved)
  const [busy, setBusy] = useState(false)
  const [recent, setRecent] = useState<NetEntry[] | null>(null)
  const prevSaved = useRef(saved)

  useEffect(() => setMode(p.network || ''), [p.network])
  // Follow saved changes (another device, an Allow tap) unless there are edits.
  useEffect(() => {
    setText((cur) => (lines(cur).join('\n') === prevSaved.current ? saved : cur))
    prevSaved.current = saved
  }, [saved])
  useEffect(() => {
    if (!manager || !listed) return
    const load = () =>
      api<{ recent: NetEntry[] }>('GET', `/api/projects/${p.id}/network`)
        .then((r) => setRecent(r.recent))
        .catch(() => {})
    load()
    const timer = setInterval(load, 5000)
    return () => clearInterval(timer)
  }, [p.id, manager, listed])

  if (!manager) {
    return (
      <section class="card">
        <h3>
          <IGlobe size={16} /> {t('Network')}
        </h3>
        <p class="dim small">
          {listed
            ? t('Only the hosts on this project’s allowlist ({n}) and the agents’ own services.', { n: p.netAllow?.length || 0 })
            : t('The internet, but not your LAN, tailnet or this server.')}{' '}
          {t('The project’s owner manages this setting.')}
        </p>
        {!listed && sb.noFirewall && <NoFirewall />}
      </section>
    )
  }

  const modeChanged = mode !== (p.network || '')
  const changed = modeChanged || (mode === 'allowlist' && lines(text).join('\n') !== saved)
  const pick = (m: Mode) => {
    setMode(m)
    if (m === 'allowlist' && !text.trim()) setText((sb.netSuggested || []).join('\n'))
  }
  const save = async () => {
    if (modeChanged && active && !confirm(t('Agents in this project are working. Saving restarts them in the new environment. Continue?'))) return
    setBusy(true)
    try {
      await api('PATCH', `/api/projects/${p.id}`, mode === 'allowlist' ? { network: mode, netAllow: lines(text) } : { network: mode })
      toast(modeChanged ? t('Saved — the project’s containers restart with the new network') : t('Saved — applies right away'))
    } catch (e) {
      toastError(e)
    } finally {
      setBusy(false)
    }
  }
  const allow = async (host: string) => {
    if (!lines(text).includes(host)) setText((cur) => (cur.trim() ? cur.replace(/\s*$/, '\n') : '') + host)
    try {
      await api('PATCH', `/api/projects/${p.id}`, { netAllow: [...(p.netAllow || []), host] })
      toast(t('Allowed {host}', { host }))
    } catch (e) {
      toastError(e)
    }
  }

  return (
    <section class="card">
      <h3>
        <IGlobe size={16} /> {t('Network')}
      </h3>
      <Segmented<Mode>
        value={mode}
        onChange={pick}
        options={[
          { value: '', label: t('Internet') },
          { value: 'allowlist', label: t('Allowlist only') },
        ]}
      />
      <p class="dim small">
        {mode === 'allowlist'
          ? t('Agents and terminals reach only the hosts below and the agents’ own services, through Collie’s proxy. Tools that use the HTTPS_PROXY variable work (curl, git over https, npm, pip, Go, browsers); SSH and other direct connections can’t get out.')
          : t('Agents and terminals can reach any host on the internet, but not your LAN, tailnet or this server.')}
      </p>
      {mode !== 'allowlist' && sb.noFirewall && <NoFirewall />}
      {mode === 'allowlist' && (
        <>
          <textarea
            class="input mono env-edit"
            rows={8}
            spellcheck={false}
            autocapitalize="off"
            value={text}
            placeholder={'github.com\n*.githubusercontent.com\nregistry.npmjs.org'}
            onInput={(e) => setText((e.target as HTMLTextAreaElement).value)}
          />
          <p class="dim small">
            {rich(t('One per line: {a} (that host), {b} (its subdomains), {c} (one port), or an IP address or range. Private addresses, such as a NAS, can be reached only when listed by address.'), {
              a: <code>example.com</code>,
              b: <code>*.example.com</code>,
              c: <code>example.com:443</code>,
            })}
          </p>
          <details class="small dim">
            <summary>{t('Always allowed: the agents’ own services')}</summary>
            <span class="mono">{(sb.netAgents || []).join('  ')}</span>
          </details>
        </>
      )}
      {(mode === 'allowlist' || listed) && <p class="dim small">{t('Changes to the list apply right away. Switching between Internet and an allowlist restarts the project’s containers.')}</p>}
      <div class="modal-actions">
        <button class="btn primary" disabled={!changed || busy} onClick={save}>
          {busy && <Spinner size={12} />} {t('Save')}
        </button>
      </div>
      {listed && (
        <>
          <h4 class="net-h">{t('Recent connections')}</h4>
          {recent === null ? (
            <Spinner />
          ) : recent.length === 0 ? (
            <p class="dim small">{t('None since Collie started.')}</p>
          ) : (
            <div>
              {recent.slice(0, 40).map((e) => (
                <div key={`${e.host}:${e.port}:${e.blocked || ''}`} class="row gap list-row">
                  <span class={cls('net-mark', e.blocked ? 'err' : 'ok')}>{e.blocked ? <IX size={13} /> : <ICheck size={13} />}</span>
                  <span class="grow small mono ellipsis">
                    {e.host}
                    {e.port !== 443 && e.port !== 80 && <span class="dim">:{e.port}</span>}
                  </span>
                  <span class="dim small">
                    {e.count > 1 && `×${e.count} · `}
                    {ago(e.last)}
                  </span>
                  {e.blocked === 'list' && (
                    <button class="btn small" onClick={() => allow(e.host)}>
                      {t('Allow')}
                    </button>
                  )}
                  {e.blocked === 'address' && <span class="dim small">{t('private address')}</span>}
                </div>
              ))}
            </div>
          )}
        </>
      )}
    </section>
  )
}
