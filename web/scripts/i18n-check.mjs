// Fails when a string passed to t() has no Japanese entry in src/locales/ja/,
// or when two dictionaries translate the same key differently.
//   node scripts/i18n-check.mjs   (run from web/)
import fs from 'fs'
import path from 'path'

const src = path.resolve(import.meta.dirname, '../src')
const unq = (s) => s.replace(/\\'/g, "'").replace(/\\"/g, '"').replace(/\\n/g, '\n')

const files = []
const walk = (d) => {
  for (const e of fs.readdirSync(d, { withFileTypes: true })) {
    const p = path.join(d, e.name)
    if (e.isDirectory()) e.name !== 'locales' && walk(p)
    else if (/\.(ts|tsx)$/.test(e.name)) files.push(p)
  }
}
walk(src)

// 'key': 'value' / key: 'value' (value possibly on the next line)
const entryRe = /^\s*(?:'((?:\\.|[^'\\])*)'|"((?:\\.|[^"\\])*)"|([A-Za-z_]\w*))\s*:\s*\n?\s*(?:'((?:\\.|[^'\\])*)'|"((?:\\.|[^"\\])*)"|`([^`]*)`)\s*,?\s*$/gm
const all = {}
const dir = path.join(src, 'locales/ja')
for (const f of fs.readdirSync(dir)) {
  for (const m of fs.readFileSync(path.join(dir, f), 'utf8').matchAll(entryRe)) {
    ;(all[unq(m[1] ?? m[2] ?? m[3])] ??= []).push([f, unq(m[4] ?? m[5] ?? m[6])])
  }
}

let problems = 0
for (const f of files) {
  const s = fs.readFileSync(f, 'utf8')
  for (const m of s.matchAll(/\b(?:t|tr)\(\s*(?:'((?:\\.|[^'\\])*)'|"((?:\\.|[^"\\])*)")/g)) {
    const k = unq(m[1] ?? m[2])
    if (!all[k]) {
      problems++
      console.log(`missing ja: ${path.relative(src, f)}: ${JSON.stringify(k)}`)
    }
  }
}
for (const [k, vs] of Object.entries(all)) {
  if (new Set(vs.map((v) => v[1])).size > 1) {
    problems++
    console.log(`conflicting ja for ${JSON.stringify(k)}: ${vs.map((v) => `${v[0]}=${v[1]}`).join(' | ')}`)
  }
}
if (problems) process.exit(1)
console.log(`i18n: ${Object.keys(all).length} Japanese entries, all keys covered`)
