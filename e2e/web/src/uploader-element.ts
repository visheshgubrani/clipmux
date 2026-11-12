// <clipmux-uploader create-url>: the app's backend (here the fixture's
// /create-upload) creates the upload and answers with it and the ClipMux URL.
import '@clipmux/uploader/element'
import { expose } from './common.ts'

const params = new URLSearchParams(location.search)
const state = { assetID: '', status: '', progress: 0, errors: [] as string[] }
expose(state)

const el = document.createElement('clipmux-uploader')
el.setAttribute('create-url', params.get('deny') ? '/create-upload?deny=1' : '/create-upload')
el.setAttribute('accept', 'video/*')
el.setAttribute('max-size', params.get('max') ?? String(64 << 20))
el.options = { concurrency: 1 }
// The events are typed: no casts.
el.addEventListener('clipmux-progress', (e) => {
  state.progress = e.detail.bytesUploaded
})
el.addEventListener('clipmux-uploaded', (e) => {
  state.assetID = e.detail.asset.id
  state.status = e.detail.asset.status
})
el.addEventListener('clipmux-error', (e) => state.errors.push(e.detail.code))
document.body.append(el)
