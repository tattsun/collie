import { useEffect, useRef, useState } from 'preact/hooks'
import { api } from '../api'
import { t, ts } from '../i18n'
import { go, groupById, me, sandbox, sessions, toast, toastError } from '../store'
import type { CustomImageDef, CustomImageStatus, Project, SandboxStatus } from '../types'
import { cls } from '../util'
import { IAlert, IBox, IPlus, IRefresh, ITrash, Spinner } from './icons'
import { Modal, rich } from './ui'

const EXAMPLE = `RUN apt-get update && apt-get install -y --no-install-recommends \\
      openjdk-21-jdk-headless postgresql-client \\
    && rm -rf /var/lib/apt/lists/*
COPY <<'EOF' /usr/local/share/ca-certificates/company.crt
-----BEGIN CERTIFICATE-----
...
-----END CERTIFICATE-----
EOF
RUN update-ca-certificates`

export function ImageLog({ onClose }: { onClose: () => void }) {
  const sb = sandbox.value
  return (
    <Modal title={t('Agent image')} onClose={onClose} wide>
      <div class="kv">
        <span>{t('Image')}</span>
        <span class="mono small">{sb.image}</span>
        <span>{t('Status')}</span>
        <span>{sb.building ? t('Building…') : sb.error ? t('Build failed') : sb.ready ? t('Built') : t('Not built yet')}</span>
      </div>
      {sb.error && <div class="error-box">{ts(sb.error)}</div>}
      {sb.log && <pre class="out build-log">{sb.log}</pre>}
      <div class="modal-actions">
        <button class="btn" disabled={sb.building} onClick={() => api('POST', '/api/sandbox/build', { refresh: true }).catch(toastError)}>
          <IRefresh size={14} /> {t('Rebuild (update agents)')}
        </button>
      </div>
    </Modal>
  )
}

/** Sidebar footer indicator while the agent image builds or after a failure. */
export function ImageIndicator() {
  const sb = sandbox.value
  const [open, setOpen] = useState(false)
  if (!sb.available) return null
  if (!sb.building && !sb.error) {
    // Admins also see their custom images' builds.
    const imgs = me.value?.admin ? sb.images || [] : []
    const building = imgs.some((x) => x.building)
    if (!building && !imgs.some((x) => x.error)) return null
    return (
      <button class={cls('foot-btn', building ? 'on' : 'err')} onClick={() => go('#/admin/containers')}>
        {building ? <Spinner size={12} /> : <IAlert size={13} />}
        {building ? t('Building agent image…') : t('Image build failed')}
      </button>
    )
  }
  return (
    <>
      <button class={cls('foot-btn', sb.error ? 'err' : 'on')} onClick={() => setOpen(true)}>
        {sb.building ? <Spinner size={12} /> : <IAlert size={13} />}
        {sb.building ? t('Building agent image…') : t('Image build failed')}
      </button>
      {open && <ImageLog onClose={() => setOpen(false)} />}
    </>
  )
}

/** What a custom image's state means for the projects that run it. */
export function customImageState(st: CustomImageStatus | undefined): { label: string; warn?: boolean } {
  if (!st) return { label: t('Not built yet') }
  if (st.building) return { label: st.ready ? t('Rebuilding… (projects run the previous build)') : t('Building…') }
  if (st.error) return { label: st.ready ? t('Build failed — projects run the previous build') : t('Build failed'), warn: true }
  if (!st.ready) return { label: t('Not built yet') }
  return { label: st.current ? t('Built') : t('Built (older version)') }
}

function standardState(sb: SandboxStatus) {
  return sb.building ? t('Building…') : sb.error ? t('Build failed') : sb.ready ? t('Built') : t('Not built yet')
}

/** The custom image a project runs (undefined: Collie's standard image). */
export function projectImage(p: Project, sb: SandboxStatus): CustomImageStatus | undefined {
  const imgs = sb.images || []
  if (p.image === 'standard') return undefined
  return imgs.find((x) => x.id === p.image) || imgs.find((x) => x.id === sb.defaultImage)
}

