// <clipmux-uploader create-url="/api/clipmux/upload">: a drop zone that
// uploads one file through ClipMuxUpload with createUrl, with progress, cancel
// and retry. No dependencies.
//
//   import '@clipmux/uploader/element'
//   document.querySelector('clipmux-uploader')!
//     .addEventListener('clipmux-uploaded', (e) => console.log(e.detail.asset.id))
//
// Attributes: create-url (your backend's route that creates the upload),
// accept (as on <input type=file>), max-size (bytes). The host's state
// attribute is idle, uploading, done or error, for styling.
//
// Styling: the parts dropzone, label, button, progress, bar, status, cancel
// and retry, and the custom properties --clipmux-uploader-accent, -border,
// -radius, -background, -color and -font. The default slot replaces the
// drop zone's text.
//
// Events (bubbling): clipmux-progress (Progress), clipmux-uploaded
// ({ asset }), clipmux-error ({ code, message, status, error }).
import { accepts, formatBytes } from './files.ts'
import { type Asset, ClipMuxUpload, ClipMuxUploadError, type Progress, type UploadOptions } from './index.ts'

export interface ClipMuxUploadedDetail {
  asset: Asset
}
export interface ClipMuxUploadErrorDetail {
  /** ClipMuxUploadError's code, or file_too_large / file_type_rejected. */
  code: string
  message: string
  status: number
  error: ClipMuxUploadError
}
export type ClipMuxProgressEvent = CustomEvent<Progress>
export type ClipMuxUploadedEvent = CustomEvent<ClipMuxUploadedDetail>
export type ClipMuxUploadErrorEvent = CustomEvent<ClipMuxUploadErrorDetail>

/** The events of <clipmux-uploader>; they bubble. */
export interface ClipMuxUploaderEventMap {
  'clipmux-progress': ClipMuxProgressEvent
  'clipmux-uploaded': ClipMuxUploadedEvent
  'clipmux-error': ClipMuxUploadErrorEvent
}

/**
 * The attributes and properties of <clipmux-uploader> for JSX, without the
 * framework's own; @clipmux/uploader/react adds them to React's
 * IntrinsicElements.
 */
export interface ClipMuxUploaderProps {
  'create-url'?: string | undefined
  accept?: string | undefined
  'max-size'?: number | string | undefined
  options?: UploaderOptions | undefined
  'onclipmux-progress'?: ((event: ClipMuxProgressEvent) => void) | undefined
  'onclipmux-uploaded'?: ((event: ClipMuxUploadedEvent) => void) | undefined
  'onclipmux-error'?: ((event: ClipMuxUploadErrorEvent) => void) | undefined
}

/** Extra ClipMuxUpload options for the element's uploads. */
export type UploaderOptions = Omit<UploadOptions, 'upload' | 'createUrl' | 'onProgress' | 'signal'>

export type UploaderState = 'idle' | 'uploading' | 'done' | 'error'

const template = `
<style>
  :host {
    display: block;
    font: var(--clipmux-uploader-font, inherit);
    color: var(--clipmux-uploader-color, inherit);
  }
  :host([hidden]) { display: none; }
  [part=dropzone] {
    display: flex;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    gap: 0.75rem;
    box-sizing: border-box;
    min-height: 10rem;
    padding: 1.5rem;
    border: var(--clipmux-uploader-border, 2px dashed color-mix(in srgb, currentColor 30%, transparent));
    border-radius: var(--clipmux-uploader-radius, 0.75rem);
    background: var(--clipmux-uploader-background, transparent);
    text-align: center;
  }
  :host([dragging]) [part=dropzone] {
    border-color: var(--clipmux-uploader-accent, #2563eb);
  }
  [part=button], [part=cancel], [part=retry] {
    font: inherit;
    padding: 0.4rem 0.9rem;
    border: 1px solid color-mix(in srgb, currentColor 30%, transparent);
    border-radius: calc(var(--clipmux-uploader-radius, 0.75rem) / 2);
    background: transparent;
    color: inherit;
    cursor: pointer;
  }
  [part=progress] {
    width: 100%;
    max-width: 24rem;
    height: 0.5rem;
    border-radius: 999px;
    overflow: hidden;
    background: color-mix(in srgb, currentColor 12%, transparent);
  }
  [part=bar] {
    height: 100%;
    width: 0;
    background: var(--clipmux-uploader-accent, #2563eb);
    transition: width 0.2s;
  }
  [part=status] { margin: 0; min-height: 1.25em; }
  [hidden] { display: none !important; }
  input { display: none; }
</style>
<div part="dropzone">
  <span part="label"><slot>Drop a video here, or</slot></span>
  <button part="button" type="button">Choose a file</button>
  <input type="file">
  <div part="progress" role="progressbar" aria-valuemin="0" aria-valuemax="100" aria-valuenow="0" hidden>
    <div part="bar"></div>
  </div>
  <p part="status" aria-live="polite"></p>
  <button part="cancel" type="button" hidden>Cancel</button>
  <button part="retry" type="button" hidden>Retry</button>
</div>`

