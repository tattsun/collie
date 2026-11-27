// Shoots the README screenshots and demo GIF into docs/images/.
//
//   mise run shots   (= mise run build && cd tools/demo && npm ci && node shoot.mjs)
//
// It runs a throwaway Collie (bin/collie) whose "claude" is
// fake-claude.mjs, so it needs no agent account and gives the same
// pictures every time. Pass --only=desktop,mobile,gif to shoot a subset.
import { execSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { pathToFileURL } from 'node:url'
import { writeGif } from './gif.mjs'
import { repoRoot, stage } from './stage.mjs'

const outDir = path.join(repoRoot, 'docs/images')
const only = (process.argv.find((a) => a.startsWith('--only=')) || '').slice(7).split(',').filter(Boolean)
const want = (k) => only.length === 0 || only.includes(k)
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

async function loadPlaywright() {
  try {
    return await import('playwright')
  } catch {}
  // Fall back to a global install (e.g. the one inside @playwright/mcp),
  // also of the system Node when mise runs its own.
  const roots = [execSync('npm root -g', { encoding: 'utf8' }).trim(), '/usr/local/lib/node_modules', '/usr/lib/node_modules']
  for (const g of roots) {
    for (const p of ['playwright', '@playwright/mcp/node_modules/playwright']) {
      const f = path.join(g, p, 'index.mjs')
      if (fs.existsSync(f)) return import(pathToFileURL(f).href)
    }
  }
  throw new Error('Playwright not found: npm i playwright && npx playwright install chromium')
}

const { chromium } = await loadPlaywright()
const browser = await chromium.launch()
fs.mkdirSync(outDir, { recursive: true })

const phone = { viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true }
async function newPage(opts, scheme) {
  const ctx = await browser.newContext({ locale: 'en-US', timezoneId: 'UTC', colorScheme: scheme, deviceScaleFactor: 2, ...opts })
  // The demo runs without passkeys; hide the banner nagging about it.
  await ctx.addInitScript(() => {
    addEventListener('DOMContentLoaded', () => {
      const s = document.createElement('style')
      s.textContent = '.banner.warn { display: none !important }'
      document.head.append(s)
    })
  })
  return ctx.newPage()
}

async function waitStatus(demo, sid, ok) {
  for (let i = 0; i < 600; i++) {
    const st = await demo.api('GET', '/api/state')
    const s = st.sessions.find((x) => x.id === sid)
    if (s && ok(s.status)) return s
    await sleep(100)
  }
  throw new Error(`session ${sid} never got there`)
}

async function open(page, base, hash) {
  await page.goto(base + '/' + hash)
  await page.waitForLoadState('networkidle')
  await sleep(600)
}

// ---- stills: desktop and phone, dark and light ----

async function stills() {
  const demo = await stage(browser)
  try {
    const search = await demo.startSearch()
    await waitStatus(demo, search.id, (s) => s === 'waiting')
    await waitStatus(demo, demo.sessions.dark.id, (s) => s === 'waiting')
    await sleep(500)
    for (const scheme of ['dark', 'light']) {
      if (want('desktop')) {
        const page = await newPage({ viewport: { width: 1280, height: 800 } }, scheme)
        await open(page, demo.base, `#/s/${search.id}`)
        await page.screenshot({ path: path.join(outDir, `desktop-${scheme}.png`) })
        await page.context().close()
      }
      if (want('mobile')) {
        const page = await newPage(phone, scheme)
        const shot = (name) => page.screenshot({ path: path.join(outDir, `.phone-${name}-${scheme}.png`) })
        await open(page, demo.base, '#/')
        await shot('list')
        await open(page, demo.base, `#/s/${search.id}`)
        await shot('approval')
        await open(page, demo.base, `#/s/${demo.sessions.dark.id}`)
        await shot('question')
        await open(page, demo.base, `#/s/${search.id}/changes`)
        await page.locator('.cfile .row-btn', { hasText: 'src/app.js' }).click()
        await sleep(400)
        await shot('changes')
        await page.context().close()
        await phones(['list', 'approval', 'changes'], scheme)
      }
    }
  } finally {
    demo.stop()
  }
}

// Lays phone screenshots side by side on a transparent background.
async function phones(names, scheme) {
  const src = (n) => path.join(outDir, `.phone-${n}-${scheme}.png`)
  const imgs = names.map((n) => `<img src="data:image/png;base64,${fs.readFileSync(src(n)).toString('base64')}">`).join('')
  const border = scheme === 'dark' ? '#3a3a3e' : '#d9d6d0'
  const page = await browser.newPage({ viewport: { width: 3 * 390 + 2 * 48 + 2 * 24, height: 844 + 2 * 24 }, deviceScaleFactor: 2 })
  await page.setContent(`<style>
    body { margin: 0; padding: 24px; display: flex; gap: 48px; background: transparent }
    img { width: 390px; height: 844px; border-radius: 36px; border: 1px solid ${border}; box-shadow: 0 6px 24px rgb(0 0 0 / 18%) }
  </style>${imgs}`)
  await page.screenshot({ path: path.join(outDir, `mobile-${scheme}.png`), omitBackground: true })
  await page.close()
  for (const n of ['list', 'approval', 'question', 'changes']) fs.rmSync(src(n), { force: true })
}

// ---- the demo GIF: a phone starts a task, approves, reviews ----

async function film(scheme) {
  // The agent plays faster than in real life, to keep the clip short.
  const demo = await stage(browser, { speed: 2 })
  const page = await newPage({ ...phone, deviceScaleFactor: 1.5 }, scheme)
  // A dot where the "finger" taps, so viewers can follow along.
  await page.addInitScript(() => {
    window.__tap = (x, y) => {
      const d = document.createElement('div')
      d.style.cssText = `position:fixed;left:${x - 22}px;top:${y - 22}px;width:44px;height:44px;border-radius:50%;
        background:rgb(255 255 255 / 45%);border:2px solid rgb(255 255 255 / 85%);box-shadow:0 0 0 1px rgb(0 0 0 / 25%);
        z-index:99999;pointer-events:none;transition:transform .45s,opacity .45s`
      document.body.append(d)
      requestAnimationFrame(() => requestAnimationFrame(() => { d.style.transform = 'scale(1.5)'; d.style.opacity = '0' }))
      setTimeout(() => d.remove(), 600)
    }
  })
  const tap = async (loc) => {
    await loc.scrollIntoViewIfNeeded()
    const b = await loc.boundingBox()
    await page.evaluate(([x, y]) => window.__tap(x, y), [b.x + b.width / 2, b.y + b.height / 2])
    await sleep(180)
    await loc.click()
  }

  await open(page, demo.base, '#/')
  const frames = []
  let recording = true
  const recorder = (async () => {
    let last = Date.now()
    while (recording) {
      const png = await page.screenshot()
      const now = Date.now()
      if (frames.length) frames[frames.length - 1].ms = now - last
      frames.push({ png, ms: 100 })
      last = now
    }
  })()

  try {
    await sleep(1200)
    await tap(page.locator('.side-head .btn.primary'))
    await page.locator('.composer-box textarea').waitFor()
    await sleep(500)
    await page.locator('.new-opts select').first().selectOption({ label: 'recipe-box' })
    await page.locator('.new-opts select').nth(1).selectOption('acceptEdits')
    await tap(page.locator('.composer-box textarea'))
    await page.keyboard.type('Add a search box to the recipe list. It should match ingredients too', { delay: 28 })
    await sleep(400)
    await tap(page.locator('.new-session .toggle .track'))
    await sleep(700)
    await tap(page.locator('.new-session .send'))

    const allow = page.locator('.ask-actions .btn.primary')
    await allow.waitFor({ timeout: 60_000 })
    await sleep(1300)
    await tap(allow)
    const sid = decodeURIComponent(page.url().split('#/s/')[1] || '')
    await waitStatus(demo, sid, (s) => s === 'idle')
    await sleep(1800)

    await tap(page.locator('.media-img').first())
    await sleep(1200)
    await tap(page.locator('.viewer-nav.next'))
    await sleep(1400)
    await tap(page.locator('.viewer-bar .icon-btn').last())
    await sleep(700)

    await tap(page.locator('nav.tabs a.tab').nth(1))
    await sleep(900)
    await tap(page.locator('.cfile .row-btn', { hasText: 'src/app.js' }))
    await sleep(1000)
    await page.mouse.move(195, 500)
    for (let i = 0; i < 8; i++) {
      await page.mouse.wheel(0, 90)
      await sleep(60)
    }
    await sleep(2200)
  } finally {
    recording = false
    await recorder
    await page.context().close()
    demo.stop()
  }
  frames[frames.length - 1].ms = 1500
  writeGif(path.join(outDir, `demo-${scheme}.gif`), frames)
}

try {
  if (want('desktop') || want('mobile')) await stills()
  if (want('gif')) await film('dark')
} finally {
  await browser.close()
}
for (const f of fs.readdirSync(outDir).sort()) console.log(`${(fs.statSync(path.join(outDir, f)).size / 1024).toFixed(0).padStart(6)} KB  docs/images/${f}`)