/** Admin → Containers: Collie's image, the custom ones, and the default. */
export function AgentImages() {
  const sb = sandbox.value
  const [edit, setEdit] = useState<string | null>(null) // an image id, '' = new
  const [showStd, setShowStd] = useState(false)
  const [busy, setBusy] = useState(false)
  const imgs = sb.images || []
  const setDefault = async (sel: HTMLSelectElement) => {
    const id = sel.value
    const name = imgs.find((x) => x.id === id)?.name || t('Collie standard')
    const undo = () => (sel.value = sb.defaultImage || '')
    if (!confirm(t('Projects that don’t pick an image, and everyone’s Home, run “{name}” from their next start. Continue?', { name }))) return undo()
    setBusy(true)
    try {
      await api('PATCH', '/api/admin/images', { default: id })
    } catch (e) {
      undo()
      toastError(e)
    } finally {
      setBusy(false)
    }
  }
  return (
    <section class="card col gap">
      <div class="row gap">
        <h3 class="grow">{t('Agent images')}</h3>
        <button class="btn small" disabled={!sb.available} onClick={() => setEdit('')}>
          <IPlus size={14} /> {t('New image')}
        </button>
      </div>
      <p class="dim small">{t('Projects run Collie’s standard image unless you add images of your own on top of it, with what your teams need: packages, your company’s certificates, internal tools. Whoever manages a project picks one in its settings.')}</p>
      <div class="table">
        <div class="row gap list-row">
          <span class="grow small">
            <b>{t('Collie standard')}</b> {!sb.defaultImage && <span class="chip">{t('default')}</span>}
            <span class={cls('dim', sb.error && 'warn')}> · {standardState(sb)}</span>
          </span>
          <button class="btn small" onClick={() => setShowStd(true)}>
            {t('Details')}
          </button>
        </div>
        {imgs.map((im) => {
          const st = customImageState(im)
          return (
            <div key={im.id} class="row gap list-row">
              <span class="grow small">
                <b>{im.name}</b> {sb.defaultImage === im.id && <span class="chip">{t('default')}</span>}
                <span class={cls('dim', st.warn && 'warn')}> · {st.label}</span> {im.building && <Spinner size={11} />}
              </span>
              <button class="btn small" onClick={() => setEdit(im.id)}>
                {t('Edit')}
              </button>
            </div>
          )
        })}
      </div>
      {imgs.length > 0 && (
        <label class="field">
          <span class="field-l">{t('Default image')}</span>
          <select class="select" value={sb.defaultImage || ''} disabled={busy} onChange={(e) => setDefault(e.target as HTMLSelectElement)}>
            <option value="">{t('Collie standard')}</option>
            {imgs.map((im) => (
              // Everyone's Home would wait for its first build.
              <option key={im.id} value={im.id} disabled={!im.ready}>
                {im.name}
              </option>
            ))}
          </select>
        </label>
      )}
      {edit !== null && <ImageEditor id={edit} onClose={() => setEdit(null)} />}
      {showStd && <ImageLog onClose={() => setShowStd(false)} />}
    </section>
  )
}

function ImageEditor({ id, onClose }: { id: string; onClose: () => void }) {
  const [def, setDef] = useState<CustomImageDef | null>(id ? null : { id: '', name: '', containerfile: '' })
  const [name, setName] = useState('')
  const [text, setText] = useState('')
  const [busy, setBusy] = useState('')
  useEffect(() => {
    if (!id) return
    api<{ images: CustomImageDef[] }>('GET', '/api/admin/images')
      .then((r) => {
        const d = r.images.find((x) => x.id === id)
        if (!d) return onClose()
        setDef(d)
        setName(d.name)
        setText(d.containerfile)
      })
      .catch(toastError)
  }, [id])
  const cur = def?.id || ''
  const st = (sandbox.value.images || []).find((x) => x.id === cur)
  const state = customImageState(st)
  // Status pushes come only when a build starts or ends: follow its log.
  const [live, setLive] = useState('')
  useEffect(() => {
    if (!cur || !st?.building) return setLive('')
    const tick = () =>
      api<{ status: CustomImageStatus[] }>('GET', '/api/admin/images')
        .then((r) => setLive(r.status.find((x) => x.id === cur)?.log || ''))
        .catch(() => {})
    tick()
    const timer = setInterval(tick, 2000)
    return () => clearInterval(timer)
  }, [cur, st?.building])
  const log = (st?.building && live) || st?.log
  const logRef = useRef<HTMLPreElement>(null)
  useEffect(() => {
    if (logRef.current) logRef.current.scrollTop = logRef.current.scrollHeight
  }, [log])
  const changed = !!def && (name !== def.name || text !== def.containerfile)

  const act = (key: string, fn: () => Promise<void>) => async () => {
    setBusy(key)
    try {
      await fn()
    } catch (e) {
      toastError(e)
    } finally {
      setBusy('')
    }
  }
  const save = act('save', async () => {
    const d = await api<CustomImageDef>(cur ? 'PUT' : 'POST', cur ? `/api/admin/images/${cur}` : '/api/admin/images', { name, containerfile: text })
    setDef(d)
    setName(d.name)
    setText(d.containerfile)
    toast(t('Saved'))
  })
  const rebuild = act('build', () => api('POST', `/api/admin/images/${cur}/build`))
  const remove = act('delete', async () => {
    if (!confirm(t('Delete the image “{name}”?\n\nProjects that use it switch to the default image the next time they start.', { name: def?.name || '' }))) return
    await api('DELETE', `/api/admin/images/${cur}`)
    onClose()
  })

  return (
    <Modal title={cur ? def?.name || '' : t('New agent image')} onClose={onClose} wide>
      {!def ? (
        <Spinner />
      ) : (
        <div class="col gap">
          <label class="field">
            <span class="field-l">{t('Name')}</span>
            <input class="input" value={name} maxLength={60} placeholder={t('e.g. Android, Data science')} onInput={(e) => setName((e.target as HTMLInputElement).value)} />
          </label>
          <label class="field">
            <span class="field-l">Containerfile</span>
            <textarea class="input mono image-edit" rows={12} spellcheck={false} value={text} placeholder={EXAMPLE} onInput={(e) => setText((e.target as HTMLTextAreaElement).value)} />
          </label>
          <p class="dim small">
            {rich(
              t('Added on top of Collie’s standard image, so no {from} line is needed. The steps run as root while building; agents still run as the server user. A multi-stage build ends with {base}.'),
              { from: <code>FROM</code>, base: <code>{'FROM ${COLLIE_BASE}'}</code> },
            )}
          </p>
          {cur && (
            <div class="kv">
              <span>{t('Status')}</span>
              <span class={cls(state.warn && 'warn')}>
                {state.label} {st?.building && <Spinner size={11} />}
              </span>
            </div>
          )}
          {st?.error && <div class="error-box">{ts(st.error)}</div>}
          {log && (
            <pre ref={logRef} class="out build-log">
              {log}
            </pre>
          )}
          <div class="modal-actions">
            {cur && (
              <button class="btn danger" disabled={!!busy} onClick={remove}>
                <ITrash size={14} /> {t('Delete')}
              </button>
            )}
            <span class="grow" />
            {cur && (
              <button class="btn" disabled={!!busy || st?.building} onClick={rebuild} title={t('Build again without the cache, to get newer packages')}>
                <IRefresh size={14} /> {t('Rebuild')}
              </button>
            )}
            <button class="btn primary" disabled={!changed || !name.trim() || !!busy} onClick={save}>
              {busy === 'save' && <Spinner size={12} />} {cur ? t('Save & build') : t('Create & build')}
            </button>
          </div>
        </div>
      )}
    </Modal>
  )
}

