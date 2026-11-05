// <clipmux-player base-url="…" playback-id="…">: Video.js 10's default skin
// around an hls.js-backed media element, with token refresh.
//
//   import '@clipmux/player/element'
//   const player = document.querySelector('clipmux-player')
//   player.getToken = async () => (await fetch('/my/token')).text()
//
// With getToken and no token, the element asks for the first token itself;
// set token instead to start with one. autoplay and muted pass through to
// the media element.
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
  static observedAttributes = ['base-url', 'playback-id', 'poster', 'autoplay', 'muted']
  #token = ''
  #getToken: GetToken | undefined
  #hls: Record<string, unknown> = {}
  #media: MediaElement | null = null
  #session: TokenSession | null = null
  #loaded = ''
  #fetching = false

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

  // autoplay and muted reach the media element before any source does.
  #flags(media: MediaElement): void {
    for (const flag of ['autoplay', 'muted'] as const) {
      if (this.hasAttribute(flag)) media.setAttribute(flag, '')
      else media.removeAttribute(flag)
    }
    media.muted = this.hasAttribute('muted')
  }

  #load(): void {
    const media = this.#media
    if (media) this.#flags(media)
    const baseUrl = this.getAttribute('base-url')
    const playbackId = this.getAttribute('playback-id')
    if (!media || !baseUrl || !playbackId) return
    const poster = this.getAttribute('poster')
    if (poster) media.poster = poster
    // A signed ID needs its first token before anything loads.
    if (!this.#token && this.#getToken) {
      if (!this.#fetching) {
        this.#fetching = true
        this.#getToken().then(
          (t) => {
            this.#fetching = false
            if (this.#token) return // a token was set meanwhile; it wins
            if (t === null) return this.#emit('clipmux-denied', {})
            this.#token = t
            this.#load()
          },
          () => {
            this.#fetching = false
            this.#emit('clipmux-error', { status: 0 })
          },
        )
      }
      return
    }
    // Reload only when what plays changes; a refreshed token doesn't.
    const loading = `${baseUrl}|${playbackId}|${this.#token}`
    if (loading === this.#loaded) return
    this.#loaded = loading
    this.#session = this.#token
      ? new TokenSession(this.#token, () => this.#getToken?.() ?? Promise.resolve(null), {
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
