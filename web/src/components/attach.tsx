import { useState } from 'preact/hooks'
import { uploadImage } from '../api'
import { toastError } from '../store'
import { IX, Spinner } from './icons'

export interface Attach {
  key: string
  id?: string
  url: string
  uploading: boolean
}

export function useAttachments() {
  const [atts, setAtts] = useState<Attach[]>([])
  const add = (files: FileList | File[]) => {
    for (const f of Array.from(files)) {
      if (!f.type.startsWith('image/')) continue
      const key = Math.random().toString(36).slice(2)
      const url = URL.createObjectURL(f)
      setAtts((a) => [...a, { key, url, uploading: true }])
      uploadImage(f)
        .then((r) => setAtts((a) => a.map((x) => (x.key === key ? { ...x, id: r.id, uploading: false } : x))))
        .catch((e) => {
          toastError(e)
          setAtts((a) => a.filter((x) => x.key !== key))
        })
    }
  }
  const remove = (key: string) => setAtts((a) => a.filter((x) => x.key !== key))
  const uploading = atts.some((a) => a.uploading)
  const ids = () => atts.filter((a) => a.id).map((a) => a.id!)
  return { atts, setAtts, add, remove, uploading, ids }
}

export function Thumbs({ atts, remove }: { atts: Attach[]; remove: (key: string) => void }) {
  if (!atts.length) return null
  return (
    <div class="thumbs edit">
      {atts.map((a) => (
        <div key={a.key} class="thumb">
          <img src={a.url} alt="" />
          {a.uploading && (
            <span class="thumb-busy">
              <Spinner size={14} />
            </span>
          )}
          <button class="thumb-x" onClick={() => remove(a.key)} aria-label="Remove image">
            <IX size={12} />
          </button>
        </div>
      ))}
    </div>
  )
}

/** Pull image files out of a paste event (screenshots on desktop). */
export function pastedImages(e: ClipboardEvent): File[] {
  return Array.from(e.clipboardData?.files || []).filter((f) => f.type.startsWith('image/'))
}