/** Project → Settings: which image the project's containers run. */
export function ProjectImage({ p }: { p: Project }) {
  const sb = sandbox.value
  const imgs = sb.images || []
  const manager = p.ownerUser ? p.ownerUser === me.value?.id : groupById.value.get(p.ownerGroup || '')?.myRole === 'owner'
  const [choice, setChoice] = useState(p.image || '')
  const [busy, setBusy] = useState(false)
  useEffect(() => setChoice(p.image || ''), [p.image])
  if (!imgs.length && !p.image) return null
  const active = sessions.value.some((s) => s.projectId === p.id && (s.status === 'running' || s.status === 'waiting' || s.status === 'starting'))
  const defName = imgs.find((x) => x.id === sb.defaultImage)?.name || t('Collie standard')
  const label = (c: string) => (c === '' ? t('Default ({name})', { name: defName }) : c === 'standard' ? t('Collie standard') : imgs.find((x) => x.id === c)?.name || c)
  const save = async () => {
    if (active && !confirm(t('Agents in this project are working. Saving restarts them in the new environment. Continue?'))) return
    setBusy(true)
    try {
      await api('PATCH', `/api/projects/${p.id}`, { image: choice })
      toast(t('Saved — agents use the new settings on their next message'))
    } catch (e) {
      toastError(e)
    } finally {
      setBusy(false)
    }
  }
  const run = projectImage({ ...p, image: choice }, sb)
  const state = run ? customImageState(run) : { label: standardState(sb), warn: !!sb.error }
  return (
    <section class="card">
      <h3>
        <IBox size={16} /> {t('Agent image')}
      </h3>
      {manager ? (
        <div class="row gap">
          <select class="select" value={choice} disabled={busy} onChange={(e) => setChoice((e.target as HTMLSelectElement).value)}>
            {['', 'standard', ...imgs.map((x) => x.id)].map((c) => (
              <option key={c} value={c}>
                {label(c)}
              </option>
            ))}
          </select>
          <button class="btn primary" disabled={choice === (p.image || '') || busy} onClick={save}>
            {busy && <Spinner size={12} />} {t('Save')}
          </button>
        </div>
      ) : (
        <p class="small">{label(choice)}</p>
      )}
      <p class={cls('small', state.warn ? 'warn' : 'dim')}>
        {run?.name || t('Collie standard')} · {state.label}
      </p>
      <p class="dim small">
        {me.value?.admin
          ? rich(t('Images are defined in {link}.'), { link: <a href="#/admin/containers">{t('Admin → Containers')}</a> })
          : t('Admins define the images; whoever manages the project picks one.')}
      </p>
    </section>
  )
}
