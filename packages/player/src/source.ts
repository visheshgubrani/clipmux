import Hls from 'hls.js'
import { createRefreshLoader, type GetToken, type LoaderClass, type TokenSession } from './refresh.ts'

export interface SourceOptions {
  /** base_url from the asset, e.g. https://media.example.net. */
  baseUrl: string
  playbackId: string
  /** A playback token (aud v) for a signed ID; omit for a public one. */
  token?: string | undefined
  /** Extra hls.js options, applied once. */
  hls?: Record<string, unknown> | undefined
}

/** The structured source for Video.js's <hlsjs-video>. */
export interface ClipMuxSource {
  src: string
  type: string
  preferPlayback: 'mse' | 'native'
  engine: { hlsJs: Record<string, unknown> }
}

/** The master playlist URL of a playback ID. */
export function playbackUrl(o: Pick<SourceOptions, 'baseUrl' | 'playbackId' | 'token'>): string {
  const base = o.baseUrl.replace(/\/+$/, '')
  const pid = encodeURIComponent(o.playbackId)
  return o.token ? `${base}/v/${o.token}/${pid}/master.m3u8` : `${base}/p/${pid}/master.m3u8`
}

/**
 * Builds the source for <hlsjs-video>. With a session, hls.js (on MSE, and
 * on iOS 17.1+ ManagedMediaSource) loads through the refresh loader. A
 * browser playing HLS natively can't be refreshed: it relies on the token's
 * lifetime (2 × duration or 4 h).
 */
export function clipmuxSource(o: SourceOptions, session?: TokenSession): ClipMuxSource {
  const hlsJs: Record<string, unknown> = { ...o.hls }
  if (session) {
    const Base = Hls.DefaultConfig.loader as unknown as LoaderClass<{ url: string }>
    hlsJs.loader = createRefreshLoader(Base, session, o.baseUrl, o.playbackId)
  }
  return {
    src: playbackUrl({ ...o, token: session?.current ?? o.token }),
    type: 'application/vnd.apple.mpegurl',
    preferPlayback: 'mse',
    engine: { hlsJs },
  }
}

export type { GetToken }
