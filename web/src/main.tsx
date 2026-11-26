import { render } from 'preact'
import './styles.css'
import { App } from './App'
import { start } from './boot'

if ('serviceWorker' in navigator && window.isSecureContext) {
  navigator.serviceWorker.register('/sw.js').catch(() => {})
}

render(<App />, document.getElementById('app')!)
start()