export class ClipMuxUploaderElement extends HTMLElement {
  static observedAttributes = ['accept']
  /** Extra options for each upload (concurrency, endpoint, store, …). */
  options: UploaderOptions = {}
  #root: ShadowRoot
  #input: HTMLInputElement
  #upload: ClipMuxUpload | null = null
  #file: File | null = null
  #state: UploaderState = 'idle'
  #asset: Asset | null = null

  constructor() {
    super()
    this.#root = this.attachShadow({ mode: 'open' })
    this.#root.innerHTML = template
    this.#input = this.#part('input') as HTMLInputElement
    this.#part('[part=button]').addEventListener('click', () => this.#input.click())
    this.#input.addEventListener('change', () => {
      const file = this.#input.files?.[0]
      this.#input.value = ''
      if (file) void this.upload(file)
    })
    this.#part('[part=cancel]').addEventListener('click', () => void this.cancel())
    this.#part('[part=retry]').addEventListener('click', () => void this.retry())
    const zone = this.#part('[part=dropzone]')
    zone.addEventListener('dragover', (e) => {
      e.preventDefault()
      if (this.#state !== 'uploading') this.toggleAttribute('dragging', true)
    })
    zone.addEventListener('dragleave', () => this.removeAttribute('dragging'))
    zone.addEventListener('drop', (e) => {
      e.preventDefault()
      this.removeAttribute('dragging')
      const file = e.dataTransfer?.files[0]
      if (file && this.#state !== 'uploading') void this.upload(file)
    })
  }

  // The element's own event types. clipmux-error is not added to the global
  // HTMLElementEventMap, where @clipmux/player's has another detail.
  override addEventListener<K extends keyof ClipMuxUploaderEventMap>(
    type: K,
    listener: (this: ClipMuxUploaderElement, ev: ClipMuxUploaderEventMap[K]) => unknown,
    options?: boolean | AddEventListenerOptions,
  ): void
  override addEventListener<K extends keyof HTMLElementEventMap>(
    type: K,
    listener: (this: ClipMuxUploaderElement, ev: HTMLElementEventMap[K]) => unknown,
    options?: boolean | AddEventListenerOptions,
  ): void
  override addEventListener(
    type: string,
    listener: EventListenerOrEventListenerObject,
    options?: boolean | AddEventListenerOptions,
  ): void
  override addEventListener(
    type: string,
    listener: EventListenerOrEventListenerObject | ((ev: never) => unknown),
    options?: boolean | AddEventListenerOptions,
  ): void {
    super.addEventListener(type, listener as EventListenerOrEventListenerObject, options)
  }
  override removeEventListener<K extends keyof ClipMuxUploaderEventMap>(
    type: K,
    listener: (this: ClipMuxUploaderElement, ev: ClipMuxUploaderEventMap[K]) => unknown,
    options?: boolean | EventListenerOptions,
  ): void
  override removeEventListener<K extends keyof HTMLElementEventMap>(
    type: K,
    listener: (this: ClipMuxUploaderElement, ev: HTMLElementEventMap[K]) => unknown,
    options?: boolean | EventListenerOptions,
  ): void
  override removeEventListener(
    type: string,
    listener: EventListenerOrEventListenerObject,
    options?: boolean | EventListenerOptions,
  ): void
  override removeEventListener(
    type: string,
    listener: EventListenerOrEventListenerObject | ((ev: never) => unknown),
    options?: boolean | EventListenerOptions,
  ): void {
    super.removeEventListener(type, listener as EventListenerOrEventListenerObject, options)
  }

  /** idle, uploading, done or error. */
  get state(): UploaderState {
    return this.#state
  }

  /** The file being (or last) uploaded. */
  get file(): File | null {
    return this.#file
  }

  /** The asset, once the upload completes. */
  get asset(): Asset | null {
    return this.#asset
  }

  connectedCallback(): void {
    this.#show(this.#state)
  }

  attributeChangedCallback(): void {
    const accept = this.getAttribute('accept')
    if (accept) this.#input.accept = accept
    else this.#input.removeAttribute('accept')
  }

  /** Uploads file, as choosing or dropping it does. Resolves with the asset, or null. */
  async upload(file: File): Promise<Asset | null> {
    if (this.#state === 'uploading') return null
    this.#file = file
    this.#asset = null
    const maxSize = Number(this.getAttribute('max-size') ?? '')
    if (!accepts(this.getAttribute('accept'), file)) {
      return this.#fail(
        new ClipMuxUploadError('file_type_rejected', `${file.name} is not a file this accepts`),
      )
    }
    if (maxSize > 0 && file.size > maxSize) {
      return this.#fail(
        new ClipMuxUploadError('file_too_large', `${file.name} is larger than ${formatBytes(maxSize)}`),
      )
    }
    const createUrl = this.getAttribute('create-url')
    if (!createUrl) {
      return this.#fail(new ClipMuxUploadError('no_create_url', 'clipmux-uploader needs a create-url'))
    }
    this.#upload = new ClipMuxUpload(file, {
      ...this.options,
      createUrl,
      onProgress: (p) => this.#progress(p),
    })
    return this.#run()
  }

  /** Tries the last failed upload again, from what storage holds. */
  async retry(): Promise<Asset | null> {
    if (this.#state !== 'error' || !this.#file) return null
    if (!this.#upload) return this.upload(this.#file)
    return this.#run()
  }

  /** Stops the upload for good and tells ClipMux. */
  async cancel(): Promise<void> {
    const up = this.#upload
    if (!up || this.#state !== 'uploading') return
    this.#upload = null
    this.#show('idle', 'Cancelled')
    await up.cancel()
  }

  async #run(): Promise<Asset | null> {
    const up = this.#upload as ClipMuxUpload
    const name = this.#file?.name ?? ''
    this.#show('uploading', `Uploading ${name}…`)
    this.#bar(0)
    try {
      const asset = await up.start()
      if (up !== this.#upload) return null
      this.#asset = asset
      this.#bar(100)
      this.#show('done', `Uploaded ${name}`)
      this.#emit('clipmux-uploaded', { asset })
      return asset
    } catch (err) {
      if (up !== this.#upload) return null // cancelled
      return this.#fail(
        err instanceof ClipMuxUploadError
          ? err
          : new ClipMuxUploadError('failed', err instanceof Error ? err.message : String(err)),
      )
    }
  }

  #fail(error: ClipMuxUploadError): null {
    // A file that was never sent has nothing to retry from.
    if (['file_type_rejected', 'file_too_large', 'no_create_url'].includes(error.code)) this.#upload = null
    this.#show('error', error.message)
    this.#emit('clipmux-error', { code: error.code, message: error.message, status: error.status, error })
    return null
  }

  #progress(p: Progress): void {
    if (this.#state !== 'uploading') return
    const pct = p.bytesTotal ? Math.floor((p.bytesUploaded / p.bytesTotal) * 100) : 0
    this.#bar(pct)
    this.#status(`Uploading ${this.#file?.name ?? ''}… ${pct}%`)
    this.#emit('clipmux-progress', p)
  }

  #show(state: UploaderState, status?: string): void {
    this.#state = state
    this.setAttribute('state', state)
    this.#part('[part=progress]').hidden = state !== 'uploading' && state !== 'done'
    this.#part('[part=cancel]').hidden = state !== 'uploading'
    this.#part('[part=retry]').hidden = state !== 'error' || !this.#upload
    this.#part('[part=button]').hidden = state === 'uploading'
    if (status !== undefined) this.#status(status)
  }

  #bar(pct: number): void {
    ;(this.#part('[part=bar]') as HTMLElement).style.width = `${pct}%`
    this.#part('[part=progress]').setAttribute('aria-valuenow', String(pct))
  }

  #status(text: string): void {
    this.#part('[part=status]').textContent = text
  }

  #part(selector: string): HTMLElement {
    return this.#root.querySelector(selector) as HTMLElement
  }

  #emit<K extends keyof ClipMuxUploaderEventMap>(
    type: K,
    detail: ClipMuxUploaderEventMap[K]['detail'],
  ): void {
    this.dispatchEvent(new CustomEvent(type, { detail, bubbles: true }))
  }
}

if (!customElements.get('clipmux-uploader')) customElements.define('clipmux-uploader', ClipMuxUploaderElement)

declare global {
  interface HTMLElementTagNameMap {
    'clipmux-uploader': ClipMuxUploaderElement
  }
}
