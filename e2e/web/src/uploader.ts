// @clipmux/uploader resuming across a reload.
import { ClipMuxUpload, type CreatedUpload, rememberUpload } from '@clipmux/uploader'
import { config, expose } from './common.ts'

const cfg = await config()
const size = 70 * 1024 * 1024
// The same bytes, name and date on every load, as a re-picked file would be.
const bytes = cfg.source_url
  ? new Uint8Array(await (await fetch(cfg.source_url)).arrayBuffer())
  : new Uint8Array(size)
if (!cfg.source_url) {
  for (let i = 0; i < size; i += 4096) bytes[i] = (i / 4096) & 0xff
}
const file = new File([bytes], cfg.source_url ? 'exit-suite.mp4' : 'exit-suite.bin', {
  lastModified: 1700000000000,
})
const memory = rememberUpload(localStorage)
const state = { resumed: false, paused: false, status: '', error: '', assetID: '' }
expose(state)

let upload: CreatedUpload | null = memory.load(file)
state.resumed = upload !== null
if (!upload) {
  upload = (await (await fetch(`/uploads?size=${size}`, { method: 'POST' })).json()) as CreatedUpload
  memory.save(file, upload)
}
const up: ClipMuxUpload = new ClipMuxUpload(file, {
  endpoint: cfg.api,
  upload,
  concurrency: 1,
  onProgress: (p) => {
    // The first visit stops after one part, as a closed tab would.
    if (!state.resumed && p.partsDone === 1 && !state.paused) {
      state.paused = true
      up.pause()
    }
  },
})
up.start().then(
  (asset) => {
    state.assetID = asset.id
    state.status = asset.status
    memory.forget(file)
  },
  (err: Error) => {
    state.error = err.message
  },
)
