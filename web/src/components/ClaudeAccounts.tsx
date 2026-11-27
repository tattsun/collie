import { useEffect, useRef, useState } from 'preact/hooks'
import { api } from '../api'
import { t } from '../i18n'
import { toast, toastError } from '../store'
import { cls, copyText } from '../util'
import { ICheck, IPlus, ITrash, Spinner } from './icons'
import { Modal, Segmented, rich } from './ui'

export type Kind = 'subscription' | 'bedrock' | 'apikey' | 'custom'

export interface ClaudeAccount {
  id: string
  name: string
  kind: Kind
  values?: Record<string, string>
  secrets?: Record<string, boolean>
  raw?: string
}
export interface ClaudeAccounts {
  accounts: ClaudeAccount[]
  default: string
  projects: Record<string, string>
}

export const loadClaudeAccounts = () => api<ClaudeAccounts>('GET', '/api/me/claude/accounts')

export const kindLabel = (k: Kind) =>
  k === 'subscription' ? t('Claude subscription') : k === 'bedrock' ? 'Amazon Bedrock' : k === 'apikey' ? t('Anthropic API key') : t('Custom')

interface Login {
  id: string
  state: 'starting' | 'link' | 'saving' | 'done' | 'error'
  url: string
  /** The link's host: only Anthropic's are ever offered. */
  host?: string
  error: string
}

/** Runs `claude setup-token` on the server: show the link, forward the code, save the token. */
function SignInModal({ accountId, name, onDone, onClose }: { accountId?: string; name?: string; onDone: () => void; onClose: () => void }) {
  const [login, setLogin] = useState<Login | null>(null)
  const [err, setErr] = useState('')
  const [code, setCode] = useState('')
  const [sending, setSending] = useState(false)
  const idRef = useRef('')
  const doneRef = useRef(false)

  useEffect(() => {
    let alive = true
    let timer: number | undefined
    const poll = async () => {
      if (!alive || !idRef.current) return
      try {
        const l = await api<Login>('GET', `/api/me/claude/login/${idRef.current}`)
        if (!alive) return
        setLogin(l)
        if (l.state === 'done') {
          doneRef.current = true
          toast(t('Signed in — used by your agents from their next start'))
          onDone()
          return
        }
        if (l.state === 'error') return
      } catch (e) {
        if (alive) setErr(String((e as Error).message || e))
        return
      }
      timer = window.setTimeout(poll, 1200)
    }
    api<Login>('POST', '/api/me/claude/login', { id: accountId || '', name: name || '' })
      .then((l) => {
        if (!alive) {
          api('DELETE', `/api/me/claude/login/${l.id}`).catch(() => {})
          return
        }
        idRef.current = l.id
        setLogin(l)
        poll()
      })
      .catch((e) => alive && setErr(String(e.message || e)))
    return () => {
      alive = false
      clearTimeout(timer)
      if (idRef.current && !doneRef.current) api('DELETE', `/api/me/claude/login/${idRef.current}`).catch(() => {})
    }
  }, [])

  const send = async () => {
    if (!code.trim() || !login) return
    setSending(true)
    try {
      await api('POST', `/api/me/claude/login/${login.id}/code`, { code })
      setCode('')
    } catch (e) {
      toastError(e)
    } finally {
      setSending(false)
    }
  }

  const failed = err || (login?.state === 'error' ? login.error : '')
  return (
    <Modal title={t('Sign in with Claude')} onClose={onClose}>
      {failed && <div class="error-box">{failed}</div>}
      {!failed && (!login || login.state === 'starting') && (
        <p class="dim row gap">
          <Spinner size={14} /> {t('Starting the sign-in…')}
        </p>
      )}
      {!failed && login && (login.state === 'link' || login.state === 'saving') && (
        <div class="col gap">
          <p>
            <b>1.</b> {t('Open the link and sign in with your Claude account.')}
          </p>
          <div class="row gap wrap">
            <a class="btn primary" href={login.url} target="_blank" rel="noopener noreferrer">
              {t('Open sign-in page')}
            </a>
            <button class="btn" onClick={() => copyText(login.url).then((ok) => toast(ok ? t('Copied') : t('Copy failed')))}>
              {t('Copy link')}
            </button>
          </div>
          {login.host && <p class="dim small">{rich(t('The page is on {host}.'), { host: <b class="mono">{login.host}</b> })}</p>}
          <p>
            <b>2.</b> {t('Paste the code it shows here.')}
          </p>
          <div class="row gap">
            <input
              class="input mono grow"
              value={code}
              autofocus
              placeholder={t('Code')}
              disabled={login.state === 'saving'}
              onInput={(e) => setCode((e.target as HTMLInputElement).value)}
              onKeyDown={(e) => e.key === 'Enter' && send()}
            />
            <button class="btn primary" disabled={!code.trim() || sending || login.state === 'saving'} onClick={send}>
              {(sending || login.state === 'saving') && <Spinner size={12} />} {t('Continue')}
            </button>
          </div>
        </div>
      )}
      <div class="modal-actions">
        <button class="btn" onClick={onClose}>
          {t('Cancel')}
        </button>
      </div>
    </Modal>
  )
}

