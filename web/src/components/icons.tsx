import type { JSX } from 'preact'

type P = { size?: number; class?: string }

function svg(path: JSX.Element, { size = 16, class: c }: P) {
  return (
    <svg class={'icon ' + (c || '')} width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
      {path}
    </svg>
  )
}

export const IPlus = (p: P) => svg(<path d="M12 5v14M5 12h14" />, p)
export const IBack = (p: P) => svg(<path d="M15 18l-6-6 6-6" />, p)
export const IChevron = (p: P) => svg(<path d="M9 18l6-6-6-6" />, p)
export const IDown = (p: P) => svg(<path d="M6 9l6 6 6-6" />, p)
export const ISend = (p: P) => svg(<path d="M12 19V5M5 12l7-7 7 7" />, p)
export const IStop = (p: P) => svg(<rect x="6" y="6" width="12" height="12" rx="2" fill="currentColor" stroke="none" />, p)
export const IImage = (p: P) =>
  svg(
    <>
      <rect x="3" y="3" width="18" height="18" rx="2" />
      <circle cx="8.5" cy="8.5" r="1.5" />
      <path d="M21 15l-5-5L5 21" />
    </>,
    p,
  )
export const ITerminal = (p: P) => svg(<path d="M4 17l6-6-6-6M12 19h8" />, p)
export const IFile = (p: P) =>
  svg(
    <>
      <path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z" />
      <path d="M14 2v6h6" />
    </>,
    p,
  )
export const IEdit = (p: P) => svg(<path d="M12 20h9M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4z" />, p)
export const ISearch = (p: P) =>
  svg(
    <>
      <circle cx="11" cy="11" r="7" />
      <path d="M21 21l-4.3-4.3" />
    </>,
    p,
  )
export const IGlobe = (p: P) =>
  svg(
    <>
      <circle cx="12" cy="12" r="10" />
      <path d="M2 12h20M12 2a15 15 0 0 1 0 20M12 2a15 15 0 0 0 0 20" />
    </>,
    p,
  )
export const IBot = (p: P) =>
  svg(
    <>
      <rect x="4" y="8" width="16" height="12" rx="2" />
      <path d="M12 4v4M9 13h.01M15 13h.01" />
    </>,
    p,
  )
export const IList = (p: P) => svg(<path d="M9 6h11M9 12h11M9 18h11M4 6h.01M4 12h.01M4 18h.01" />, p)
export const IPlug = (p: P) => svg(<path d="M9 2v6M15 2v6M6 8h12v4a6 6 0 0 1-12 0zM12 18v4" />, p)
export const IBolt = (p: P) => svg(<path d="M13 2L3 14h9l-1 8 10-12h-9z" />, p)
export const ICheck = (p: P) => svg(<path d="M20 6L9 17l-5-5" />, p)
export const IX = (p: P) => svg(<path d="M18 6L6 18M6 6l12 12" />, p)
export const IMore = (p: P) =>
  svg(
    <>
      <circle cx="5" cy="12" r="1" />
      <circle cx="12" cy="12" r="1" />
      <circle cx="19" cy="12" r="1" />
    </>,
    p,
  )
export const IFolder = (p: P) => svg(<path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z" />, p)
export const IBranch = (p: P) =>
  svg(
    <>
      <circle cx="6" cy="5" r="2" />
      <circle cx="6" cy="19" r="2" />
      <circle cx="18" cy="7" r="2" />
      <path d="M6 7v10M18 9a8 8 0 0 1-8 8" />
    </>,
    p,
  )
export const IDiff = (p: P) => svg(<path d="M12 3v14M5 10h14M5 21h14" />, p)
export const IChat = (p: P) => svg(<path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z" />, p)
export const IBrain = (p: P) => svg(<path d="M12 4a3 3 0 0 0-3 3 3 3 0 0 0-3 3 3 3 0 0 0 1 5 3 3 0 0 0 5 3V4zM12 4a3 3 0 0 1 3 3 3 3 0 0 1 3 3 3 3 0 0 1-1 5 3 3 0 0 1-5 3" />, p)
export const IShield = (p: P) => svg(<path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z" />, p)
export const IRefresh = (p: P) => svg(<path d="M21 12a9 9 0 1 1-3-6.7L21 8M21 3v5h-5" />, p)
export const IBell = (p: P) => svg(<path d="M18 8a6 6 0 0 0-12 0c0 7-3 9-3 9h18s-3-2-3-9M13.7 21a2 2 0 0 1-3.4 0" />, p)
export const ITrash = (p: P) => svg(<path d="M3 6h18M8 6V4h8v2M19 6l-1 14H6L5 6" />, p)
export const IArchive = (p: P) => svg(<path d="M21 8v13H3V8M1 3h22v5H1zM10 12h4" />, p)
export const ISpark = (p: P) => svg(<path d="M12 3l2 5 5 2-5 2-2 5-2-5-5-2 5-2z" />, p)

export function Spinner({ size = 14 }: { size?: number }) {
  return <span class="spinner" style={{ width: size, height: size }} />
}
export const IBox = (p: P) =>
  svg(
    <>
      <path d="M21 8l-9-5-9 5 9 5 9-5z" />
      <path d="M3 8v8l9 5 9-5V8M12 13v8" />
    </>,
    p,
  )
export const IAlert = (p: P) => svg(<path d="M12 9v4M12 17h.01M10.3 3.9L1.8 18a2 2 0 0 0 1.7 3h17a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0z" />, p)
export const ISettings = (p: P) =>
  svg(
    <>
      <circle cx="12" cy="12" r="3" />
      <path d="M19.4 15a1.7 1.7 0 0 0 .3 1.8l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.7 1.7 0 0 0-1.8-.3 1.7 1.7 0 0 0-1 1.5V21a2 2 0 1 1-4 0v-.1a1.7 1.7 0 0 0-1.1-1.5 1.7 1.7 0 0 0-1.8.3l-.1.1a2 2 0 1 1-2.8-2.8l.1-.1a1.7 1.7 0 0 0 .3-1.8 1.7 1.7 0 0 0-1.5-1H3a2 2 0 1 1 0-4h.1a1.7 1.7 0 0 0 1.5-1.1 1.7 1.7 0 0 0-.3-1.8l-.1-.1a2 2 0 1 1 2.8-2.8l.1.1a1.7 1.7 0 0 0 1.8.3H9a1.7 1.7 0 0 0 1-1.5V3a2 2 0 1 1 4 0v.1a1.7 1.7 0 0 0 1 1.5 1.7 1.7 0 0 0 1.8-.3l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1a1.7 1.7 0 0 0-.3 1.8V9a1.7 1.7 0 0 0 1.5 1H21a2 2 0 1 1 0 4h-.1a1.7 1.7 0 0 0-1.5 1z" />
    </>,
    p,
  )
