import { useEffect, useState } from 'preact/hooks'
import type { ContainerInfo, Project } from '../types'
import { api } from '../api'
import { sandbox, sessions, toast, toastError } from '../store'
import { cls, home } from '../util'
import { IAlert, IBox, IRefresh, Spinner } from './icons'
import { Modal, Segmented } from './ui'

export function ImageLog({ onClose }: { onClose: () => void }) {
  const sb = sandbox.value
  return (
    <Modal title="Agent image" onClose={onClose} wide>
      <div class="kv">
        <span>Image</span>
        <span class="mono small">{sb.image}</span>
        <span>Status</span>
        <span>{sb.building ? 'Building…' : sb.error ? 'Build failed' : sb.ready ? 'Ready' : 'Not built yet'}</span>
      </div>
      {sb.error && <div class="error-box">{sb.error}</div>}
      {sb.log && <pre class="out build-log">{sb.log}</pre>}
      <div class="modal-actions">
        <button class="btn" disabled={sb.building} onClick={() => api('POST', '/api/sandbox/build', { refresh: true }).catch(toastError)}>
          <IRefresh size={14} /> Rebuild (update agents)
        </button>
      </div>
    </Modal>
  )
}

/** Sidebar footer indicator while the agent image builds or after a failure. */
export function ImageIndicator() {
  const sb = sandbox.value
  const [open, setOpen] = useState(false)
  if (!sb.available || (!sb.building && !sb.error)) return null
  return (
    <>
      <button class={cls('foot-btn', sb.error ? 'err' : 'on')} onClick={() => setOpen(true)}>
        {sb.building ? <Spinner size={12} /> : <IAlert size={13} />}
        {sb.building ? 'Building agent image…' : 'Image build failed'}
      </button>
      {open && <ImageLog onClose={() => setOpen(false)} />}
    </>
  )
}

