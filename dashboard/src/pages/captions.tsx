import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import * as React from 'react'
import { type ChaptersStatus, del, describe, post, put, type SpeechStatus, type Step } from '@/api'
import { useConfirm } from '@/components/confirm'
import { PageBody, PageHeader, Section } from '@/components/page'
import { QueryState } from '@/components/query-state'
import { Steps } from '@/components/steps'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from '@/components/ui/collapsible'
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { Item, ItemActions, ItemContent, ItemDescription, ItemTitle } from '@/components/ui/item'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { Skeleton } from '@/components/ui/skeleton'
import { Spinner } from '@/components/ui/spinner'
import { notify } from '@/lib/notify'
import { q } from '@/queries'

// Captions & AI (P5 track B): where transcriptions run (D075), the speech
// API that can take them, and the LLM that makes chapters (D083). Each
// option says what, if anything, leaves your infrastructure.

const modes: { value: SpeechStatus['mode']; title: string; leaves: string; needsApi: boolean }[] = [
  {
    value: 'local',
    title: 'On this server and your PCs (whisper.cpp)',
    leaves: 'Nothing leaves your infrastructure.',
    needsApi: false,
  },
  {
    value: 'local_then_api',
    title: 'Locally, else the speech API',
    leaves: 'Audio goes to the speech API only when no machine with speech is free.',
    needsApi: true,
  },
  {
    value: 'api',
    title: 'Always the speech API',
    leaves: "Every video's audio goes to the speech API.",
    needsApi: true,
  },
]

export function Captions() {
  return (
    <PageBody className="max-w-[880px]">
      <PageHeader
        title="Captions & AI"
        description="Subtitles are made on your own machines by default. Each option says what, if anything, leaves your infrastructure."
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

function SpeechSection() {
  const qc = useQueryClient()
  const confirm = useConfirm()
  const speech = useQuery(q.speech())
  const connect = useConnect('/v1/settings/speech/connect', 'speech')
  const setMode = useMutation({
    mutationFn: (mode: string) => put<SpeechStatus>('/v1/settings/speech', { mode }),
    onSuccess: (next) => qc.setQueryData(['speech'], next),
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
          <div className="flex flex-col gap-4">
            <Field>
              <FieldLabel htmlFor="transcribe">Transcribe</FieldLabel>
              <NativeSelect
                id="transcribe"
                aria-label="Transcribe"
                value={st.mode}
                onChange={(e) => setMode.mutate(e.target.value)}
                className="w-full sm:w-96"
              >
                {modes.map((m) => (
                  <NativeSelectOption key={m.value} value={m.value} disabled={m.needsApi && !st.connection}>
                    {m.title}
                  </NativeSelectOption>
                ))}
              </NativeSelect>
              <FieldDescription className={st.mode === 'local' ? 'text-success' : 'text-warning'}>
                {modes.find((m) => m.value === st.mode)?.leaves}
              </FieldDescription>
            </Field>
            {st.connection && (
              <Item variant="outline" className="bg-card" data-connected="speech">
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
                        description: 'Transcriptions go back to this server and your PCs.',
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
              <CollapsibleTrigger render={<Button variant="outline" size="sm" />}>
                {st.connection ? 'Connect a different speech API' : 'Connect a speech API'}
              </CollapsibleTrigger>
              <CollapsibleContent className="pt-4">
                <form
                  aria-label="Connect a speech API"
                  className="rounded-lg border bg-card p-4"
                  onSubmit={(e) => {
                    e.preventDefault()
                    const f = new FormData(e.currentTarget)
                    connect.mutate({
                      base_url: f.get('base_url'),
                      model: f.get('model'),
                      api_key: f.get('api_key'),
                    })
                  }}
                >
                  <FieldGroup>
                    <p className="text-sm text-muted-foreground">
                      Any OpenAI-compatible transcription API. With it, the video's audio leaves your
                      infrastructure.
                    </p>
                    <Field>
                      <FieldLabel htmlFor="sp-url">Speech API URL</FieldLabel>
                      <Input
                        id="sp-url"
                        name="base_url"
                        placeholder="https://api.groq.com/openai/v1"
                        required
                      />
                    </Field>
                    <div className="grid gap-4 sm:grid-cols-2">
                      <Field>
                        <FieldLabel htmlFor="sp-model">Speech model</FieldLabel>
                        <Input id="sp-model" name="model" placeholder="whisper-large-v3-turbo" required />
                      </Field>
                      <Field>
                        <FieldLabel htmlFor="sp-key">Speech API key</FieldLabel>
                        <Input
                          id="sp-key"
                          name="api_key"
                          type="password"
                          placeholder="Empty for a server that needs none"
                          autoComplete="off"
                        />
                      </Field>
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

function ChaptersSection() {
  const qc = useQueryClient()
  const confirm = useConfirm()
  const chapters = useQuery(q.chapters())
  const connect = useConnect('/v1/settings/chapters/connect', 'chapters')
  const [provider, setProvider] = React.useState<'anthropic' | 'openai_compatible'>('anthropic')
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
                      base_url: f.get('base_url') ?? '',
                      model: f.get('model') ?? '',
                      api_key: f.get('api_key'),
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
                          required
                        />
                      </Field>
                    )}
                    <div className="grid gap-4 sm:grid-cols-2">
                      <Field>
                        <FieldLabel htmlFor="llm-model">LLM model</FieldLabel>
                        <Input
                          id="llm-model"
                          name="model"
                          placeholder={provider === 'anthropic' ? 'claude-sonnet-5-5' : 'gpt-5-mini'}
                          required={provider === 'openai_compatible'}
                        />
                      </Field>
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
                          required={provider === 'anthropic'}
                        />
                      </Field>
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
