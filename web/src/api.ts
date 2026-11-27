import { t } from './i18n'
import { authed } from './store'
import { confirmIdentity } from './authflow'
import type { AttachedFile } from './types'

export class ApiError extends Error {
  constructor(message: string, public status: number) {
    super(message)
  }
}

export async function api<T = any>(method: string, path: string, body?: unknown, retried = false): Promise<T> {
  const init: RequestInit = { method, credentials: 'same-origin', headers: {} }
  if (body !== undefined) {
    ;(init.headers as Record<string, string>)['Content-Type'] = 'application/json'
    init.body = JSON.stringify(body)
  }
  const r = await fetch(path, init)
  if (r.status === 401) {
    authed.value = false
    throw new ApiError(t('Not signed in'), 401)
  }
  const data = await r.json().catch(() => ({}))
  // A sensitive change without a recent passkey check: confirm, then retry.
  if (r.status === 403 && data.reauth && !retried) {
    await confirmIdentity()
    return api<T>(method, path, body, true)
  }
  if (!r.ok) throw new ApiError(data.error || r.statusText, r.status)
  return data as T
}

/** Store a `claude setup-token` token in the user's Claude subscription account. */
export async function saveClaudeToken(token: string) {
  await api('POST', '/api/me/claude/token', { token })
}

const MAX_EDGE = 2000

/** Downscale large images client-side so phone photos upload fast. */
async function shrink(file: File): Promise<Blob> {
  if (!/^image\/(png|jpeg|webp)$/.test(file.type) || file.size < 900_000) return file
  try {
    const bmp = await createImageBitmap(file)
    const scale = Math.min(1, MAX_EDGE / Math.max(bmp.width, bmp.height))
    if (scale === 1 && file.size < 3_500_000) return file
    const c = document.createElement('canvas')
    c.width = Math.round(bmp.width * scale)
    c.height = Math.round(bmp.height * scale)
    c.getContext('2d')!.drawImage(bmp, 0, 0, c.width, c.height)
    const blob = await new Promise<Blob | null>((res) => c.toBlob(res, 'image/jpeg', 0.86))
    return blob && blob.size < file.size ? blob : file
  } catch {
    return file
  }
}

/** Attach any other file (documents, spreadsheets…), kept as it is. */
export async function uploadFile(file: File): Promise<AttachedFile> {
  const fd = new FormData()
  fd.append('file', file, file.name || 'file')
  const r = await fetch('/api/files', { method: 'POST', body: fd, credentials: 'same-origin' })
  const data = await r.json().catch(() => ({}))
  if (!r.ok) throw new ApiError(data.error || r.statusText, r.status)
  return data
}

export async function uploadImage(file: File): Promise<{ id: string; url: string }> {
  const fd = new FormData()
  fd.append('file', await shrink(file), file.name || 'image')
  const r = await fetch('/api/uploads', { method: 'POST', body: fd, credentials: 'same-origin' })
  const data = await r.json().catch(() => ({}))
  if (!r.ok) throw new ApiError(data.error || r.statusText, r.status)
  return data
}