const BEDROCK_AUTH = ['bearer', 'keys'] as const

function Field({ label, children, hint }: { label: string; children: any; hint?: any }) {
  return (
    <label class="field">
      <span class="field-l">{label}</span>
      {children}
      {hint && <span class="field-h">{hint}</span>}
    </label>
  )
}

/** Add or edit an account. */
function AccountForm({ account, onSaved, onClose }: { account?: ClaudeAccount; onSaved: () => void; onClose: () => void }) {
  const editing = !!account
  const [kind, setKind] = useState<Kind>(account?.kind || 'subscription')
  const [name, setName] = useState(account?.name || '')
  const [vals, setVals] = useState<Record<string, string>>({ ...(account?.values || {}) })
  const [auth, setAuth] = useState<(typeof BEDROCK_AUTH)[number]>(account?.secrets?.AWS_SECRET_ACCESS_KEY || account?.values?.AWS_ACCESS_KEY_ID ? 'keys' : 'bearer')
  const [raw, setRaw] = useState(account?.raw || '')
  const [busy, setBusy] = useState(false)
  const [signIn, setSignIn] = useState(false)
  const [manual, setManual] = useState(false)
  const set = (k: string, v: string) => setVals((x) => ({ ...x, [k]: v }))
  const input = (k: string, ph?: string, secret?: boolean) => (
    <input
      class="input mono"
      type={(secret ? 'password' : 'text') as 'text'}
      value={vals[k] || ''}
      autocomplete="off"
      spellcheck={false}
      placeholder={secret && account?.secrets?.[k] ? t('(saved — leave empty to keep)') : ph}
      onInput={(e) => set(k, (e.target as HTMLInputElement).value)}
    />
  )
  const defaultName = kind === 'bedrock' ? 'Bedrock' : kind === 'apikey' ? 'API key' : kind === 'custom' ? 'Custom' : 'Claude subscription'

  const save = async (extra?: { token?: string }) => {
    setBusy(true)
    try {
      const nm = name.trim() || defaultName
      if (kind === 'subscription') {
        await api('POST', '/api/me/claude/token', { token: extra?.token || '', id: account?.id || '', name: nm })
      } else {
        const values: Record<string, string> = { ...vals }
        const clear: string[] = []
        if (kind === 'bedrock') {
          const drop = auth === 'bearer' ? ['AWS_ACCESS_KEY_ID', 'AWS_SECRET_ACCESS_KEY', 'AWS_SESSION_TOKEN'] : ['AWS_BEARER_TOKEN_BEDROCK']
          drop.forEach((k) => {
            delete values[k]
            clear.push(k)
          })
        }
        const body = { name: nm, kind, values, clear, raw }
        if (editing) await api('PUT', `/api/me/claude/accounts/${account!.id}`, body)
        else await api('POST', '/api/me/claude/accounts', body)
      }
      toast(t('Saved — used by your agents from their next start'))
      onSaved()
    } catch (e) {
      toastError(e)
    } finally {
      setBusy(false)
    }
  }

  if (signIn)
    return <SignInModal accountId={account?.id} name={name.trim() || defaultName} onDone={onSaved} onClose={() => setSignIn(false)} />

  return (
    <Modal title={editing ? t('Edit account') : t('Add a Claude account')} onClose={onClose}>
      <div class="col gap">
        {!editing && (
          <Segmented
            value={kind}
            onChange={setKind}
            options={[
              { value: 'subscription', label: t('Subscription') },
              { value: 'bedrock', label: 'Bedrock' },
              { value: 'apikey', label: t('API key') },
              { value: 'custom', label: t('Custom') },
            ]}
          />
        )}
        <Field label={t('Name')} hint={t('Shown when you pick an account for a project, e.g. “Personal” or “Work Bedrock”.')}>
          <input class="input" value={name} placeholder={defaultName} onInput={(e) => setName((e.target as HTMLInputElement).value)} />
        </Field>

        {kind === 'subscription' && (
          <>
            <p class="dim small">{t('Uses your Claude Pro / Max plan. Collie runs the sign-in for you — no terminal needed.')}</p>
            <div class="row gap wrap">
              <button class="btn primary" disabled={busy} onClick={() => setSignIn(true)}>
                {editing ? t('Sign in again') : t('Sign in with Claude')}
              </button>
              <button class="btn" onClick={() => setManual(!manual)}>
                {t('Paste a token instead')}
              </button>
            </div>
            {manual && (
              <div class="col gap">
                <p class="dim small">{rich(t('Run {cmd} anywhere you are signed in to Claude and paste the token.'), { cmd: <code>claude setup-token</code> })}</p>
                <div class="row gap">
                  <input class="input mono grow" type="password" placeholder="sk-ant-oat01-…" value={vals.CLAUDE_CODE_OAUTH_TOKEN || ''} onInput={(e) => set('CLAUDE_CODE_OAUTH_TOKEN', (e.target as HTMLInputElement).value)} />
                  <button class="btn" disabled={busy || !(vals.CLAUDE_CODE_OAUTH_TOKEN || '').trim()} onClick={() => save({ token: vals.CLAUDE_CODE_OAUTH_TOKEN })}>
                    {t('Save')}
                  </button>
                </div>
              </div>
            )}
          </>
        )}

        {kind === 'bedrock' && (
          <>
            <p class="dim small">{t('Runs Claude Code on Amazon Bedrock with your AWS credentials. Billing goes to your AWS account.')}</p>
            <Field label="AWS region">{input('AWS_REGION', 'us-east-1')}</Field>
            <Segmented
              value={auth}
              onChange={setAuth}
              options={[
                { value: 'bearer', label: t('Bedrock API key') },
                { value: 'keys', label: t('Access keys') },
              ]}
            />
            {auth === 'bearer' ? (
              <Field label={t('Bedrock API key')}>{input('AWS_BEARER_TOKEN_BEDROCK', '', true)}</Field>
            ) : (
              <>
                <Field label="AWS_ACCESS_KEY_ID">{input('AWS_ACCESS_KEY_ID', 'AKIA…')}</Field>
                <Field label="AWS_SECRET_ACCESS_KEY">{input('AWS_SECRET_ACCESS_KEY', '', true)}</Field>
                <Field label="AWS_SESSION_TOKEN" hint={t('Only for temporary credentials.')}>
                  {input('AWS_SESSION_TOKEN', '', true)}
                </Field>
              </>
            )}
            <details>
              <summary class="dim small">{t('Model overrides (optional)')}</summary>
              <div class="col gap">
                <Field label="ANTHROPIC_MODEL" hint={t('A Bedrock model ID or inference profile ARN. Leave empty for Claude Code’s default.')}>
                  {input('ANTHROPIC_MODEL', 'us.anthropic.claude-…')}
                </Field>
                <Field label="ANTHROPIC_DEFAULT_HAIKU_MODEL">{input('ANTHROPIC_DEFAULT_HAIKU_MODEL', 'us.anthropic.claude-…')}</Field>
              </div>
            </details>
          </>
        )}

        {kind === 'apikey' && (
          <>
            <p class="dim small">{t('Pay-as-you-go with an Anthropic API key (or any compatible endpoint).')}</p>
            <Field label="ANTHROPIC_API_KEY">{input('ANTHROPIC_API_KEY', 'sk-ant-api03-…', true)}</Field>
            <Field label={t('Base URL (optional)')}>{input('ANTHROPIC_BASE_URL', 'https://…')}</Field>
          </>
        )}

        {kind === 'custom' && (
          <>
            <p class="dim small">{t('Any KEY=VALUE lines, e.g. for Vertex AI or a gateway.')}</p>
            <textarea class="input mono env-edit" rows={6} spellcheck={false} value={raw} placeholder={'CLAUDE_CODE_USE_VERTEX=1\nANTHROPIC_VERTEX_PROJECT_ID=…'} onInput={(e) => setRaw((e.target as HTMLTextAreaElement).value)} />
          </>
        )}
      </div>
      <div class="modal-actions">
        <button class="btn" onClick={onClose}>
          {t('Cancel')}
        </button>
        {kind !== 'subscription' && (
          <button class="btn primary" disabled={busy} onClick={() => save()}>
            {busy && <Spinner size={12} />} {t('Save')}
          </button>
        )}
      </div>
    </Modal>
  )
}

