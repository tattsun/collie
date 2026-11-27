import { useState } from 'preact/hooks'
import { t } from '../i18n'
import { uploadFile, uploadImage } from '../api'
import { toastError } from '../store'
import type { AttachedFile } from '../types'
import { IFile, IX, Spinner } from './icons'

/** The server's limit for an attached file (images are shrunk instead). */
const MAX_FILE = 25 << 20

export interface Attach {
  key: string
  /** image: shown to the agent inline; file: given to it as a copy to read */
  kind: 'image' | 'file'
  id?: string
  /** preview of an image */
  url?: string
  name: string
  size: number
  uploading: boolean
}

/** Images the agents look at directly; any other file is passed as a file. */
const inlineImage = (f: File) => /^image\/(png|jpeg|gif|webp)$/.test(f.type)

export function useAttachments() {
  const [atts, setAtts] = useState<Attach[]>([])
  const add = (files: FileList | File[]) => {
    for (const f of Array.from(files)) {
      const kind = inlineImage(f) ? 'image' : 'file'
      if (kind === 'file' && f.size > MAX_FILE) {
        toastError(new Error(t('{name} is larger than 25 MB', { name: f.name })))
        continue
      }
      const key = Math.random().toString(36).slice(2)
      const url = kind === 'image' ? URL.createObjectURL(f) : undefined
      setAtts((a) => [...a, { key, kind, url, name: f.name, size: f.size, uploading: true }])
      ;(kind === 'image' ? uploadImage(f) : uploadFile(f))
        .then((r) => setAtts((a) => a.map((x) => (x.key === key ? { ...x, id: r.id, uploading: false } : x))))
        .catch((e) => {
          toastError(e)
          setAtts((a) => a.filter((x) => x.key !== key))
        })
    }
  }
  const remove = (key: string) => setAtts((a) => a.filter((x) => x.key !== key))
  const uploading = atts.some((a) => a.uploading)
  return { atts, setAtts, add, remove, uploading }
}

/** The message body fields for the attachments. */
export function attachBody(atts: Attach[]) {
  const of = (kind: Attach['kind']) => atts.filter((a) => a.kind === kind && a.id).map((a) => a.id!)
  return { images: of('image'), files: of('file') }
}

export function fileSize(n: number) {
  if (n < 1024) return `${n} B`
  if (n < 1 << 20) return `${Math.round(n / 1024)} KB`
  return `${(n / (1 << 20)).toFixed(1)} MB`
}

export function Thumbs({ atts, remove }: { atts: Attach[]; remove: (key: string) => void }) {
  if (!atts.length) return null
  return (
    <div class="thumbs edit">
      {atts.map((a) =>
        a.kind === 'image' ? (
          <div key={a.key} class="thumb">
            <img src={a.url} alt="" />
            {a.uploading && (
              <span class="thumb-busy">
                <Spinner size={14} />
              </span>
            )}
            <button class="thumb-x" onClick={() => remove(a.key)} aria-label={t('Remove image')}>
              <IX size={12} />
            </button>
          </div>
        ) : (
          <div key={a.key} class="thumb file-chip">
            {a.uploading ? <Spinner size={14} /> : <IFile size={16} />}
            <span class="file-name">{a.name}</span>
            <span class="dim small">{fileSize(a.size)}</span>
            <button class="thumb-x" onClick={() => remove(a.key)} aria-label={t('Remove file')}>
              <IX size={12} />
            </button>
          </div>
        ),
      )}
    </div>
  )
}

/** Files attached to a sent message: each downloads the copy Collie keeps. */
export function FileChips({ files }: { files: AttachedFile[] }) {
  return (
    <div class="thumbs">
      {files.map((f) => (
        <a key={f.id} class="file-chip" href={`/api/files/${encodeURIComponent(f.id)}`} download={f.name} title={t('Download')}>
          <IFile size={16} />
          <span class="file-name">{f.name}</span>
          <span class="dim small">{fileSize(f.size)}</span>
        </a>
      ))}
    </div>
  )
}

/** Pull files out of a paste event (screenshots, files copied in a file manager). */
export function pastedFiles(e: ClipboardEvent): File[] {
  return Array.from(e.clipboardData?.files || [])
}
