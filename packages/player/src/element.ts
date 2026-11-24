// <clipmux-player base-url="…" playback-id="…">: Video.js 10's default skin
// around an hls.js-backed media element, with token refresh.
//
//   import '@clipmux/player/element'
//   const player = document.querySelector('clipmux-player')
//   player.getToken = async ({ playbackId, native }) =>
//     (await fetch(`/my/token?id=${playbackId}&native=${native}`)).text()
//
// With getToken and no token, the element asks for the first token itself;
// set token instead to start with one. autoplay and muted pass through to
// the media element.
//
// Or let the app's backend answer for it: session-url="/api/clipmux/playback"
// POSTs {playback_id, native} there for the first load and every refresh,
// and takes base_url, the token, the poster and the tracks from the answer
// (session.ts). base-url isn't needed then; a getToken set as well wins for
// refreshes.
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
import { chaptersTrackOf, fetchSession, type PlaybackSession, SessionError } from './session.ts'
import { clipmuxSource, playsNatively } from './source.ts'
import { fetchTrack, type PlayerChapter, parseChapters, trackUrl } from './tracks.ts'

// A generated track ends at the asset duration. HLS can report a slightly
// longer duration as it loads, which would leave a sliver on the seek bar.
const CHAPTER_END_TOLERANCE = 0.25

interface MediaElement extends HTMLVideoElement {
  source: unknown
  /** The <video> it plays in. */
  target?: HTMLVideoElement | null
  engine: {
    on(event: string, fn: (event: string, data: { response?: { code?: number } }) => void): void
  } | null
}

interface ChaptersRequest {
  trackId: string
  token: string
  status: 'loading' | 'ready' | 'failed'
  announced: boolean
}

export interface ClipMuxTokenDetail {
  token: string
}
export interface ClipMuxErrorDetail {
  /** The HTTP status that failed, or 0 for a network or media error. */
  status: number
  /** Set when a track's VTT failed to load. */
  track?: string
}
export interface ClipMuxChaptersDetail {
  chapters: PlayerChapter[]
}
export type ClipMuxTokenEvent = CustomEvent<ClipMuxTokenDetail>
export type ClipMuxDeniedEvent = CustomEvent<Record<string, never>>
export type ClipMuxErrorEvent = CustomEvent<ClipMuxErrorDetail>
export type ClipMuxChaptersEvent = CustomEvent<ClipMuxChaptersDetail>

/** The events of <clipmux-player>; they bubble. */
export interface ClipMuxPlayerEventMap {
  'clipmux-token': ClipMuxTokenEvent
  'clipmux-denied': ClipMuxDeniedEvent
  'clipmux-error': ClipMuxErrorEvent
  'clipmux-chapters': ClipMuxChaptersEvent
}

/**
 * The attributes and properties of <clipmux-player> for JSX, without the
 * framework's own; @clipmux/player/react adds them to React's
 * IntrinsicElements.
 */
export interface ClipMuxPlayerProps {
  'playback-id'?: string | undefined
  'session-url'?: string | undefined
  'base-url'?: string | undefined
  poster?: string | undefined
  'chapters-track'?: string | undefined
  autoplay?: boolean | undefined
  muted?: boolean | undefined
  token?: string | undefined
  getToken?: GetToken | undefined
  hls?: Record<string, unknown> | undefined
  'onclipmux-token'?: ((event: ClipMuxTokenEvent) => void) | undefined
  'onclipmux-denied'?: ((event: ClipMuxDeniedEvent) => void) | undefined
  'onclipmux-error'?: ((event: ClipMuxErrorEvent) => void) | undefined
  'onclipmux-chapters'?: ((event: ClipMuxChaptersEvent) => void) | undefined
}

export class ClipMuxPlayerElement extends HTMLElement {
  static observedAttributes = [
    'base-url',
    'playback-id',
    'session-url',
    'poster',
    'autoplay',
    'muted',
    'chapters-track',
  ]
  #token = ''
  #getToken: GetToken | undefined
  #hls: Record<string, unknown> = {}
  #media: MediaElement | null = null
  #session: TokenSession | null = null
  #loaded = ''
  #fetching = false
  // session-url: the answer in use, the url|playback ID it answers, and
  // the one being asked for.
  #playback: PlaybackSession | null = null
  #playbackFor = ''
  #asking: { key: string } | null = null
  #chaptersRequest: ChaptersRequest | null = null
  #chapters: PlayerChapter[] = []
  #chaptersTrack: { video: HTMLVideoElement; track: TextTrack } | null = null

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

