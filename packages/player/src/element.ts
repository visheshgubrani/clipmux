// <clipmux-player base-url="…" playback-id="…">: Video.js 10's default skin
// around an hls.js-backed media element, with token refresh.
//
//   import '@clipmux/player/element'
//   const player = document.querySelector('clipmux-player')
//   player.getToken = async () => (await fetch('/my/token')).text()
//   player.token = initialToken
//
// Events: clipmux-token (a new token is in use), clipmux-denied (getToken
// returned null or delivery answered 403; playback stops), clipmux-error
// ({ status }).
import '@videojs/html/video/player'
import '@videojs/html/video/skin'
import '@videojs/html/media/hlsjs-video'
import { type GetToken, TokenSession } from './refresh.ts'
import { clipmuxSource } from './source.ts'

interface MediaElement extends HTMLVideoElement {
  source: unknown
  engine: {
    on(event: string, fn: (event: string, data: { response?: { code?: number } }) => void): void
  } | null
}

export class ClipMuxPlayerElement extends HTMLElement {
  static observedAttributes = ['base-url', 'playback-id', 'poster']
  #token = ''
  #getToken: GetToken | undefined
  #hls: Record<string, unknown> = {}
  #media: MediaElement | null = null
  #session: TokenSession | null = null

  /** The playback token; unset for a public playback ID. */
  get token(): string {
    return this.#session?.current ?? this.#token
  }
  set token(t: string) {
    this.#token = t
    this.#load()
  }

  /** Called on a 401 for a fresh token; return null to stop playback. */
  get getToken(): GetToken | undefined {
    return this.#getToken
  }
  set getToken(fn: GetToken | undefined) {
    this.#getToken = fn
    this.#load()
  }

  /** Extra hls.js options; set before the first load. */
  set hls(options: Record<string, unknown>) {
    this.#hls = options
  }

  /** The <hlsjs-video> element, for its properties and events. */
  get media(): MediaElement | null {
    return this.#media
  }

  connectedCallback(): void {
    if (!this.#media) {
      this.innerHTML =
        '<video-player><video-skin style="display:block;width:100%;aspect-ratio:16/9">' +
        '<hlsjs-video playsinline crossorigin="anonymous"></hlsjs-video></video-skin></video-player>'
      this.#media = this.querySelector('hlsjs-video') as MediaElement | null
      this.#media?.addEventListener('error', () => this.#emit('clipmux-error', { status: 0 }))
    }
    this.#load()
  }

  attributeChangedCallback(): void {
    this.#load()
  }

  #emit(type: string, detail: unknown): void {
    this.dispatchEvent(new CustomEvent(type, { detail, bubbles: true }))
  }

  #load(): void {
    const media = this.#media
    const baseUrl = this.getAttribute('base-url')
    const playbackId = this.getAttribute('playback-id')
    if (!media || !baseUrl || !playbackId) return
    const poster = this.getAttribute('poster')
    if (poster) media.poster = poster
    this.#session = this.#token
      ? new TokenSession(this.#token, this.#getToken, {
          onToken: (token) => this.#emit('clipmux-token', { token }),
          onDenied: () => {
            media.pause()
            this.#emit('clipmux-denied', {})
          },
        })
      : null
    media.source = clipmuxSource(
      { baseUrl, playbackId, token: this.#token, hls: this.#hls },
      this.#session ?? undefined,
    )
  }
}

if (!customElements.get('clipmux-player')) customElements.define('clipmux-player', ClipMuxPlayerElement)

declare global {
  interface HTMLElementTagNameMap {
    'clipmux-player': ClipMuxPlayerElement
  }
}
