import { useEffect, useState } from 'preact/hooks'
import type { Agent, AgentDefaults, SessionPrefs } from '../types'
import { api } from '../api'
import { agentCaps, efforts, sessionPrefs, toast, toastError } from '../store'
import { t } from '../i18n'
import { Spinner } from './icons'

/** Changes my preferences for new sessions (the server tells my other devices). */
export async function patchSessionPrefs(patch: { agents?: Partial<Record<Agent, AgentDefaults>>; instructions?: string }) {
  sessionPrefs.value = await api<SessionPrefs>('PATCH', '/api/me/session-prefs', patch)
}

/** The models offered for an agent, keeping `current` even when it is not listed. */
export function modelOptions(agent: Agent, current: string) {
  const listed = agentCaps.value[agent]?.models?.length
    ? agentCaps.value[agent]!.models!
    : agent === 'claude'
      ? [{ id: 'opus', label: 'Opus' }, { id: 'sonnet', label: 'Sonnet' }, { id: 'haiku', label: 'Haiku' }]
      : []
  const opts = listed.filter((m) => m.id !== 'default').map((m) => ({ id: m.id, label: m.label || m.id }))
  if (current && !opts.some((o) => o.id === current)) opts.push({ id: current, label: current })
  return opts
}

/** Model and reasoning effort pickers; '' is the agent's own default. */
export function ModelEffortFields(p: { agent: Agent; model: string; effort: string; onModel: (v: string) => void; onEffort: (v: string) => void }) {
  return (
    <div class="row gap wrap">
      <label class="field grow">
        <span class="field-l">{t('Model')}</span>
        <select class="select" value={p.model} onChange={(e) => p.onModel((e.target as HTMLSelectElement).value)}>
          <option value="">{t('Default')}</option>
          {modelOptions(p.agent, p.model).map((m) => (
            <option key={m.id} value={m.id}>
              {m.label}
            </option>
          ))}
        </select>
      </label>
      <label class="field">
        <span class="field-l">{t('Effort')}</span>
        <select class="select" value={p.effort} onChange={(e) => p.onEffort((e.target as HTMLSelectElement).value)}>
          <option value="">{t('Default')}</option>
          {(efforts.value[p.agent] || []).map((x) => (
            <option key={x} value={x}>
              {x}
            </option>
          ))}
        </select>
      </label>
    </div>
  )
}

/** Settings: what the sessions I start get unless I choose otherwise. */
export function SessionPrefsCard() {
  const prefs = sessionPrefs.value
  const saved = prefs.instructions || ''
  const [text, setText] = useState(saved)
  const [busy, setBusy] = useState(false)
  // Saved on another device: show it, unless this one has unsaved edits.
  const [base, setBase] = useState(saved)
  useEffect(() => {
    if (text === base) setText(saved)
    setBase(saved)
  }, [saved])

  const run = async (fn: () => Promise<void>, done: string) => {
    setBusy(true)
    try {
      await fn()
      toast(done)
    } catch (e) {
      toastError(e)
    } finally {
      setBusy(false)
    }
  }
  const setDefaults = (agent: Agent, d: AgentDefaults) => run(() => patchSessionPrefs({ agents: { [agent]: d } }), t('Saved'))

  return (
    <section class="card col gap">
      <h3>{t('New sessions')}</h3>
      <p class="dim small">{t('What the sessions you start get unless you choose otherwise, from New session or through your Home agent. Default leaves the choice to the agent.')}</p>
      {(['claude', 'codex'] as Agent[]).map((a) => {
        const d = prefs.agents[a] || {}
        return (
          <div key={a} class="col gap">
            <b class="small">{a === 'claude' ? 'Claude Code' : 'Codex'}</b>
            <ModelEffortFields
              agent={a}
              model={d.model || ''}
              effort={d.effort || ''}
              onModel={(model) => setDefaults(a, { ...d, model })}
              onEffort={(effort) => setDefaults(a, { ...d, effort })}
            />
          </div>
        )
      })}
      <label class="field">
        <span class="field-l">{t('Instructions for all sessions')}</span>
        <textarea
          class="input env-edit"
          rows={5}
          value={text}
          placeholder={t('e.g. Write pull request titles in English and descriptions in Japanese.')}
          onInput={(e) => setText((e.target as HTMLTextAreaElement).value)}
        />
        <span class="field-h">
          {t('Given to every agent you start, in all of your projects and Home, as an addition to its system prompt. Your messages and the repository’s files (CLAUDE.md, AGENTS.md) are left as they are. Applies to sessions started from now on.')}
        </span>
      </label>
      <div class="modal-actions">
        <button class="btn" disabled={busy || text.trim() === saved} onClick={() => run(() => patchSessionPrefs({ instructions: text }), t('Saved — applies to sessions started from now on'))}>
          {busy && <Spinner size={12} />} {t('Save')}
        </button>
      </div>
    </section>
  )
}
