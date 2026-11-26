import { useEffect, useMemo, useState } from 'preact/hooks'
import type { Changes, FileChange } from '../types'
import { api } from '../api'
import { parseDiff } from '../diff'
import { DiffView } from './Timeline'
import { IBranch, IChevron, IRefresh, Spinner } from './icons'
import { cls } from '../util'
import { t, ts } from '../i18n'

const statusName: Record<string, string> = { M: 'modified', A: 'added', D: 'deleted', R: 'renamed', '?': 'new', C: 'copied', U: 'conflict' }

function FileRow({ f }: { f: FileChange }) {
  const [open, setOpen] = useState(false)
  const lines = useMemo(() => (open ? parseDiff(f.diff) : []), [open, f.diff])
  const { add, del } = useMemo(() => {
    let add = 0
    let del = 0
    for (const l of f.diff.split('\n')) {
      if (l.startsWith('+') && !l.startsWith('+++')) add++
      else if (l.startsWith('-') && !l.startsWith('---')) del++
    }
    return { add, del }
  }, [f.diff])
  const code = f.status[0] || 'M'
  return (
    <div class={cls('cfile', open && 'open')}>
      <button class="row-btn" onClick={() => setOpen(!open)}>
        <span class={cls('fstat', 's' + (code === '?' ? 'N' : code))} title={statusName[code] ? t(statusName[code]) : f.status}>
          {code === '?' ? 'N' : code}
        </span>
        <span class="detail mono">{f.path}</span>
        <span class="stats">
          {add > 0 && <span class="add">+{add}</span>}
          {del > 0 && <span class="del">−{del}</span>}
        </span>
        <IChevron size={12} class="chev" />
      </button>
      {open && (f.binary ? <div class="dim small pad">{t('Binary file')}</div> : f.diff ? <DiffView lines={lines} /> : <div class="dim small pad">{t('No textual changes')}</div>)}
    </div>
  )
}

export function ChangesView({ url, refreshKey }: { url: string; refreshKey?: unknown }) {
  const [data, setData] = useState<Changes | null>(null)
  const [err, setErr] = useState('')
  const [loading, setLoading] = useState(false)
  const load = async () => {
    setLoading(true)
    try {
      setData(await api<Changes>('GET', url))
      setErr('')
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setLoading(false)
    }
  }
  useEffect(() => {
    load()
  }, [url, refreshKey])

  return (
    <div class="changes page-pad">
      <div class="changes-head">
        {data?.branch && (
          <span class="chip">
            <IBranch size={12} /> {data.branch}
          </span>
        )}
        {data && <span class="dim small">{data.files.length === 1 ? t('1 changed file') : t('{n} changed files', { n: data.files.length })}</span>}
        <span class="grow" />
        <button class="btn small" onClick={load} disabled={loading}>
          {loading ? <Spinner size={12} /> : <IRefresh size={14} />} {t('Refresh')}
        </button>
      </div>
      {err && <div class="error-box">{ts(err)}</div>}
      {data && !data.repo && <div class="dim pad">{t('Not a git repository.')}</div>}
      {data?.commits && data.commits.length > 0 && (
        <div class="commits">
          <div class="group-h">{t('Commits on this branch')}</div>
          {data.commits.map((c) => (
            <div key={c} class="commit mono small">
              {c}
            </div>
          ))}
        </div>
      )}
      {data?.repo && data.files.length === 0 && <div class="dim pad">{t('Working tree clean.')}</div>}
      {data?.files.map((f) => <FileRow key={f.path} f={f} />)}
    </div>
  )
}