/** The Claude accounts list (Account → Agent accounts). */
export function ClaudeAccountsCard() {
  const [data, setData] = useState<ClaudeAccounts | null>(null)
  const [form, setForm] = useState<ClaudeAccount | 'new' | null>(null)
  const load = () =>
    loadClaudeAccounts()
      .then(setData)
      .catch(toastError)
  useEffect(() => {
    load()
  }, [])
  const act = (fn: () => Promise<unknown>) => async () => {
    try {
      await fn()
      load()
    } catch (e) {
      toastError(e)
    }
  }
  const saved = () => {
    setForm(null)
    load()
  }
  return (
    <section class="card col gap">
      <h3>Claude Code</h3>
      <p class="dim small">
        {t('Your agents use these accounts. The default applies everywhere; you can pick a different one per project in the project’s Settings.')}
      </p>
      {!data && <Spinner />}
      {data && data.accounts.length === 0 && <p class="warn small">{t('No Claude account yet — add one to start agents.')}</p>}
      {data?.accounts.map((a) => (
        <div key={a.id} class="row gap list-row">
          <span class="grow">
            <b>{a.name}</b> <span class="chip">{kindLabel(a.kind)}</span>
            {data.default === a.id && (
              <span class="chip ok-chip">
                <ICheck size={11} /> {t('default')}
              </span>
            )}
            {a.values?.AWS_REGION && <span class="dim small"> · {a.values.AWS_REGION}</span>}
          </span>
          {data.default !== a.id && (
            <button class="btn small" onClick={act(() => api('PUT', '/api/me/claude/default', { id: a.id }))}>
              {t('Make default')}
            </button>
          )}
          <button class="icon-btn" aria-label={t('Edit')} onClick={() => setForm(a)}>
            ✎
          </button>
          <button
            class="icon-btn"
            aria-label={t('Delete')}
            onClick={() => confirm(t('Delete account “{name}”?', { name: a.name })) && act(() => api('DELETE', `/api/me/claude/accounts/${a.id}`))()}
          >
            <ITrash size={14} />
          </button>
        </div>
      ))}
      <div class="row gap">
        <button class={cls('btn', data && data.accounts.length === 0 && 'primary')} onClick={() => setForm('new')}>
          <IPlus size={14} /> {t('Add account')}
        </button>
      </div>
      {form && <AccountForm account={form === 'new' ? undefined : form} onSaved={saved} onClose={() => setForm(null)} />}
    </section>
  )
}

