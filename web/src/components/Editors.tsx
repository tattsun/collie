import { useEffect, useMemo, useState } from 'preact/hooks'
import { api } from '../api'
import { t } from '../i18n'
import { toast, toastError } from '../store'
import type { ProfileFile } from '../types'
import { cls } from '../util'
import { IFile, IFolder, IPlus, ITrash, Spinner } from './icons'

/** KEY=VALUE editor backed by GET/PUT {url} → {env}. */
export function EnvEditor({ url, placeholder, note }: { url: string; placeholder?: string; note?: string }) {
  const [env, setEnv] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  useEffect(() => {
    api<{ env: string }>('GET', url)
      .then((r) => setEnv(r.env))
      .catch(() => setEnv(''))
  }, [url])
  const save = async () => {
    setBusy(true)
    try {
      await api('PUT', url, { env: env || '' })
      toast(note || t('Saved'))
    } catch (e) {
      toastError(e)
    } finally {
      setBusy(false)
    }
  }
  return (
    <div class="col gap">
      <textarea class="input mono env-edit" rows={5} spellcheck={false} value={env ?? ''} placeholder={placeholder} onInput={(e) => setEnv((e.target as HTMLTextAreaElement).value)} />
      <div class="modal-actions">
        <button class="btn" disabled={env === null || busy} onClick={save}>
          {busy && <Spinner size={12} />} {t('Save')}
        </button>
      </div>
    </div>
  )
}

// Labels stay in English here and are translated where rendered.
const QUICK: { path: string; label: string }[] = [
  { path: 'claude/CLAUDE.md', label: 'Claude instructions' },
  { path: 'claude/mcp.json', label: 'Claude MCP servers' },
  { path: 'claude/settings.json', label: 'Claude settings' },
  { path: 'codex/AGENTS.md', label: 'Codex instructions' },
  { path: 'codex/config.toml', label: 'Codex config (MCP…)' },
  { path: 'gitconfig', label: 'gitconfig' },
]

const SKILL_TEMPLATE = (name: string) => `---
name: ${name}
description: What this skill does and when to use it.
---

# ${name}

Instructions for the agent…
`