export function ProjectSettings({ p }: { p: Project }) {
  const sb = sandbox.value
  const isolated = p.runtime === 'container'
  const [runtime, setRuntime] = useState<'host' | 'container'>(isolated ? 'container' : 'host')
  const [memory, setMemory] = useState(p.memory || '')
  const [cpus, setCpus] = useState(p.cpus || '')
  const [pids, setPids] = useState(p.pids ? String(p.pids) : '')
  const [env, setEnv] = useState<string | null>(null)
  const [info, setInfo] = useState<ContainerInfo | null>(null)
  const [busy, setBusy] = useState('')
  const [showLog, setShowLog] = useState(false)
  const active = sessions.value.some((s) => s.projectId === p.id && (s.status === 'running' || s.status === 'waiting' || s.status === 'starting'))

  const refresh = () => {
    if (isolated) api<ContainerInfo>('GET', `/api/projects/${p.id}/container`).then(setInfo).catch(() => {})
  }
  useEffect(() => {
    refresh()
    api<{ env: string }>('GET', `/api/projects/${p.id}/env`).then((r) => setEnv(r.env)).catch(() => setEnv(''))
    const t = setInterval(refresh, 5000)
    return () => clearInterval(t)
  }, [p.id, isolated])

  const changed = runtime !== (isolated ? 'container' : 'host') || memory !== (p.memory || '') || cpus !== (p.cpus || '') || pids !== (p.pids ? String(p.pids) : '')

  const save = async () => {
    if (active && !confirm('Agents in this project are working. Saving restarts them in the new environment. Continue?')) return
    setBusy('save')
    try {
      await api('PATCH', `/api/projects/${p.id}`, { runtime, memory, cpus, pids: pids ? Number(pids) : 0 })
      toast('Saved — agents use the new settings on their next message')
    } catch (e) {
      toastError(e)
    } finally {
      setBusy('')
    }
  }
  const saveEnv = async () => {
    setBusy('env')
    try {
      await api('PUT', `/api/projects/${p.id}/env`, { env: env || '' })
      toast('Saved — applies when the container is next started')
    } catch (e) {
      toastError(e)
    } finally {
      setBusy('')
    }
  }
  const stop = async (kill: boolean) => {
    if (kill && !confirm(`Kill everything running in “${p.name}” right now?\n\nAll its agents and shells are terminated immediately. Conversations can be resumed afterwards.`)) return
    setBusy(kill ? 'kill' : 'stop')
    try {
      await api('POST', `/api/projects/${p.id}/stop`, { kill })
      toast(kill ? 'Killed' : 'Stopped')
      refresh()
    } catch (e) {
      toastError(e)
    } finally {
      setBusy('')
    }
  }

  return (
    <div class="page-pad narrow psettings">
      <section class="card">
        <h3>
          <IBox size={16} /> Where agents run
        </h3>
        <Segmented<'host' | 'container'>
          value={runtime}
          onChange={setRuntime}
          options={[
            { value: 'container', label: 'Isolated container', disabled: !sb.available },
            { value: 'host', label: 'Directly on the server' },
          ]}
        />
        <p class="dim small">
          {runtime === 'container'
            ? 'Agents and terminals run in this project’s own rootless container: only this folder is visible, with resource limits and no access to your LAN or tailnet. A runaway agent cannot touch other projects or the server.'
            : 'Agents run as the server user with full access to its files and network. Use only for projects you trust.'}
        </p>
        {!sb.available && <p class="err small">Podman is not installed on the server, so isolation is unavailable.</p>}
        {sb.available && runtime === 'container' && !sb.limits && (
          <p class="warn small">
            <IAlert size={12} /> Resource limits are not enforced on this server: Collie is not running under a systemd user session. Install it with{' '}
            <code>deploy/install.sh</code> (enables lingering) to get memory/CPU/process limits.
          </p>
        )}
        {runtime === 'container' && (
          <div class="limits">
            <label class="field">
              <span class="field-l">Memory</span>
              <input class="input" value={memory} placeholder={`${sb.memory || 'none'} (default)`} onInput={(e) => setMemory((e.target as HTMLInputElement).value)} />
            </label>
            <label class="field">
              <span class="field-l">CPUs</span>
              <input class="input" value={cpus} placeholder={`${sb.cpus || 'none'} (default)`} inputMode="decimal" onInput={(e) => setCpus((e.target as HTMLInputElement).value)} />
            </label>
            <label class="field">
              <span class="field-l">Processes</span>
              <input class="input" value={pids} placeholder={`${sb.pids || 'none'} (default)`} inputMode="numeric" onInput={(e) => setPids((e.target as HTMLInputElement).value)} />
            </label>
          </div>
        )}
        <div class="modal-actions">
          <button class="btn primary" disabled={!changed || !!busy} onClick={save}>
            {busy === 'save' && <Spinner size={12} />} Save
          </button>
        </div>
      </section>

      {isolated && (
        <section class="card">
          <h3>Container</h3>
          <div class="kv">
            <span>State</span>
            <span>{info ? (info.running ? 'Running' : info.exists ? 'Stopped' : 'Not created yet') : '…'}</span>
            {info?.running && (
              <>
                <span>Memory</span>
                <span class="mono">{info.mem}</span>
                <span>CPU</span>
                <span class="mono">{info.cpu}</span>
                <span>Processes</span>
                <span class="mono">{info.pids}</span>
              </>
            )}
            <span>Image</span>
            <span>
              {sb.building ? 'building…' : sb.error ? 'build failed' : sb.ready ? 'ready' : 'built on first use'}{' '}
              <button class="link-btn" onClick={() => setShowLog(true)}>
                details
              </button>
            </span>
          </div>
          <div class="ask-actions">
            <button class="btn" disabled={!!busy} onClick={() => stop(false)}>
              {busy === 'stop' && <Spinner size={12} />} Stop agents & container
            </button>
            <button class="btn danger" disabled={!!busy} onClick={() => stop(true)}>
              {busy === 'kill' && <Spinner size={12} />} Kill now
            </button>
          </div>
          {info?.running && info.limitsEnforced === false && <p class="warn small">Limits are configured but not enforced on this host.</p>}
          <p class="dim small">The container also stops by itself when nothing has used it for a while.</p>
        </section>
      )}

      {isolated && (
        <section class="card">
          <h3>Secrets & environment</h3>
          <p class="dim small">
            KEY=VALUE lines exported only in this project’s container — e.g. <code>GH_TOKEN</code> with a token scoped to this repository so the agent can push.
          </p>
          <textarea
            class="input mono env-edit"
            rows={5}
            spellcheck={false}
            value={env ?? ''}
            placeholder={'GH_TOKEN=github_pat_…'}
            onInput={(e) => setEnv((e.target as HTMLTextAreaElement).value)}
          />
          <div class="modal-actions">
            <button class="btn" disabled={env === null || !!busy} onClick={saveEnv}>
              {busy === 'env' && <Spinner size={12} />} Save
            </button>
          </div>
        </section>
      )}

      {isolated && sb.shared && <SharedProfile path={sb.shared} />}
      {showLog && <ImageLog onClose={() => setShowLog(false)} />}
    </div>
  )
}

function SharedProfile({ path }: { path: string }) {
  const [env, setEnv] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  useEffect(() => {
    api<{ env: string }>('GET', '/api/shared/env').then((r) => setEnv(r.env)).catch(() => setEnv(''))
  }, [])
  const save = async () => {
    setBusy(true)
    try {
      await api('PUT', '/api/shared/env', { env: env || '' })
      toast('Saved — containers pick it up on their next start')
    } catch (e) {
      toastError(e)
    } finally {
      setBusy(false)
    }
  }
  return (
    <section class="card">
      <h3>Shared environment (all isolated projects)</h3>
      <p class="dim small">
        Claude Code login for containers: run <code>claude setup-token</code> (e.g. in this project’s Terminal tab) and add{' '}
        <code>CLAUDE_CODE_OAUTH_TOKEN=…</code> here. Codex uses the server user’s <code>codex login</code>.
      </p>
      <textarea
        class="input mono env-edit"
        rows={5}
        spellcheck={false}
        value={env ?? ''}
        onInput={(e) => setEnv((e.target as HTMLTextAreaElement).value)}
      />
      <div class="modal-actions">
        <button class="btn" disabled={env === null || busy} onClick={save}>
          {busy && <Spinner size={12} />} Save
        </button>
      </div>
      <p class="dim small">
        Shared skills, instructions and MCP servers (Playwright is on by default) live in <code>{home(path)}</code> on the server — see its README.md.
      </p>
    </section>
  )
}
