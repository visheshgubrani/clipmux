import { useMutation } from '@tanstack/react-query'
import * as React from 'react'
import { ApiError, describe, type ModelList, post } from '@/api'
import {
  Autocomplete,
  AutocompleteCollection,
  AutocompleteContent,
  AutocompleteEmpty,
  AutocompleteGroup,
  AutocompleteGroupLabel,
  AutocompleteInput,
  AutocompleteItem,
  AutocompleteList,
} from '@/components/ui/autocomplete'
import { Field, FieldDescription, FieldLabel } from '@/components/ui/field'
import { Spinner } from '@/components/ui/spinner'

// A Connect form's model field (D127): once a URL (and a key, where one is
// needed) is typed, the API's own models are listed under it, the likely
// ones first, and the usual one filled in while the owner hasn't typed one.
// It stays a free-text input: an API that lists nothing still connects.

type Model = ModelList['data'][number]
/** speech: a transcription API; llm: an OpenAI-compatible chat API; claude: Anthropic's. */
export type ModelKind = 'speech' | 'llm' | 'claude'
type Group = { value: string; items: Model[] }

const speechLike = /whisper|transcri|speech|stt|asr/i
const notChat = /embed|whisper|tts|transcri|dall-e|image|moderation|rerank|realtime/i

/** The models in two groups, suggested first; nothing is hidden. */
export function groupModels(kind: ModelKind, models: Model[]): Group[] {
  if (kind === 'claude') return models.length ? [{ value: 'Models', items: models }] : []
  const likely = (m: Model) => (kind === 'speech' ? speechLike.test(m.id) : !notChat.test(m.id))
  const suggested = models.filter(likely)
  const other = models.filter((m) => !likely(m))
  if (!suggested.length || !other.length) return models.length ? [{ value: 'Models', items: models }] : []
  return [
    { value: 'Suggested', items: suggested },
    { value: 'Other models', items: other },
  ]
}

/** The model to fill in for the owner, if there's an obvious one. */
function usual(kind: ModelKind, models: Model[]): string | undefined {
  const ids = models.map((m) => m.id)
  if (kind === 'speech') {
    return (
      ['whisper-large-v3-turbo', 'whisper-1'].find((id) => ids.includes(id)) ??
      ids.find((id) => speechLike.test(id))
    )
  }
  if (kind === 'claude') return ids.includes('claude-opus-5-5') ? 'claude-opus-5-5' : ids[0]
  const chat = ids.filter((id) => !notChat.test(id))
  return chat.length === 1 ? chat[0] : undefined
}

function hostOf(url: string | undefined): string {
  try {
    return url ? new URL(url).host : ''
  } catch {
    return ''
  }
}

export function ModelField({
  id,
  label,
  placeholder,
  path,
  body,
  kind,
  required,
  hint,
}: {
  id: string
  label: string
  placeholder: string
  /** The list route, e.g. /v1/settings/speech/models. */
  path: string
  /** What to send it, or null while there's nothing to ask with yet. */
  body: { base_url?: string; api_key?: string; provider?: string } | null
  kind: ModelKind
  required?: boolean
  /** Shown before anything has been asked. */
  hint: string
}) {
  const [value, setValue] = React.useState('')
  // Whether the owner typed or picked the model: then nothing is filled in.
  const chosen = React.useRef(false)
  const list = useMutation({ mutationFn: (b: object) => post<ModelList>(path, b) })
  const { mutate, reset } = list
  // Ask a moment after the URL or key stops changing.
  const ask = body ? JSON.stringify(body) : ''
  React.useEffect(() => {
    if (!ask) {
      reset()
      return
    }
    const t = setTimeout(() => mutate(JSON.parse(ask) as object), 400)
    return () => clearTimeout(t)
  }, [ask, mutate, reset])
  const models = list.data?.data
  React.useEffect(() => {
    if (!chosen.current) setValue(models ? (usual(kind, models) ?? '') : '')
  }, [models, kind])
  const groups = React.useMemo(() => groupModels(kind, models ?? []), [kind, models])
  // Filter by what's typed, but show the whole list while the field holds
  // a listed model (filled in or picked), so another is one click away.
  const shown = React.useMemo(() => {
    const v = value.trim().toLowerCase()
    if (!v || models?.some((m) => m.id === value)) return groups
    return groups
      .map((g) => ({
        ...g,
        items: g.items.filter((m) => m.id.toLowerCase().includes(v) || m.name?.toLowerCase().includes(v)),
      }))
      .filter((g) => g.items.length)
  }, [groups, models, value])
  const host = kind === 'claude' && !body?.base_url ? 'Anthropic' : hostOf(body?.base_url)
  const n = models?.length ?? 0
  return (
    <Field>
      <FieldLabel htmlFor={id}>{label}</FieldLabel>
      <Autocomplete
        items={groups}
        filteredItems={shown}
        value={value}
        onValueChange={(v) => {
          setValue(v)
          chosen.current = v !== ''
        }}
        itemToStringValue={(m: Model) => m.id}
        openOnInputClick
      >
        <AutocompleteInput
          id={id}
          name="model"
          placeholder={placeholder}
          required={required}
          autoComplete="off"
          spellCheck={false}
        />
        {groups.length > 0 && (
          <AutocompleteContent>
            <AutocompleteEmpty>No listed model matches; it can still be used.</AutocompleteEmpty>
            <AutocompleteList>
              {(group: Group) => (
                <AutocompleteGroup key={group.value} items={group.items}>
                  {shown.length > 1 && <AutocompleteGroupLabel>{group.value}</AutocompleteGroupLabel>}
                  <AutocompleteCollection>
                    {(m: Model) => (
                      <AutocompleteItem key={m.id} value={m}>
                        {m.name ? (
                          <span className="flex min-w-0 flex-col">
                            <span>{m.name}</span>
                            <span className="truncate font-mono text-xs text-muted-foreground">{m.id}</span>
                          </span>
                        ) : (
                          <span className="truncate font-mono">{m.id}</span>
                        )}
                      </AutocompleteItem>
                    )}
                  </AutocompleteCollection>
                </AutocompleteGroup>
              )}
            </AutocompleteList>
          </AutocompleteContent>
        )}
      </Autocomplete>
      <FieldDescription aria-live="polite" className={list.isError ? 'text-destructive' : undefined}>
        {list.isPending ? (
          <span className="inline-flex items-center gap-1.5">
            <Spinner /> Reading models…
          </span>
        ) : list.isError ? (
          list.error instanceof ApiError && list.error.code === 'key_rejected' ? (
            'The API refused the key.'
          ) : (
            describe(list.error)
          )
        ) : list.data && !list.data.listed ? (
          "This API doesn't list its models; type the name."
        ) : list.data ? (
          `${n} ${n === 1 ? 'model' : 'models'} from ${host}`
        ) : (
          hint
        )}
      </FieldDescription>
    </Field>
  )
}
