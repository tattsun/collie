import { useState } from 'preact/hooks'
import { authed, conn, route } from './store'
import { Sidebar } from './components/Sidebar'
import { NewSession } from './components/NewSession'
import { SessionView } from './components/SessionView'
import { ProjectPage, ProjectsPage } from './components/Projects'
import { Toasts } from './components/ui'
import { Spinner } from './components/icons'
import { cls } from './util'
import { login } from './boot'

function Login() {
  const [token, setToken] = useState('')
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)
  const submit = async (e: Event) => {
    e.preventDefault()
    setBusy(true)
    setErr('')
    const ok = await login(token.trim())
    setBusy(false)
    if (!ok) setErr('That token did not work.')
  }
  return (
    <div class="login">
      <form class="card login-card" onSubmit={submit}>
        <img src="/icon.svg" width={48} height={48} alt="" />
        <h1>Collie</h1>
        <p class="dim small">
          Enter the access token printed by the server
          <br />
          (or run <code>collie --print-token</code>).
        </p>
        <input class="input mono" type="password" autoComplete="current-password" value={token} placeholder="Access token" onInput={(e) => setToken((e.target as HTMLInputElement).value)} autoFocus />
        {err && <div class="err small">{err}</div>}
        <button class="btn primary big" disabled={!token || busy}>
          {busy && <Spinner size={12} />} Sign in
        </button>
      </form>
    </div>
  )
}

function Main() {
  const r = route.value
  switch (r.name) {
    case 'session':
      return <SessionView key={r.id} id={r.id!} />
    case 'projects':
      return <ProjectsPage />
    case 'project':
      return <ProjectPage key={r.id} id={r.id!} />
    default:
      return <NewSession key={r.query.get('project') || ''} />
  }
}

export function App() {
  if (authed.value === null)
    return (
      <div class="login">
        <Spinner size={22} />
      </div>
    )
  if (authed.value === false) return <Login />
  const r = route.value
  return (
    <div class={cls('app', 'r-' + r.name)}>
      <Sidebar />
      <main class="main">
        <Main />
      </main>
      {conn.value === 'offline' && <div class="offline">Reconnecting…</div>}
      <Toasts />
    </div>
  )
}