/** Per-project account choice (Project → Settings). */
export function ProjectClaudeAccount({ projectId }: { projectId: string }) {
  const [data, setData] = useState<ClaudeAccounts | null>(null)
  useEffect(() => {
    loadClaudeAccounts().then(setData).catch(() => {})
  }, [projectId])
  if (!data) return null
  const def = data.accounts.find((a) => a.id === data.default)
  const chosen = data.projects[projectId] || ''
  const pick = async (id: string) => {
    try {
      await api('PUT', `/api/me/claude/projects/${projectId}`, { id })
      setData({ ...data, projects: { ...data.projects, [projectId]: id } })
      toast(t('Saved — applies to agents and shells started from now on'))
    } catch (e) {
      toastError(e)
    }
  }
  return (
    <section class="card col gap">
      <h3>{t('Claude account')}</h3>
      <p class="dim small">{t('Which of your Claude accounts agents use in this project. Other members of a group project use their own.')}</p>
      {data.accounts.length === 0 ? (
        <p class="warn small">
          {t('No Claude account yet.')} <a href="#/account/agents">{t('Add one')}</a>
        </p>
      ) : (
        <select class="select" value={chosen} onChange={(e) => pick((e.target as HTMLSelectElement).value)}>
          <option value="">{t('Default')}{def ? ` (${def.name})` : ''}</option>
          {data.accounts.map((a) => (
            <option key={a.id} value={a.id}>
              {a.name} — {kindLabel(a.kind)}
            </option>
          ))}
        </select>
      )}
    </section>
  )
}
