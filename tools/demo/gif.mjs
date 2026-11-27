// Turns recorded PNG frames into a small GIF: one palette for the whole
// clip, identical frames merged into one longer frame, and pixels that did
// not change since the previous frame left transparent (UI recordings are
// mostly still, so this is what keeps the file small).
import fs from 'node:fs'
import gifenc from 'gifenc'
import pngjs from 'pngjs'

const { GIFEncoder, applyPalette, quantize } = gifenc
const { PNG } = pngjs

const decode = (buf) => PNG.sync.read(buf)

// frames: [{ png: Buffer, ms: number }] (ms = how long the frame shows)
export function writeGif(file, frames, { colors = 255 } = {}) {
  const first = decode(frames[0].png)
  const { width, height } = first
  // Palette from a spread of frames, so colors that appear later are in it.
  const picks = [0, 0.25, 0.5, 0.75, 1].map((f) => Math.round(f * (frames.length - 1)))
  const sample = Buffer.concat([...new Set(picks)].map((i) => decode(frames[i].png).data))
  const palette = quantize(new Uint8Array(sample), colors)
  const clear = palette.length // reserved transparent index
  palette.push([0, 0, 0])

  const gif = GIFEncoder()
  let prev = null // indices currently on screen
  let pending = null // { index, ms } waiting for its final duration
  const flush = () => {
    if (!pending) return
    gif.writeFrame(pending.index, width, height, {
      palette: pending.first ? palette : undefined,
      delay: pending.ms,
      transparent: !pending.first,
      transparentIndex: clear,
      dispose: 1,
    })
    pending = null
  }
  for (const [n, f] of frames.entries()) {
    const img = n === 0 ? first : decode(f.png)
    const index = applyPalette(new Uint8Array(img.data.buffer, img.data.byteOffset, img.data.length), palette)
    if (!prev) {
      pending = { index, ms: f.ms, first: true }
      prev = index
      continue
    }
    const delta = new Uint8Array(index.length)
    let changed = 0
    for (let i = 0; i < index.length; i++) {
      if (index[i] === prev[i]) delta[i] = clear
      else {
        delta[i] = index[i]
        changed++
      }
    }
    if (changed === 0) {
      pending.ms += f.ms
      continue
    }
    flush()
    pending = { index: delta, ms: f.ms }
    prev = index
  }
  flush()
  gif.finish()
  fs.writeFileSync(file, gif.bytes())
}
