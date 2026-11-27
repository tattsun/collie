// Japanese dictionary, assembled from the per-area files in locales/ja/.
import core, { patterns } from './locales/ja/core'
import account from './locales/ja/account'
import projects from './locales/ja/projects'
import session from './locales/ja/session'
import schedules, { patterns as schedulePatterns } from './locales/ja/schedules'

export const ja: Record<string, string> = { ...account, ...projects, ...session, ...schedules, ...core }
export const jaPatterns = [...patterns, ...schedulePatterns]