/** Edits a profile (global = admins, me = personal) file by file. */
export function ProfileEditor({ scope }: { scope: 'global' | 'me' }) {
  const [files, setFiles] = useState<ProfileFile[] | null>(null)
  const [open, setOpen] = useState<string | null>(null)
  const [content, setContent] = useState('')
  const [orig, setOrig] = useState('')
  const [busy, setBusy] = useState(false)
  const base = `/api/profile/${scope}`

  const reload = () => api<ProfileFile[]>('GET', `${base}/files`).then(setFiles).catch(toastError)
  useEffect(() => {
    reload()
  }, [scope])

  const openFile = async (path: string) => {
    try {
      const r = await api<{ content: string }>('GET', `${base}/file?path=${encodeURIComponent(path)}`)
      setOpen(path)
      setContent(r.content)
      setOrig(r.content)
    } catch (e) {
      toastError(e)
    }
  }
  const save = async () => {
    if (!open) return
    setBusy(true)
    try {
      await api('PUT', `${base}/file?path=${encodeURIComponent(open)}`, { content })
      setOrig(content)
      toast(t('Saved — applies the next time an agent starts'))
      reload()
    } catch (e) {
      toastError(e)
    } finally {
      setBusy(false)
    }
  }
  const remove = async (path: string) => {
    if (!confirm(t('Delete {path}?', { path }))) return
    try {
      await api('DELETE', `${base}/file?path=${encodeURIComponent(path)}`)
      if (open === path || open?.startsWith(path + '/')) setOpen(null)
      reload()
    } catch (e) {
      toastError(e)
    }
  }
  const newSkill = async (agent: 'claude' | 'codex') => {
    const name = prompt(t('Skill name (a-z, 0-9, -)'))?.trim().toLowerCase()
    if (!name || !/^[a-z0-9][a-z0-9-]*$/.test(name)) return
    const path = `${agent}/skills/${name}/SKILL.md`
    try {
      await api('PUT', `${base}/file?path=${encodeURIComponent(path)}`, { content: SKILL_TEMPLATE(name) })
      await reload()
      openFile(path)
    } catch (e) {
      toastError(e)
    }
  }
  const newFile = async () => {
    const path = prompt(t('Path inside the profile (e.g. claude/agents/reviewer.md)'))?.trim()
    if (!path) return
    try {
      await api('PUT', `${base}/file?path=${encodeURIComponent(path)}`, { content: '' })
      await reload()
      openFile(path)
    } catch (e) {
      toastError(e)
    }
  }

  const tree = useMemo(() => (files || []).filter((f) => f.path !== 'README.md'), [files])
  const skills = tree.filter((f) => f.dir && /^(claude|codex)\/skills\/[^/]+$/.test(f.path))
  const others = tree.filter((f) => !f.dir && f.path !== 'claude/plugins.json' && !QUICK.some((q) => q.path === f.path) && !/^(claude|codex)\/skills\//.test(f.path))

  return (
    <div class="profile-editor">
      <div class="pe-side">
        <div class="group-h">{t('Settings')}</div>
        {QUICK.map((q) => (
          <button key={q.path} class={cls('pe-item', open === q.path && 'on')} onClick={() => openFile(q.path)}>
            <IFile size={13} /> {t(q.label)}
          </button>
        ))}
        <div class="group-h row">
          <span class="grow">{t('Skills')}</span>
          <button class="link-btn" onClick={() => newSkill('claude')} title={t('New Claude skill')}>
            <IPlus size={12} /> Claude
          </button>
          <button class="link-btn" onClick={() => newSkill('codex')} title={t('New Codex skill')}>
            <IPlus size={12} /> Codex
          </button>
        </div>
        {skills.length === 0 && <div class="dim small pad-s">{t('No skills yet.')}</div>}
        {skills.map((sk) => (
          <div key={sk.path} class="pe-row">
            <button class={cls('pe-item', open?.startsWith(sk.path + '/') && 'on')} onClick={() => openFile(sk.path + '/SKILL.md')}>
              <IFolder size={13} /> {sk.path.split('/')[0]}: {sk.path.split('/').pop()}
            </button>
            <button class="icon-btn tiny" onClick={() => remove(sk.path)} aria-label={t('Delete skill')}>
              <ITrash size={13} />
            </button>
          </div>
        ))}
        <div class="group-h row">
          <span class="grow">{t('Other files')}</span>
          <button class="link-btn" onClick={newFile}>
            <IPlus size={12} /> {t('File')}
          </button>
        </div>
        {others.map((f) => (
          <div key={f.path} class="pe-row">
            <button class={cls('pe-item', open === f.path && 'on')} onClick={() => openFile(f.path)}>
              <IFile size={13} /> <span class="mono small">{f.path}</span>
            </button>
            <button class="icon-btn tiny" onClick={() => remove(f.path)} aria-label={t('Delete')}>
              <ITrash size={13} />
            </button>
          </div>
        ))}
      </div>
      <div class="pe-main">
        {open ? (
          <>
            <div class="row gap">
              <span class="mono small grow">{open}</span>
              <button class="btn primary small" disabled={busy || content === orig} onClick={save}>
                {busy && <Spinner size={12} />} {t('Save')}
              </button>
            </div>
            <textarea class="input mono pe-text" spellcheck={false} value={content} onInput={(e) => setContent((e.target as HTMLTextAreaElement).value)} />
          </>
        ) : (
          <div class="dim small pad">
            {scope === 'global'
              ? t('The global profile applies to every user’s isolated projects. Users can add personal skills and settings on top.')
              : t('Your personal profile is merged over the global one for all of your projects: your skills win on name clashes, MCP servers and settings are merged, instructions are appended.')}
          </div>
        )}
      </div>
    </div>
  )
}
