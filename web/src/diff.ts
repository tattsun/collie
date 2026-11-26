import { t as tr } from './i18n'
export type DiffLine = { k: 'add' | 'del' | 'ctx' | 'hunk' | 'file' | 'meta'; t: string }

export function parseDiff(text: string): DiffLine[] {
  const out: DiffLine[] = []
  for (const t of text.replace(/\n$/, '').split('\n')) {
    if (t.startsWith('diff --git') || t.startsWith('+++ ') || t.startsWith('--- ')) out.push({ k: 'file', t })
    else if (t.startsWith('@@')) out.push({ k: 'hunk', t })
    else if (t.startsWith('index ') || t.startsWith('new file') || t.startsWith('deleted file') || t.startsWith('similarity') || t.startsWith('rename ') || t.startsWith('\\'))
      out.push({ k: 'meta', t })
    else if (t.startsWith('+')) out.push({ k: 'add', t })
    else if (t.startsWith('-')) out.push({ k: 'del', t })
    else out.push({ k: 'ctx', t })
  }
  return out
}

/** Line diff of two snippets (LCS), used for Claude's Edit tool input. */
export function snippetDiff(a: string, b: string): DiffLine[] {
  const x = a === '' ? [] : a.split('\n')
  const y = b === '' ? [] : b.split('\n')
  if (x.length * y.length > 250_000) {
    return [...x.map((t) => ({ k: 'del' as const, t: '-' + t })), ...y.map((t) => ({ k: 'add' as const, t: '+' + t }))]
  }
  const n = x.length
  const m = y.length
  const dp: Uint32Array[] = Array.from({ length: n + 1 }, () => new Uint32Array(m + 1))
  for (let i = n - 1; i >= 0; i--)
    for (let j = m - 1; j >= 0; j--) dp[i][j] = x[i] === y[j] ? dp[i + 1][j + 1] + 1 : Math.max(dp[i + 1][j], dp[i][j + 1])
  const out: DiffLine[] = []
  let i = 0
  let j = 0
  while (i < n && j < m) {
    if (x[i] === y[j]) {
      out.push({ k: 'ctx', t: ' ' + x[i] })
      i++
      j++
    } else if (dp[i + 1][j] >= dp[i][j + 1]) out.push({ k: 'del', t: '-' + x[i++] })
    else out.push({ k: 'add', t: '+' + y[j++] })
  }
  while (i < n) out.push({ k: 'del', t: '-' + x[i++] })
  while (j < m) out.push({ k: 'add', t: '+' + y[j++] })
  return collapseContext(out)
}

function collapseContext(lines: DiffLine[], keep = 3): DiffLine[] {
  const out: DiffLine[] = []
  let run: DiffLine[] = []
  const flush = (end: boolean) => {
    const start = out.length === 0
    if (run.length > keep * 2 + 1 || ((start || end) && run.length > keep)) {
      const head = start ? [] : run.slice(0, keep)
      const tail = end ? [] : run.slice(-keep)
      const hidden = run.length - head.length - tail.length
      out.push(...head)
      if (hidden > 0) out.push({ k: 'hunk', t: hidden > 1 ? tr('⋯ {n} unchanged lines', { n: hidden }) : tr('⋯ 1 unchanged line') })
      out.push(...tail)
    } else out.push(...run)
    run = []
  }
  for (const l of lines) {
    if (l.k === 'ctx') run.push(l)
    else {
      if (run.length) flush(false)
      out.push(l)
    }
  }
  if (run.length) flush(true)
  return out
}

export function diffStats(lines: DiffLine[]) {
  let add = 0
  let del = 0
  for (const l of lines) {
    if (l.k === 'add') add++
    else if (l.k === 'del') del++
  }
  return { add, del }
}
