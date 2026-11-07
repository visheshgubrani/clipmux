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
// Subtitles listed in the master show in the captions menu. Chapters are a
// track the app names: chapters-track="trk_…" loads its VTT with the
// playback's token and marks the chapters on the time slider.
//
// Events: clipmux-token (a new token is in use), clipmux-denied (getToken
// returned null or delivery answered 403; playback stops), clipmux-error
// ({ status }), clipmux-chapters ({ chapters }, in seconds).
import '@videojs/html/video/player'
import '@videojs/html/video/skin'
import '@videojs/html/media/hlsjs-video'
import { type GetToken, TokenSession } from './refresh.ts'
import { clipmuxSource } from './source.ts'
import { type Chapter, fetchTrack, parseChapters, trackUrl } from './tracks.ts'

interface MediaElement extends HTMLVideoElement {
  source: unknown
  /** The <video> it plays in. */
  target?: HTMLVideoElement | null
  engine: {
    on(event: string, fn: (event: string, data: { response?: { code?: number } }) => void): void
  } | null
}

export class ClipMuxPlayerElement extends HTMLElement {
  static observedAttributes = ['base-url', 'playback-id', 'poster', 'autoplay', 'muted', 'chapters-track']
  #token = ''
  #getToken: GetToken | undefined
  #hls: Record<string, unknown> = {}
  #media: MediaElement | null = null
  #session: TokenSession | null = null
  #loaded = ''
  #fetching = false
  #chaptersLoaded = ''
  #chapters: Chapter[] = []
  #chaptersTrack: TextTrack | null = null

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

  /** The chapters of chapters-track, in seconds, once loaded. */
  get chapters(): Chapter[] {
    return this.#chapters
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
    if (loading !== this.#loaded) this.#play(media, baseUrl, playbackId, loading)
    this.#loadChapters(media, baseUrl, playbackId)
  }

  #play(media: MediaElement, baseUrl: string, playbackId: string, loading: string): void {
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

  // The chapters become the cues of a hidden chapters text track of the
  // <video> itself, which the skin's time slider reads. It is made with
  // addTextTrack: a <track> without a src empties its cues when it fails to
  // load, and a light-DOM one is copied into the <video> without them.
  // hls.js clears every text track's cues as its manifest loads, so the
  // cues are added once the media has metadata.
  #loadChapters(media: MediaElement, baseUrl: string, playbackId: string): void {
    const trackId = this.getAttribute('chapters-track') ?? ''
    const key = `${baseUrl}|${playbackId}|${trackId}`
    if (key === this.#chaptersLoaded) return
    this.#chaptersLoaded = key
    this.#dropChapters()
    if (!trackId) return
    fetchTrack(
      (t) => trackUrl(baseUrl, playbackId, trackId, t),
      this.#session,
      this.#token || undefined,
    ).then(
      (vtt) => {
        if (key !== this.#chaptersLoaded) return
        this.#chapters = parseChapters(vtt)
        const add = () => {
          if (key !== this.#chaptersLoaded) return
          const video = media.target ?? media
          const track = video.addTextTrack('chapters', 'Chapters')
          track.mode = 'hidden'
          for (const c of this.#chapters) track.addCue(new VTTCue(c.startTime, c.endTime, c.title))
          this.#chaptersTrack = track
          this.#emit('clipmux-chapters', { chapters: this.#chapters })
        }
        if (media.readyState >= HTMLMediaElement.HAVE_METADATA) add()
        else media.addEventListener('loadedmetadata', add, { once: true })
      },
      () => this.#emit('clipmux-error', { status: 0, track: trackId }),
    )
  }

  // A text track can't be removed: the old one is emptied and disabled.
  #dropChapters(): void {
    const old = this.#chaptersTrack
    if (old) {
      for (const cue of Array.from(old.cues ?? [])) old.removeCue(cue)
      old.mode = 'disabled'
    }
    this.#chaptersTrack = null
    this.#chapters = []
  }
}

if (!customElements.get('clipmux-player')) customElements.define('clipmux-player', ClipMuxPlayerElement)

declare global {
  interface HTMLElementTagNameMap {
    'clipmux-player': ClipMuxPlayerElement
  }
}
