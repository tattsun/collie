import { t } from './i18n'
import { authed, conn, route } from './store'
import { Sidebar } from './components/Sidebar'
import { NewSession } from './components/NewSession'
import { SessionView } from './components/SessionView'
import { ProjectPage, ProjectsPage } from './components/Projects'
import { Toasts } from './components/ui'
import { Viewer } from './components/Media'
import { Spinner } from './components/icons'
import { cls } from './util'
import { inviteCode } from './authflow'
import { InvitePage, LoginPage, PasskeyBanner } from './components/Auth'
import { AccountPage } from './components/Account'
import { GroupsPage } from './components/Groups'
import { AdminPage } from './components/Admin'

function Main() {
  const r = route.value
  switch (r.name) {
    case 'session':
      return <SessionView key={r.id} id={r.id!} />
    case 'projects':
      return <ProjectsPage />
    case 'project':
      return <ProjectPage key={r.id} id={r.id!} />
    case 'account':
      return <AccountPage />
    case 'groups':
      return <GroupsPage key={r.id || ''} />
    case 'admin':
      return <AdminPage />
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
  if (inviteCode.value) return <InvitePage />
  if (authed.value === false) return <LoginPage />
  const r = route.value
  return (
    <div class={cls('app', 'r-' + r.name)}>
      <Sidebar />
      <main class="main">
        <PasskeyBanner />
        <Main />
      </main>
      {conn.value === 'offline' && <div class="offline">{t('Reconnecting…')}</div>}
      <Toasts />
      <Viewer />
    </div>
  )
}
