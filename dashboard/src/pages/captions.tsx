import { type FormEvent, useCallback, useEffect, useState } from 'react'
import { ApiError, api, post, type Step } from '../api.ts'
import { Button } from '../components/ui/button.tsx'
import { Card, CardTitle, Muted } from '../components/ui/card.tsx'
import { Input } from '../components/ui/input.tsx'
import { Steps } from './routing.tsx'

// Captions (P5 track B): where transcriptions run (D075), the speech API
// that can take them, and the LLM that makes chapters (D083). Each Connect
// runs its steps and saves only if they all pass.

interface SpeechStatus {
  mode: 'local' | 'api' | 'local_then_api'
  connection: { base_url: string; model: string; has_key: boolean; verified_at: string } | null
}

interface ChaptersStatus {
  connection: {
    provider: 'openai_compatible' | 'anthropic'
    base_url?: string
    model: string
    response_format?: string
    has_key: boolean
    verified_at: string
  } | null
}

const selectClass = 'h-9 w-full rounded-md border border-zinc-300 bg-white px-2 text-sm'

const modes: Record<SpeechStatus['mode'], string> = {
  local: 'On this server and your PCs (whisper.cpp)',
  local_then_api: 'Locally, else the speech API',
  api: 'Always the speech API',
}

function useConnect(path: string, onDone: () => void) {
  const [steps, setSteps] = useState<Step[]>([])
  const [busyState, setBusy] = useState(false)
  const [error, setError] = useState('')
  const connect = async (e: FormEvent<HTMLFormElement>, body: Record<string, unknown>) => {
    e.preventDefault()
    const el = e.currentTarget
    setBusy(true)
    setError('')
    setSteps([])
    try {
      const res = await post<{ connected: boolean; steps: Step[] }>(path, body)
      setSteps(res.steps)
      if (res.connected) el.reset()
      onDone()
    } catch (err) {
      setError(err instanceof ApiError ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }
  return { steps, busy: busyState, error, setError, connect }
}

function SpeechCard() {
  const [st, setSt] = useState<SpeechStatus | null>(null)
  const load = useCallback(async () => setSt(await api<SpeechStatus>('/v1/settings/speech')), [])
  useEffect(() => {
    void load()
  }, [load])
  const c = useConnect('/v1/settings/speech/connect', load)
  if (!st) return null
  const setMode = async (mode: string) => {
    c.setError('')
    try {
      setSt(await api<SpeechStatus>('/v1/settings/speech', { method: 'PUT', body: JSON.stringify({ mode }) }))
    } catch (err) {
      c.setError(err instanceof ApiError ? err.message : String(err))
    }
  }
  const disconnect = async () => {
    if (!confirm('Disconnect the speech API? Transcriptions go back to this server and your PCs.')) return
    setSt(await api<SpeechStatus>('/v1/settings/speech/connect', { method: 'DELETE' }))
  }
  return (
    <Card className="grid gap-4" aria-label="Speech">
      <CardTitle>Subtitles</CardTitle>
      <Muted>
        Subtitles are transcribed with whisper.cpp on workers that can (see Workers), or by a speech API you
        connect. <strong>With a speech API, the video's audio leaves your infrastructure.</strong>
      </Muted>
      <label className="grid gap-1.5 text-sm font-medium">
        Transcribe
        <select
          className={selectClass}
          value={st.mode}
          onChange={(e) => void setMode(e.target.value)}
          aria-label="Transcribe"
        >
          {Object.entries(modes).map(([v, label]) => (
            <option key={v} value={v} disabled={v !== 'local' && !st.connection}>
              {label}
            </option>
          ))}
        </select>
      </label>
      {st.connection ? (
        <div className="flex items-center justify-between rounded-md border border-zinc-200 p-3 text-sm">
          <span>
            {st.connection.model} at <span className="font-mono text-xs">{st.connection.base_url}</span>
          </span>
          <Button variant="outline" size="sm" onClick={() => void disconnect()}>
            Disconnect
          </Button>
        </div>
      ) : null}
      <details className="rounded-md border border-zinc-200 p-3">
        <summary className="cursor-pointer text-sm font-medium">Connect a speech API</summary>
        <form
          className="mt-3 grid gap-3"
          aria-label="Connect a speech API"
          onSubmit={(e) => {
            const f = new FormData(e.currentTarget)
            void c.connect(e, {
              base_url: f.get('base_url'),
              model: f.get('model'),
              api_key: f.get('api_key'),
            })
          }}
        >
          <Input
            name="base_url"
            placeholder="https://api.groq.com/openai/v1"
            aria-label="Speech API URL"
            required
          />
          <Input name="model" placeholder="whisper-large-v3-turbo" aria-label="Speech model" required />
          <Input
            name="api_key"
            type="password"
            placeholder="API key (empty for a server that needs none)"
            aria-label="Speech API key"
            autoComplete="off"
          />
          <div>
            <Button type="submit" disabled={c.busy}>
              {c.busy ? 'Connecting…' : 'Connect'}
            </Button>
          </div>
        </form>
        <div className="mt-2">
          <Steps steps={c.steps} />
        </div>
      </details>
      {c.error && <p className="text-sm text-red-700">{c.error}</p>}
    </Card>
  )
}

function ChaptersCard() {
  const [st, setSt] = useState<ChaptersStatus | null>(null)
  const [provider, setProvider] = useState<'openai_compatible' | 'anthropic'>('anthropic')
  const load = useCallback(async () => setSt(await api<ChaptersStatus>('/v1/settings/chapters')), [])
  useEffect(() => {
    void load()
  }, [load])
  const c = useConnect('/v1/settings/chapters/connect', load)
  if (!st) return null
  const disconnect = async () => {
    if (!confirm('Disconnect the LLM? Chapters already made keep playing.')) return
    setSt(await api<ChaptersStatus>('/v1/settings/chapters/connect', { method: 'DELETE' }))
  }
  return (
    <Card className="grid gap-4" aria-label="Chapters">
      <CardTitle>Chapters</CardTitle>
      <Muted>
        Chapters are made from a video's transcript by an LLM you connect: Claude, or any OpenAI-compatible
        API such as OpenAI, Ollama or vLLM. <strong>The transcript's text is sent to that provider.</strong>
      </Muted>
      {st.connection ? (
        <div
          className="flex items-center justify-between rounded-md border border-zinc-200 p-3 text-sm"
          data-connected="chapters"
        >
          <span>
            {st.connection.provider === 'anthropic' ? 'Claude' : 'OpenAI-compatible'}: {st.connection.model}
            {st.connection.base_url && (
              <span className="font-mono text-xs"> at {st.connection.base_url}</span>
            )}
          </span>
          <Button variant="outline" size="sm" onClick={() => void disconnect()}>
            Disconnect
          </Button>
        </div>
      ) : (
        <Muted>No LLM connected: uploads asking for chapters get them errored with llm_not_connected.</Muted>
      )}
      <details className="rounded-md border border-zinc-200 p-3" open={!st.connection}>
        <summary className="cursor-pointer text-sm font-medium">Connect an LLM</summary>
        <form
          className="mt-3 grid gap-3"
          aria-label="Connect an LLM"
          onSubmit={(e) => {
            const f = new FormData(e.currentTarget)
            void c.connect(e, {
              provider,
              base_url: f.get('base_url') ?? '',
              model: f.get('model') ?? '',
              api_key: f.get('api_key'),
            })
          }}
        >
          <select
            className={selectClass}
            value={provider}
            onChange={(e) => setProvider(e.target.value as typeof provider)}
            aria-label="LLM provider"
          >
            <option value="anthropic">Claude (Anthropic)</option>
            <option value="openai_compatible">OpenAI-compatible (OpenAI, Ollama, vLLM, Groq…)</option>
          </select>
          {provider === 'openai_compatible' && (
            <Input
              name="base_url"
              placeholder="https://api.openai.com/v1 or http://ollama.lan:11434/v1"
              aria-label="LLM API URL"
              required
            />
          )}
          <Input
            name="model"
            placeholder={provider === 'anthropic' ? 'claude-opus-5-5' : 'gpt-5-mini'}
            aria-label="LLM model"
            required={provider === 'openai_compatible'}
          />
          <Input
            name="api_key"
            type="password"
            placeholder={
              provider === 'anthropic' ? 'Anthropic API key' : 'API key (empty for a server that needs none)'
            }
            aria-label="LLM API key"
            autoComplete="off"
            required={provider === 'anthropic'}
          />
          <div>
            <Button type="submit" disabled={c.busy}>
              {c.busy ? 'Connecting…' : 'Connect'}
            </Button>
          </div>
        </form>
        <div className="mt-2">
          <Steps steps={c.steps} />
        </div>
      </details>
      {c.error && <p className="text-sm text-red-700">{c.error}</p>}
    </Card>
  )
}

export function Captions() {
  return (
    <>
      <SpeechCard />
      <ChaptersCard />
    </>
  )
}