  /** The session-url answer in use, once there is one. */
  get session(): PlaybackSession | null {
    return this.#playback
  }

  /** The chapters of chapters-track, in seconds, once loaded. */
  get chapters(): PlayerChapter[] {
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
      this.#media?.addEventListener('loadedmetadata', () => this.#showChapters())
      this.#media?.addEventListener('durationchange', () => this.#showChapters())
    }
    this.#load()
  }

  attributeChangedCallback(): void {
    this.#load()
  }

  #emit<K extends keyof ClipMuxPlayerEventMap>(type: K, detail: ClipMuxPlayerEventMap[K]['detail']): void {
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
    const sessionUrl = this.getAttribute('session-url')
    if (sessionUrl) {
      this.#loadSession(sessionUrl)
      return
    }
    this.#asking = null
    const baseUrl = this.getAttribute('base-url')
    const playbackId = this.getAttribute('playback-id')
    if (!media) return
    if (!baseUrl || !playbackId) {
      this.#clearChapters()
      return
    }
    const poster = this.getAttribute('poster')
    if (poster) media.poster = poster
    // A signed ID needs its first token before anything loads.
    if (!this.#token && this.#getToken) {
      this.#clearChapters()
      if (!this.#fetching) {
        this.#fetching = true
        this.#getToken({ playbackId, native: playsNatively() }).then(
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
    this.#loadChapters(baseUrl, playbackId, this.getAttribute('chapters-track') ?? '')
  }

  // session-url: each playback ID gets its session first; then it plays
  // like any other, from the session's base URL, playback ID and token.
  #loadSession(sessionUrl: string): void {
    const media = this.#media
    const playbackId = this.getAttribute('playback-id')
    if (!media) return
    if (!playbackId) {
      this.#asking = null
      this.#clearChapters()
      return
    }
    const key = `${sessionUrl}|${playbackId}`
    if (key !== this.#playbackFor) {
      if (key === this.#asking?.key) return
      this.#clearChapters()
      const request = { key }
      this.#asking = request
      fetchSession(sessionUrl, playbackId, playsNatively()).then(
        (s) => {
          if (request !== this.#asking) return // the playback changed meanwhile
          this.#asking = null
          if (s === null) return this.#emit('clipmux-denied', {})
          this.#playback = s
          this.#playbackFor = key
          this.#token = s.token
          this.#load()
        },
        (err: unknown) => {
          if (request !== this.#asking) return
          this.#asking = null
          this.#emit('clipmux-error', { status: err instanceof SessionError ? err.status : 0 })
        },
      )
      return
    }
    this.#asking = null
    const s = this.#playback as PlaybackSession
    const baseUrl = this.getAttribute('base-url') ?? s.base_url
    const poster = this.getAttribute('poster') ?? s.poster_url
    if (poster) media.poster = poster
    const loading = `${baseUrl}|${s.playback_id}|${this.#token}`
    if (loading !== this.#loaded) this.#play(media, baseUrl, s.playback_id, loading)
    this.#loadChapters(baseUrl, s.playback_id, this.getAttribute('chapters-track') ?? chaptersTrackOf(s))
  }

  // A fresh token for the session: getToken's, else the session URL's.
  async #nextToken(): Promise<string | null> {
    const sessionUrl = this.getAttribute('session-url')
    const playbackId = this.getAttribute('playback-id')
    if (!playbackId) return null
    if (this.#getToken) return this.#getToken({ playbackId, native: playsNatively() })
    if (!sessionUrl) return null
    const playing = this.#session
    const s = await fetchSession(sessionUrl, playbackId, playsNatively())
    if (s === null) return null
    if (
      playing === this.#session &&
      sessionUrl === this.getAttribute('session-url') &&
      playbackId === this.getAttribute('playback-id') &&
      this.#playback?.playback_id === s.playback_id
    ) {
      this.#playback = {
        ...this.#playback,
        token: s.token,
        ...(s.expires_at ? { expires_at: s.expires_at } : {}),
        ...(s.tracks ? { tracks: s.tracks } : {}),
      }
    }
    return s.token
  }

  #play(media: MediaElement, baseUrl: string, playbackId: string, loading: string): void {
    this.#clearChapters()
    this.#loaded = loading
    const sessionUrl = this.getAttribute('session-url')
    const requestedId = this.getAttribute('playback-id')
    const requestedBase = this.getAttribute('base-url')
    const isCurrent = () =>
      session === this.#session &&
      sessionUrl === this.getAttribute('session-url') &&
      requestedId === this.getAttribute('playback-id') &&
      requestedBase === this.getAttribute('base-url')
    const session: TokenSession | null = this.#token
      ? new TokenSession(this.#token, () => (isCurrent() ? this.#nextToken() : Promise.resolve(null)), {
          onToken: (token) => {
            if (!isCurrent()) return
            const trackId =
              this.getAttribute('chapters-track') ?? (sessionUrl ? chaptersTrackOf(this.#playback) : '')
            this.#loadChapters(baseUrl, playbackId, trackId)
            this.#emit('clipmux-token', { token })
          },
          onDenied: () => {
            if (!isCurrent()) return
            media.pause()
            this.#emit('clipmux-denied', {})
          },
        })
      : null
    this.#session = session
    media.source = clipmuxSource(
      { baseUrl, playbackId, token: this.#token, hls: this.#hls },
      this.#session ?? undefined,
    )
  }

  #loadChapters(baseUrl: string, playbackId: string, trackId: string): void {
    if (!trackId) {
      this.#clearChapters()
      return
    }
    const previous = this.#chaptersRequest
    const token = this.token
    if (previous?.trackId === trackId && (previous.status !== 'failed' || previous.token === token)) {
      return
    }
    const request: ChaptersRequest = { trackId, token, status: 'loading', announced: false }
    this.#chaptersRequest = request
    void fetchTrack((t) => trackUrl(baseUrl, playbackId, trackId, t), this.#session, token || undefined)
      .then((vtt) => {
        if (this.#chaptersRequest !== request) return
        this.#chapters = parseChapters(vtt)
        request.status = 'ready'
        this.#showChapters()
      })
      .catch(() => {
        if (this.#chaptersRequest !== request) return
        request.status = 'failed'
        this.#chapters = []
        this.#writeChapterCues()
        this.#emit('clipmux-error', { status: 0, track: trackId })
      })
  }

  // hls.js clears text cues when loading a manifest. Wait for metadata so
  // the skin gets the current chapters even when the VTT arrives first.
  #showChapters(): void {
    const request = this.#chaptersRequest
    if (
      request?.status !== 'ready' ||
      !this.#media ||
      this.#media.readyState < HTMLMediaElement.HAVE_METADATA
    ) {
      return
    }
    this.#writeChapterCues()
    if (!request.announced) {
      request.announced = true
      this.#emit('clipmux-chapters', { chapters: this.#chapters })
    }
  }

  #clearChapters(): void {
    if (!this.#chaptersRequest) return
    this.#chaptersRequest = null
    this.#chapters = []
    this.#writeChapterCues()
  }

  // Video.js reads the first chapters track. TextTrackList has no removal
  // API for addTextTrack, so keep one track and notify the skin of cue edits.
  #writeChapterCues(): void {
    const video = this.#media?.target ?? this.#media
    if (!video) return
    let track = this.#chaptersTrack?.video === video ? this.#chaptersTrack.track : null
    if (!track) {
      if (this.#chapters.length === 0) return
      track = video.addTextTrack('chapters', 'Chapters')
      track.mode = 'hidden'
      this.#chaptersTrack = { video, track }
    }
    for (const cue of Array.from(track.cues ?? [])) track.removeCue(cue)
    const last = this.#chapters.at(-1)
    for (const chapter of this.#chapters) {
      const gap = video.duration - chapter.endTime
      const end =
        chapter === last && gap > 0 && gap <= CHAPTER_END_TOLERANCE ? video.duration : chapter.endTime
      track.addCue(new VTTCue(chapter.startTime, end, chapter.title))
    }
    video.textTracks.dispatchEvent(new Event('change'))
  }
}

if (!customElements.get('clipmux-player')) customElements.define('clipmux-player', ClipMuxPlayerElement)

declare global {
  interface HTMLElementTagNameMap {
    'clipmux-player': ClipMuxPlayerElement
  }
  interface HTMLElementEventMap extends ClipMuxPlayerEventMap {}
}
