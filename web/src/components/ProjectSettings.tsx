import { useEffect, useState } from 'preact/hooks'
import type { ContainerInfo, Project } from '../types'
import { api } from '../api'
import { groupById, me, sandbox, sessions, toast, toastError } from '../store'
import { ProjectClaudeAccount } from './ClaudeAccounts'
import { NetworkSettings } from './NetworkSettings'
import { EnvEditor } from './Editors'
import { EffectivePlugins, PluginsEditor } from './Plugins'
import { ImageLog, ProjectImage, customImageState, projectImage } from './Images'
import { cls } from '../util'
import { IAlert, IBox, Spinner } from './icons'
import { Segmented, Toggle, rich, ContainerUsage } from './ui'
import { t } from '../i18n'

export function ProjectSettings({ p }: { p: Project }) {
  const sb = sandbox.value
  const admin = !!me.value?.admin
  const manager = p.ownerUser ? p.ownerUser === me.value?.id : groupById.value.get(p.ownerGroup || '')?.myRole === 'owner'
  const isolated = p.runtime === 'container'
  const [runtime, setRuntime] = useState<'host' | 'container'>(isolated ? 'container' : 'host')
  const [memory, setMemory] = useState(p.memory || '')
  const [cpus, setCpus] = useState(p.cpus || '')
  const [pids, setPids] = useState(p.pids ? String(p.pids) : '')
  const [nested, setNested] = useState(!!p.nested)
  const [ctrs, setCtrs] = useState<(ContainerInfo & { userName?: string; mine?: boolean })[] | null>(null)
  const [busy, setBusy] = useState('')
  const [showLog, setShowLog] = useState(false)
  const [pluginsSaved, setPluginsSaved] = useState(0)
  const active = sessions.value.some((s) => s.projectId === p.id && (s.status === 'running' || s.status === 'waiting' || s.status === 'starting'))

  const refresh = () => {
    if (isolated) api<any[]>('GET', `/api/projects/${p.id}/container`).then(setCtrs).catch(() => {})
  }
  useEffect(() => {
    refresh()
    const timer = setInterval(refresh, 5000)
    return () => clearInterval(timer)
  }, [p.id, isolated])

  const changed = runtime !== (isolated ? 'container' : 'host') || memory !== (p.memory || '') || cpus !== (p.cpus || '') || pids !== (p.pids ? String(p.pids) : '') || nested !== !!p.nested

  const save = async () => {
    if (active && !confirm(t('Agents in this project are working. Saving restarts them in the new environment. Continue?'))) return
    setBusy('save')
    try {
      await api('PATCH', `/api/projects/${p.id}`, { runtime, memory, cpus, pids: pids ? Number(pids) : 0, nested: runtime === 'container' && nested })
      toast(t('Saved — agents use the new settings on their next message'))
    } catch (e) {
      toastError(e)
    } finally {
      setBusy('')
    }
  }
  const stop = async (kill: boolean) => {
    if (kill && !confirm(t('Kill everything running in “{name}” right now (all members)?\n\nAgents and shells are terminated immediately. Conversations can be resumed afterwards.', { name: p.name }))) return
    setBusy(kill ? 'kill' : 'stop')
    try {
      await api('POST', `/api/projects/${p.id}/stop`, { kill })
      toast(kill ? t('Killed') : t('Stopped'))
      refresh()
    } catch (e) {
      toastError(e)
    } finally {
      setBusy('')
    }
  }
  const limitsUnenforced = ctrs?.some((c) => c.running && c.limitsEnforced === false)
  const custom = projectImage(p, sb)

  return (
    <div class="page-pad narrow psettings">
      <section class="card">
        <h3>
          <IBox size={16} /> {t('Where agents run')}
        </h3>
        {admin ? (
          <>
            <Segmented<'host' | 'container'>
              value={runtime}
              onChange={setRuntime}
              options={[
                { value: 'container', label: t('Isolated container'), disabled: !sb.available },
                { value: 'host', label: t('Directly on the server') },
              ]}
            />
            <p class="dim small">
              {runtime === 'container'
                ? t('Each member’s agents and terminals run in their own rootless container for this project: only this folder is visible, with resource limits and no access to your LAN or tailnet.')
                : t('Agents run as the server user with full access to its files and network. Use only for projects you trust.')}
            </p>
            {sb.available && runtime === 'container' && !sb.limits && (
              <p class="warn small">
                <IAlert size={12} /> {t('Resource limits are not enforced on this server: Collie is not running under a systemd user session (see deploy/install.sh).')}
              </p>
            )}
            {runtime === 'container' && (
              <div class="limits">
                <label class="field">
                  <span class="field-l">{t('Memory')}</span>
                  <input class="input" value={memory} placeholder={t('{value} (default)', { value: sb.memory || t('none') })} onInput={(e) => setMemory((e.target as HTMLInputElement).value)} />
                </label>
                <label class="field">
                  <span class="field-l">{t('CPUs')}</span>
                  <input class="input" value={cpus} placeholder={t('{value} (default)', { value: sb.cpus || t('none') })} inputMode="decimal" onInput={(e) => setCpus((e.target as HTMLInputElement).value)} />
                </label>
                <label class="field">
                  <span class="field-l">{t('Processes')}</span>
                  <input class="input" value={pids} placeholder={t('{value} (default)', { value: sb.pids || t('none') })} inputMode="numeric" onInput={(e) => setPids((e.target as HTMLInputElement).value)} />
                </label>
              </div>
            )}
            {runtime === 'container' && !p.home && (
              <Toggle
                checked={nested}
                onChange={setNested}
                label={t('Allow containers inside (Podman)')}
                hint={t('For developing container tooling such as Collie itself. The containers keep running as the server user’s ids, but can switch between their own ids and reach more of the kernel (FUSE, tun devices, nested namespaces with their own /proc). Only for projects you trust.')}
              />
            )}
            <div class="modal-actions">
              <button class="btn primary" disabled={!changed || !!busy} onClick={save}>
                {busy === 'save' && <Spinner size={12} />} {t('Save')}
              </button>
            </div>
          </>
        ) : (
          <p class="dim small">
            {isolated
              ? t('Isolated: your agents run in your own container for this project (limits {memory} memory, {cpus} CPUs).', { memory: p.memory || sb.memory || '—', cpus: p.cpus || sb.cpus || '—' })
              : t('Runs directly on the server.')}{' '}
            {isolated && p.nested && t('Podman works inside the containers.') + ' '}
            {t('Admins manage this setting.')}
          </p>
        )}
      </section>

      {isolated && <NetworkSettings p={p} manager={manager} active={active} />}

      {isolated && !p.home && <ProjectImage p={p} />}

      {isolated && (
        <section class="card">
          <h3>{t('Containers')}</h3>
          {!ctrs && <Spinner />}
          {ctrs && ctrs.length === 0 && <p class="dim small">{t('None yet — they start with the first agent or terminal.')}</p>}
          {ctrs?.map((c) => (
            <div key={c.userId} class="row gap list-row">
              <span class={cls('dot', c.running ? 'running' : 'stopped')} />
              <span class="grow small">
                {c.mine ? t('You') : c.userName}
                {c.running ? (
                  <>
                    {' · '}
                    <ContainerUsage mem={c.mem} cpu={c.cpu} pids={c.pids} limited={c.limitsEnforced} />
                  </>
                ) : (
                  <span class="dim"> · {t('stopped')}</span>
                )}
              </span>
            </div>
          ))}
          {limitsUnenforced && <p class="warn small">{t('Limits are configured but not enforced on this host.')}</p>}
          <div class="row gap wrap">
            <span class="dim small grow">
              {t('Image: {status}', {
                status: custom
                  ? `${custom.name} · ${customImageState(custom).label}`
                  : sb.building
                    ? t('building…')
                    : sb.error
                      ? t('build failed')
                      : sb.ready
                        ? t('ready')
                        : t('built on first use'),
              })}
              {admin && custom && (
                <>
                  {' '}
                  <a href="#/admin/containers">{t('details')}</a>
                </>
              )}
              {admin && !custom && (
                <>
                  {' '}
                  <button class="link-btn" onClick={() => setShowLog(true)}>
                    {t('details')}
                  </button>
                </>
              )}
            </span>
          </div>
          <div class="ask-actions">
            <button class="btn" disabled={!!busy} onClick={() => stop(false)}>
              {busy === 'stop' && <Spinner size={12} />} {t('Stop everything')}
            </button>
            <button class="btn danger" disabled={!!busy} onClick={() => stop(true)}>
              {busy === 'kill' && <Spinner size={12} />} {t('Kill now')}
            </button>
          </div>
          <p class="dim small">{t('Containers also stop by themselves when nothing has used them for a while.')}</p>
        </section>
      )}

      {isolated && <ProjectClaudeAccount projectId={p.id} />}

      {isolated && manager && (
        <section class="card">
          <h3>{t('Project secrets')}</h3>
          <p class="dim small">
            {rich(
              p.ownerGroup
                ? t('KEY=VALUE lines exported in this project’s containers (for every member) — e.g. {code} scoped to this repository.')
                : t('KEY=VALUE lines exported in this project’s containers — e.g. {code} scoped to this repository.'),
              { code: <code>GH_TOKEN</code> },
            )}
          </p>
          <EnvEditor url={`/api/projects/${p.id}/env`} placeholder={'GH_TOKEN=github_pat_…'} note={t('Saved — applies to agents and shells started from now on')} />
        </section>
      )}

      {isolated && (
        <section class="card col gap">
          <h3>{t('Plugins')}</h3>
          <p class="dim small">
            {rich(
              p.ownerGroup
                ? t('Claude Code plugins installed for every member’s agents in this project. Plugins enabled in the repository’s {file} are installed too.')
                : t('Claude Code plugins installed for your agents in this project. Plugins enabled in the repository’s {file} are installed too.'),
              { file: <code>.claude/settings.json</code> },
            )}
          </p>
          <PluginsEditor url={`/api/projects/${p.id}/plugins`} readOnlyNote={t('Group owners can change these.')} onSaved={() => setPluginsSaved((n) => n + 1)} />
          <div class="group-h">{t('What your agents get here')}</div>
          <EffectivePlugins projectId={p.id} reload={pluginsSaved} />
          <p class="dim small">{t('Installed into your container before Claude starts; changes apply to agents started from now on.')}</p>
        </section>
      )}

      {isolated && (
        <section class="card">
          <h3>{t('Skills & MCP')}</h3>
          <p class="dim small">
            {rich(
              admin
                ? t('Agents get the global profile (Admin → Global skills & MCP) plus your own ({link}), and whatever the repository itself contains ({dir}, {mcp}, {agents}).')
                : t('Agents get the global profile plus your own ({link}), and whatever the repository itself contains ({dir}, {mcp}, {agents}).'),
              {
                link: <a href="#/account/profile">{t('Account → My skills & MCP')}</a>,
                dir: <code>.claude/</code>,
                mcp: <code>.mcp.json</code>,
                agents: <code>AGENTS.md</code>,
              },
            )}
          </p>
        </section>
      )}
      {showLog && <ImageLog onClose={() => setShowLog(false)} />}
    </div>
  )
}
