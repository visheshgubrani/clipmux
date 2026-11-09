export interface FixtureConfig {
  source_url?: string
  live?: boolean
  base_url: string
  playback_id: string
  api: string
  subtitles_track: string
  chapters_track: string
  cue_text: string
  cue_start: number
}

export async function config(): Promise<FixtureConfig> {
  return (await fetch('/config')).json()
}

export async function token(ttl = 3600, aud = 'v'): Promise<string> {
  return (await fetch(`/token?ttl=${ttl}&aud=${aud}`)).text()
}

/** What the specs read back from the page. */
export function expose(state: object): void {
  ;(globalThis as unknown as { __state: object }).__state = state
}
