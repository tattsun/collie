import type { JSX } from 'preact'
import { IBot, IEdit, IFile, IGlobe, IList, IPlug, ISearch, ISpark, ITerminal, IBolt, IImage } from './icons'
import { rel as relTo, firstLine, home } from '../util'
import { t } from '../i18n'

const rel = (p: string | undefined, base?: string) => home(relTo(p, base))

export interface ToolView {
  icon: (p: { size?: number }) => JSX.Element
  label: string
  detail: string
  mono?: boolean
}

const s = (v: unknown) => (typeof v === 'string' ? v : '')

export function describeTool(name: string, input: any, cwd?: string): ToolView {
  const i = input && typeof input === 'object' ? input : {}
  const path = (k = 'file_path') => rel(s(i[k]) || s(i.path) || s(i.notebook_path), cwd)
  switch (name) {
    case 'Bash':
    case 'BashOutput':
      return { icon: ITerminal, label: t('Run'), detail: firstLine(s(i.command) || s(i.bash_id), 200), mono: true }
    case 'Read':
      return { icon: IFile, label: t('Read'), detail: path() + (i.offset ? `:${i.offset}` : ''), mono: true }
    case 'Write':
      return { icon: IEdit, label: t('Write'), detail: path(), mono: true }
    case 'Edit':
    case 'MultiEdit':
      return { icon: IEdit, label: t('Edit'), detail: path(), mono: true }
    case 'NotebookEdit':
      return { icon: IEdit, label: t('Edit notebook'), detail: path('notebook_path'), mono: true }
    case 'Patch': {
      const files: string[] = Array.isArray(i.files) ? i.files.map((f: string) => rel(f, cwd)) : []
      return { icon: IEdit, label: files.length > 1 ? t('Edit {n} files', { n: files.length }) : t('Edit'), detail: files.join(', '), mono: true }
    }
    case 'Grep':
      return { icon: ISearch, label: t('Search'), detail: s(i.pattern) + (i.path ? '  ' + t('in {path}', { path: rel(s(i.path), cwd) }) : ''), mono: true }
    case 'Glob':
      return { icon: ISearch, label: t('Find files'), detail: s(i.pattern), mono: true }
    case 'LS':
      return { icon: IFile, label: t('List'), detail: path('path'), mono: true }
    case 'WebFetch':
      return { icon: IGlobe, label: t('Fetch'), detail: s(i.url) }
    case 'WebSearch':
      return { icon: IGlobe, label: t('Web search'), detail: s(i.query) }
    case 'Task':
    case 'Agent':
      return { icon: IBot, label: s(i.subagent_type) || t('Agent'), detail: s(i.description) || firstLine(s(i.prompt)) }
    case 'TodoWrite':
      return { icon: IList, label: t('Update plan'), detail: '' }
    case 'Skill':
      return { icon: ISpark, label: t('Skill'), detail: s(i.skill) || s(i.name) || s(i.command) }
    case 'ViewImage':
      return { icon: IImage, label: t('View image'), detail: path('path'), mono: true }
    case 'ToolSearch':
      return { icon: ISearch, label: t('Find tools'), detail: s(i.query) }
  }
  if (name === 'mcp__collie__show_media') {
    const files = Array.isArray(i.files) ? i.files : []
    const names = files.map((f: any) => (typeof f === 'string' ? f : s(f?.path)).split('/').pop()).filter(Boolean)
    return { icon: IImage, label: t('Show files'), detail: s(i.title) || names.join(', ') }
  }
  if (name.startsWith('mcp__')) {
    const [, server, ...rest] = name.split('__')
    return { icon: IPlug, label: `${server} · ${rest.join('__')}`, detail: firstString(i) }
  }
  return { icon: IBolt, label: name, detail: firstString(i) }
}

function firstString(i: Record<string, unknown>): string {
  for (const k of ['command', 'description', 'query', 'url', 'path', 'file_path', 'pattern', 'prompt', 'name']) {
    if (typeof i[k] === 'string' && i[k]) return firstLine(i[k] as string)
  }
  for (const v of Object.values(i)) if (typeof v === 'string' && v) return firstLine(v)
  return ''
}
