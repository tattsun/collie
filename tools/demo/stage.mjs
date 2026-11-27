// Stages the demo: throwaway projects, a fresh Collie (bin/collie) whose
// "claude" is fake-claude.mjs, and the sessions the README shows.
import { execFileSync, spawn } from 'node:child_process'
import fs from 'node:fs'
import http from 'node:http'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { apiServer, blog, recipeBox, recipeBoxAfter } from './repos.mjs'

const here = path.dirname(fileURLToPath(import.meta.url))
export const repoRoot = path.resolve(here, '../..')
export const work = process.env.DEMO_DIR || '/tmp/collie-demo'
export const mediaDir = path.join(work, 'media')
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

function writeTree(dir, files) {
  for (const [name, body] of Object.entries(files)) {
    fs.mkdirSync(path.dirname(path.join(dir, name)), { recursive: true })
    fs.writeFileSync(path.join(dir, name), body)
  }
}

function gitRepo(dir, files) {
  writeTree(dir, files)
  const git = (...a) => execFileSync('git', ['-c', 'user.name=Demo', '-c', 'user.email=demo@example.com', ...a], { cwd: dir, stdio: 'ignore' })
  git('init', '-q', '-b', 'main')
  git('add', '-A')
  git('commit', '-q', '-m', 'Initial commit')
}

// Serves a directory over http (ES modules do not load from file://).
export function serveDir(dir) {
  const types = { '.html': 'text/html; charset=utf-8', '.js': 'text/javascript', '.css': 'text/css' }
  const srv = http.createServer((req, res) => {
    const p = path.join(dir, decodeURIComponent(new URL(req.url, 'http://x').pathname).replace(/\/$/, '/index.html'))
    fs.readFile(p, (err, b) => {
      if (err) return res.writeHead(404).end()
      res.writeHead(200, { 'content-type': types[path.extname(p)] || 'application/octet-stream' }).end(b)
    })
  })
  return new Promise((resolve) => srv.listen(0, '127.0.0.1', () => resolve({ url: `http://127.0.0.1:${srv.address().port}/`, close: () => srv.close() })))
}

// The app screenshots the search scenario shows with show_media.
async function appShots(browser) {
  const dir = path.join(work, 'recipe-box-after')
  writeTree(dir, recipeBoxAfter())
  const srv = await serveDir(dir)
  const page = await browser.newPage({ viewport: { width: 390, height: 700 }, deviceScaleFactor: 2, locale: 'en-US' })
  await page.goto(srv.url)
  await page.screenshot({ path: path.join(mediaDir, 'app-all.png') })
  await page.fill('#q', 'tomato')
  await page.screenshot({ path: path.join(mediaDir, 'app-tomato.png') })
  await page.close()
  srv.close()
}

export async function stage(browser, { port = 7979, speed = 1 } = {}) {
  fs.rmSync(work, { recursive: true, force: true })
  const root = path.join(work, 'projects')
  fs.mkdirSync(mediaDir, { recursive: true })
  gitRepo(path.join(root, 'recipe-box'), recipeBox)
  gitRepo(path.join(root, 'shop-api'), apiServer)
  gitRepo(path.join(root, 'blog'), blog)
  await appShots(browser)

  const bin = path.join(repoRoot, 'bin/collie')
  if (!fs.existsSync(bin)) throw new Error('build first: mise run build')
  const base = `http://127.0.0.1:${port}`
  const proc = spawn(
    bin,
    ['--no-auth', '--addr', `127.0.0.1:${port}`, '--data', path.join(work, 'data'), '--root', root,
      '--claude', path.join(here, 'fake-claude.mjs'), '--codex', '/nonexistent/codex', '--podman', '/nonexistent/podman', '--idle-timeout', '0'],
    { env: { ...process.env, USER: 'demo', DEMO_MEDIA: mediaDir, DEMO_SPEED: String(speed) }, stdio: ['ignore', 'ignore', process.env.DEMO_LOG ? 'inherit' : 'ignore'] },
  )
  process.on('exit', () => proc.kill())
  for (let i = 0; ; i++) {
    try {
      if ((await fetch(base + '/api/state')).ok) break
    } catch {}
    if (i > 100) throw new Error('collie did not start')
    await sleep(100)
  }
  const api = async (method, p, body) => {
    const r = await fetch(base + p, { method, headers: { 'content-type': 'application/json' }, body: body && JSON.stringify(body) })
    if (!r.ok) throw new Error(`${method} ${p}: ${r.status} ${await r.text()}`)
    return r.json()
  }
  const project = (name) => api('POST', '/api/projects', { kind: 'existing', path: path.join(root, name), runtime: 'host' })
  const blogP = await project('blog')
  const shop = await project('shop-api')
  const recipe = await project('recipe-box')

  const session = (p, title, prompt, extra = {}) =>
    api('POST', '/api/sessions', { projectId: p.id, agent: 'claude', mode: 'acceptEdits', title, prompt, ...extra })
  const sessions = {}
  sessions.pagination = await session(blogP, 'Fix empty pages after page 1', 'The post list is empty from page 2 on. Please fix it')
  await sleep(1500)
  sessions.webhook = await session(shop, 'Make Stripe webhooks idempotent', 'Stripe webhooks are sometimes processed twice. Make the handler idempotent')
  await sleep(1500)
  sessions.dark = await session(recipe, 'Dark mode', 'I want a dark mode')
  await sleep(1500)
  return {
    base,
    api,
    projects: { blog: blogP, shop, recipe },
    sessions,
    // The search session is started by the caller, so it can be filmed.
    startSearch: () =>
      session(recipe, 'Add a search box', 'Add a search box to the recipe list. It should match ingredients too', { worktree: true }),
    stop: () => proc.kill(),
  }
}
