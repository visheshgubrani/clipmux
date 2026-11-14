import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import * as React from 'react'
import {
  type ChaptersStatus,
  del,
  describe,
  post,
  put,
  type Routing,
  type SpeechStatus,
  type Step,
} from '@/api'
import { Choice } from '@/components/choice'
import { useConfirm } from '@/components/confirm'
import { ModelField } from '@/components/model-field'
import { PageBody, PageHeader, Section } from '@/components/page'
import { QueryState } from '@/components/query-state'
import { Steps } from '@/components/steps'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from '@/components/ui/collapsible'
import { Field, FieldError, FieldGroup, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { Item, ItemActions, ItemContent, ItemDescription, ItemTitle } from '@/components/ui/item'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { RadioGroup } from '@/components/ui/radio-group'
import { Skeleton } from '@/components/ui/skeleton'
import { Spinner } from '@/components/ui/spinner'
import { encodedOn } from '@/lib/encoding'
import { notify } from '@/lib/notify'
import { q } from '@/queries'

// Captions & AI (P5 track B): where transcriptions run (D075), the speech
// API that can take them, and the LLM that makes chapters (D083). Each
// option says what, if anything, leaves your infrastructure. Subtitles in
// the local mode are made where videos are encoded (D127), so on a cloud
// GPU they are made there, with the whisper model built into the image.

export function Captions() {
  return (
    <PageBody className="max-w-[880px]">
      <PageHeader
        title="Captions & AI"
        description="Subtitles are made where your videos are encoded, unless you choose a speech API. Each option says what, if anything, leaves your infrastructure."
      />
      <SpeechSection />
      <ChaptersSection />
    </PageBody>
  )
}

function useConnect(path: string, invalidate: string) {
  const qc = useQueryClient()
  const [steps, setSteps] = React.useState<Step[]>([])
  const m = useMutation({
    mutationFn: (body: object) => post<{ connected: boolean; steps: Step[] }>(path, body),
    onMutate: () => setSteps([]),
    onSuccess: (res) => {
      setSteps(res.steps)
      void qc.invalidateQueries({ queryKey: [invalidate] })
    },
  })
  return { ...m, steps }
}

/** An http or https URL, the only kind an API's models are asked of. */
function isHTTP(url: string): boolean {
  try {
    return /^https?:$/.test(new URL(url.trim()).protocol)
  } catch {
    return false
  }
}

/** Where local subtitles are made, in the words of the cards and toasts. */
function whereLocal(r: Routing | undefined): { title: string; body: string } {
  if (!r) return { title: 'where videos are encoded', body: 'Where videos are encoded.' }
  const on = encodedOn(r)
  if (on === 'this server') return { title: 'on this server', body: 'On this server, with whisper.cpp.' }
  const fallback =
    r.fallback_executor === 'builtin' ? ', or on this server when it can’t start a machine' : ''
  return {
    title: `on ${on}`,
    body: `On ${on}, with the whisper model built into ClipMux’s image${fallback}.`,
  }
}

function SpeechSection() {
  const qc = useQueryClient()
  const confirm = useConfirm()
  const speech = useQuery(q.speech())
  const routing = useQuery(q.routing())
  const connect = useConnect('/v1/settings/speech/connect', 'speech')
  const [url, setURL] = React.useState('')
  const [key, setKey] = React.useState('')
  const local = whereLocal(routing.data)
  const setMode = useMutation({
    mutationFn: (mode: SpeechStatus['mode']) => put<SpeechStatus>('/v1/settings/speech', { mode }),
    onSuccess: (next) => {
      qc.setQueryData(['speech'], next)
      notify.done(
        next.mode === 'api'
          ? 'Subtitles are made by the speech API'
          : next.mode === 'local_then_api'
            ? `Subtitles are made ${local.title}, else by the speech API`
            : `Subtitles are made ${local.title}`,
      )
    },
    onError: (err) => notify.failed("Couldn't change where subtitles are made", err),
  })
  const disconnect = useMutation({
    mutationFn: () => del<SpeechStatus>('/v1/settings/speech/connect'),
    onSuccess: (next) => {
      qc.setQueryData(['speech'], next)
      notify.done('Speech API disconnected')
    },
  })
  return (
    <Section title="Subtitles" aria-label="Speech">
      <QueryState query={speech} what="the speech settings" skeleton={<Skeleton className="h-40 w-full" />}>
        {(st) => (
          <RadioGroup
            value={st.mode === 'api' ? 'api' : 'local'}
            onValueChange={(v) => {
              const mode = v === 'api' ? 'api' : 'local'
              if (mode !== (st.mode === 'api' ? 'api' : 'local')) setMode.mutate(mode)
            }}
            aria-label="Where subtitles are made"
          >
            <Choice
              group="speech"
              value="local"
              title="Where videos are encoded"
              body={
                <>
                  {local.body} <span className="text-success">Audio stays on machines you run.</span>
                </>
              }
              disabled={setMode.isPending}
            >
              {st.connection && st.mode !== 'api' && (
                <Field orientation="horizontal">
                  <Checkbox
                    id="speech-fallback"
                    checked={st.mode === 'local_then_api'}
                    disabled={setMode.isPending}
                    onCheckedChange={(v) => setMode.mutate(v === true ? 'local_then_api' : 'local')}
                  />
                  <FieldLabel htmlFor="speech-fallback" className="font-normal">
                    If no machine can transcribe, use the speech API
                  </FieldLabel>
                </Field>
              )}
            </Choice>
            <Choice
              group="speech"
              value="api"
              title="A speech API"
              body={
                <>
                  Any OpenAI-compatible transcription API: Groq, OpenAI, Fireworks, DeepInfra, or your own
                  Speaches or LocalAI. <span className="text-warning">Every video’s audio goes to it.</span>
                </>
              }
              status={st.connection ? <Badge variant="success">Connected</Badge> : null}
              disabled={!st.connection || setMode.isPending}
            >
              {st.connection && (
                <Item variant="outline" className="bg-background" data-connected="speech">
                  <ItemContent>
                    <ItemTitle>{st.connection.model}</ItemTitle>
                    <ItemDescription className="truncate font-mono text-xs">
                      {st.connection.base_url}
                    </ItemDescription>
                  </ItemContent>
                  <ItemActions>
                    <Button
                      variant="ghost"
                      size="sm"
                      className="text-destructive"
                      onClick={async () => {
                        const ok = await confirm({
                          title: 'Disconnect the speech API?',
                          description: 'Transcriptions go back to where videos are encoded.',
                          action: 'Disconnect',
                        })
                        if (ok) disconnect.mutate()
                      }}
                    >
                      Disconnect
                    </Button>
                  </ItemActions>
                </Item>
              )}
              <Collapsible defaultOpen={false}>
                <CollapsibleTrigger render={<Button variant="outline" size="sm" className="w-fit" />}>
                  {st.connection ? 'Connect a different speech API' : 'Connect a speech API'}
                </CollapsibleTrigger>
                <CollapsibleContent className="pt-4">
                  <form
                    aria-label="Connect a speech API"
                    className="rounded-lg border bg-background p-4"
                    onSubmit={(e) => {
                      e.preventDefault()
                      const f = new FormData(e.currentTarget)
                      connect.mutate({ base_url: url.trim(), model: f.get('model'), api_key: key.trim() })
                    }}
                  >
                    <FieldGroup>
                      <p className="text-sm text-muted-foreground">
                        It must answer <span className="font-mono">verbose_json</span> with timestamps (OpenAI{' '}
                        <span className="font-mono">whisper-1</span>, Groq{' '}
                        <span className="font-mono">whisper-large-v3-turbo</span>). Connecting transcribes an
                        11 s sample on it.
                      </p>
                      <Field>
                        <FieldLabel htmlFor="sp-url">Speech API URL</FieldLabel>
                        <Input
                          id="sp-url"
                          name="base_url"
                          placeholder="https://api.groq.com/openai/v1"
                          value={url}
                          onChange={(e) => setURL(e.target.value)}
                          required
                        />
                      </Field>
                      <div className="grid gap-4 sm:grid-cols-2">
                        <Field>
                          <FieldLabel htmlFor="sp-key">Speech API key</FieldLabel>
                          <Input
                            id="sp-key"
                            name="api_key"
                            type="password"
                            placeholder="Empty for a server that needs none"
                            autoComplete="off"
                            value={key}
                            onChange={(e) => setKey(e.target.value)}
                          />
                        </Field>
                        <ModelField
                          id="sp-model"
                          label="Speech model"
                          placeholder="whisper-large-v3-turbo"
                          path="/v1/settings/speech/models"
                          body={isHTTP(url) ? { base_url: url.trim(), api_key: key.trim() } : null}
                          kind="speech"
                          hint="Its models are listed once the URL is in."
                          required
                        />
                      </div>
                      {connect.isError && <FieldError>{describe(connect.error)}</FieldError>}
                      <Steps steps={connect.steps} />
                      <div>
                        <Button type="submit" disabled={connect.isPending}>
                          {connect.isPending && <Spinner data-icon="inline-start" />}
                          {connect.isPending ? 'Connecting…' : 'Connect'}
                        </Button>
                      </div>
                    </FieldGroup>
                  </form>
                </CollapsibleContent>
              </Collapsible>
            </Choice>
          </RadioGroup>
        )}
      </QueryState>
    </Section>
  )
}

function ChaptersSection() {
  const qc = useQueryClient()
  const confirm = useConfirm()
  const chapters = useQuery(q.chapters())
  const connect = useConnect('/v1/settings/chapters/connect', 'chapters')
  const [provider, setProvider] = React.useState<'anthropic' | 'openai_compatible'>('anthropic')
  const [url, setURL] = React.useState('')
  const [key, setKey] = React.useState('')
  const listWith =
    provider === 'anthropic'
      ? key.trim()
        ? { provider, api_key: key.trim() }
        : null
      : isHTTP(url)
        ? { provider, base_url: url.trim(), api_key: key.trim() }
        : null
  const disconnect = useMutation({
    mutationFn: () => del<ChaptersStatus>('/v1/settings/chapters/connect'),
    onSuccess: (next) => {
      qc.setQueryData(['chapters'], next)
      notify.done('LLM disconnected')
    },
  })
  return (
    <Section
      title="Chapters"
      description="An LLM reads a video's transcript and titles its chapters. Only the transcript's text is sent, never the video or its audio."
      actions={
        <Badge variant={chapters.data?.connection ? 'success' : 'secondary'}>
          {chapters.data?.connection ? 'On' : 'Off'}
        </Badge>
      }
      aria-label="Chapters"
    >
      <QueryState
        query={chapters}
        what="the chapters settings"
        skeleton={<Skeleton className="h-32 w-full" />}
      >
        {(st) => (
          <div className="flex flex-col gap-4">
            {st.connection ? (
              <Item variant="outline" className="bg-card" data-connected="chapters">
                <ItemContent>
                  <ItemTitle>
                    {st.connection.provider === 'anthropic' ? 'Claude' : 'OpenAI-compatible'}:{' '}
                    {st.connection.model}
                  </ItemTitle>
                  {st.connection.base_url && (
                    <ItemDescription className="truncate font-mono text-xs">
                      {st.connection.base_url}
                    </ItemDescription>
                  )}
                </ItemContent>
                <ItemActions>
                  <Button
                    variant="ghost"
                    size="sm"
                    className="text-destructive"
                    onClick={async () => {
                      const ok = await confirm({
                        title: 'Disconnect the LLM?',
                        description:
                          'Chapters already made keep playing. New uploads asking for chapters get none.',
                        action: 'Disconnect',
                      })
                      if (ok) disconnect.mutate()
                    }}
                  >
                    Disconnect
                  </Button>
                </ItemActions>
              </Item>
            ) : (
              <p className="text-sm text-muted-foreground">
                No LLM connected, so uploads asking for chapters get none (llm_not_connected).
              </p>
            )}
            <Collapsible defaultOpen={!st.connection}>
              <CollapsibleTrigger render={<Button variant="outline" size="sm" />}>
                Connect an LLM
              </CollapsibleTrigger>
              <CollapsibleContent className="pt-4">
                <form
                  aria-label="Connect an LLM"
                  className="rounded-lg border bg-card p-4"
                  onSubmit={(e) => {
                    e.preventDefault()
                    const f = new FormData(e.currentTarget)
                    connect.mutate({
                      provider,
                      base_url: provider === 'openai_compatible' ? url.trim() : '',
                      model: f.get('model') ?? '',
                      api_key: key.trim(),
                    })
                  }}
                >
                  <FieldGroup>
                    <Field>
                      <FieldLabel htmlFor="llm-provider">LLM provider</FieldLabel>
                      <NativeSelect
                        id="llm-provider"
                        aria-label="LLM provider"
                        value={provider}
                        onChange={(e) => setProvider(e.target.value as typeof provider)}
                      >
                        <NativeSelectOption value="anthropic">Claude (Anthropic)</NativeSelectOption>
                        <NativeSelectOption value="openai_compatible">
                          OpenAI-compatible (OpenAI, Ollama, vLLM, Groq…)
                        </NativeSelectOption>
                      </NativeSelect>
                    </Field>
                    {provider === 'openai_compatible' && (
                      <Field>
                        <FieldLabel htmlFor="llm-url">LLM API URL</FieldLabel>
                        <Input
                          id="llm-url"
                          name="base_url"
                          placeholder="https://api.openai.com/v1 or http://ollama.lan:11434/v1"
                          value={url}
                          onChange={(e) => setURL(e.target.value)}
                          required
                        />
                      </Field>
                    )}
                    <div className="grid gap-4 sm:grid-cols-2">
                      <Field>
                        <FieldLabel htmlFor="llm-key">LLM API key</FieldLabel>
                        <Input
                          id="llm-key"
                          name="api_key"
                          type="password"
                          placeholder={
                            provider === 'anthropic'
                              ? 'Anthropic API key'
                              : 'Empty for a server that needs none'
                          }
                          autoComplete="off"
                          value={key}
                          onChange={(e) => setKey(e.target.value)}
                          required={provider === 'anthropic'}
                        />
                      </Field>
                      <ModelField
                        key={provider}
                        id="llm-model"
                        label="LLM model"
                        placeholder={provider === 'anthropic' ? 'claude-opus-5-5' : 'gpt-5-mini'}
                        path="/v1/settings/chapters/models"
                        body={listWith}
                        kind={provider === 'anthropic' ? 'claude' : 'llm'}
                        hint={
                          provider === 'anthropic'
                            ? 'Claude’s models are listed once the key is in.'
                            : 'Its models are listed once the URL is in.'
                        }
                        required={provider === 'openai_compatible'}
                      />
                    </div>
                    {connect.isError && <FieldError>{describe(connect.error)}</FieldError>}
                    <Steps steps={connect.steps} />
                    <div>
                      <Button type="submit" disabled={connect.isPending}>
                        {connect.isPending && <Spinner data-icon="inline-start" />}
                        {connect.isPending ? 'Connecting…' : 'Connect'}
                      </Button>
                    </div>
                  </FieldGroup>
                </form>
              </CollapsibleContent>
            </Collapsible>
          </div>
        )}
      </QueryState>
    </Section>
  )
}
