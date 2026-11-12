import { ClipMuxUpload, type CreatedUpload, rememberUpload } from '@clipmux/uploader'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { CheckIcon, PauseIcon, PlayIcon, UploadIcon, XIcon } from 'lucide-react'
import * as React from 'react'
import { Link } from 'react-router'
import { post } from '@/api'
import { Button } from '@/components/ui/button'
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from '@/components/ui/collapsible'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import {
  Field,
  FieldContent,
  FieldDescription,
  FieldGroup,
  FieldLabel,
  FieldLegend,
  FieldSet,
} from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { Progress } from '@/components/ui/progress'
import { Switch } from '@/components/ui/switch'
import { ToggleGroup, ToggleGroupItem } from '@/components/ui/toggle-group'
import { videoPath } from '@/lib/format'
import { notify } from '@/lib/notify'
import { cn } from '@/lib/utils'
import { q } from '@/queries'

// Upload (docs/design.md): several files at once, each its own ordinary
// POST /v1/uploads as the chosen app (the Dashboard app by default), sent in
// parts straight to storage and resumed after a reload.

type State = 'ready' | 'uploading' | 'paused' | 'done' | 'failed' | 'cancelled'

interface Item {
  key: string
  file: File
  title: string
  state: State
  progress: number
  error?: string
  assetId?: string
  appId?: string
  upload?: ClipMuxUpload
}

interface Options {
  privacy: 'signed' | 'public'
  subtitles: boolean
  chapters: boolean
  app: string
  passthrough: string
}

const remembered = rememberUpload(
  (() => {
    try {
      return window.localStorage
    } catch {
      const m = new Map<string, string>()
      return {
        getItem: (k) => m.get(k) ?? null,
        setItem: (k, v) => void m.set(k, v),
        removeItem: (k) => void m.delete(k),
      }
    }
  })() as Storage,
)

const titleFrom = (name: string) =>
  name
    .replace(/\.[^.]+$/, '')
    .replace(/[_-]+/g, ' ')
    .trim()
    .slice(0, 200)
const fileKey = (f: File) => `${f.name}:${f.size}:${f.lastModified}`

interface UploadAPI {
  open: (files?: File[]) => void
}
const UploadContext = React.createContext<UploadAPI | null>(null)

export function useUpload(): UploadAPI {
  const c = React.useContext(UploadContext)
  if (!c) throw new Error('useUpload outside UploadProvider')
  return c
}

export function UploadProvider({ children }: { children: React.ReactNode }) {
  const [open, setOpen] = React.useState(false)
  const [items, setItems] = React.useState<Item[]>([])
  const [opts, setOpts] = React.useState<Options>({
    privacy: 'signed',
    subtitles: false,
    chapters: false,
    app: '',
    passthrough: '',
  })
  const qc = useQueryClient()
  const apps = useQuery({ ...q.apps(), enabled: open })
  const chapters = useQuery({ ...q.chapters(), enabled: open })
  const llm = !!chapters.data?.connection

  const add = React.useCallback((files: File[]) => {
    const videos = files.filter(
      (f) => f.type.startsWith('video/') || /\.(mp4|mov|mkv|webm|m4v|avi|mxf|ts)$/i.test(f.name),
    )
    setItems((cur) => {
      const have = new Set(cur.map((i) => i.key))
      return [
        ...cur,
        ...videos
          .filter((f) => !have.has(fileKey(f)))
          .map((f) => ({
            key: fileKey(f),
            file: f,
            title: titleFrom(f.name),
            state: 'ready' as const,
            progress: 0,
          })),
      ]
    })
  }, [])

  const api = React.useMemo<UploadAPI>(
    () => ({
      open: (files) => {
        if (files?.length) add(files)
        setOpen(true)
      },
    }),
    [add],
  )

  const update = (key: string, patch: Partial<Item>) =>
    setItems((cur) => cur.map((i) => (i.key === key ? { ...i, ...patch } : i)))

  const run = async (item: Item) => {
    update(item.key, { state: 'uploading', progress: 0 })
    try {
      const app = opts.app || undefined
      let created: CreatedUpload | null = remembered.load(item.file)
      let assetId: string | undefined
      if (!created) {
        const res = await post<CreatedUpload & { asset_id: string }>(
          '/v1/uploads',
          {
            size: item.file.size,
            playback_policy: opts.privacy,
            ...(item.title ? { title: item.title } : {}),
            ...(opts.passthrough && items.length === 1 ? { passthrough: opts.passthrough } : {}),
            ...(opts.subtitles || opts.chapters
              ? { generate: { subtitles: opts.subtitles || opts.chapters, chapters: opts.chapters && llm } }
              : {}),
          },
          app,
        )
        created = res
        assetId = res.asset_id
        remembered.save(item.file, res)
      }
      const upload = new ClipMuxUpload(item.file, {
        endpoint: location.origin,
        upload: created,
        onProgress: (p) => update(item.key, { progress: p.bytesUploaded / p.bytesTotal }),
      })
      update(item.key, { upload, ...(app ? { appId: app } : {}) })
      const asset = await upload.start()
      remembered.forget(item.file)
      update(item.key, { state: 'done', progress: 1, assetId: asset.id ?? assetId })
      void qc.invalidateQueries({ queryKey: ['library'] })
      void qc.invalidateQueries({ queryKey: ['library-counts'] })
    } catch (err) {
      const code = (err as { code?: string }).code
      if (code === 'cancelled') {
        remembered.forget(item.file)
        update(item.key, { state: 'cancelled' })
        return
      }
      // A remembered upload that ClipMux no longer knows starts again next time.
      if (code === 'not_found' || code === 'upload_expired') remembered.forget(item.file)
      update(item.key, { state: 'failed', error: (err as Error).message })
      notify.failed(`Couldn't upload ${item.file.name}`, err)
    }
  }

  const start = async () => {
    const queue = items.filter((i) => i.state === 'ready' || i.state === 'failed')
    // Two at a time: each already sends four parts in parallel.
    const workers = Array.from({ length: Math.min(2, queue.length) }, async () => {
      for (let next = queue.shift(); next; next = queue.shift()) await run(next)
    })
    await Promise.all(workers)
  }

  const active = items.some((i) => i.state === 'uploading' || i.state === 'paused')
  const pending = items.filter((i) => i.state === 'ready' || i.state === 'failed').length
  const doneItems = items.filter((i) => i.state === 'done')

  const close = (next: boolean) => {
    setOpen(next)
    // Finished and cancelled rows leave once the dialog closes; running ones keep going.
    if (!next)
      setItems((cur) =>
        cur.filter(
          (i) =>
            i.state === 'uploading' || i.state === 'paused' || i.state === 'ready' || i.state === 'failed',
        ),
      )
  }

  return (
    <UploadContext.Provider value={api}>
      {children}
      <Dialog open={open} onOpenChange={close}>
        <DialogContent className="sm:max-w-xl">
          <DialogHeader>
            <DialogTitle>Upload videos</DialogTitle>
            <DialogDescription>
              Uploads keep going if you close this, and resume if you reload.
            </DialogDescription>
          </DialogHeader>
          <div className="flex max-h-[60vh] flex-col gap-5 overflow-y-auto">
            <Dropzone onFiles={add} compact={items.length > 0} />
            {items.length > 0 && (
              <ul className="flex flex-col gap-2" aria-label="Files">
                {items.map((i) => (
                  <FileRow
                    key={i.key}
                    item={i}
                    onTitle={(title) => update(i.key, { title })}
                    onRemove={() => setItems((cur) => cur.filter((x) => x.key !== i.key))}
                    onPause={() => {
                      i.upload?.pause()
                      update(i.key, { state: 'paused' })
                    }}
                    onResume={() => {
                      i.upload?.resume()
                      update(i.key, { state: 'uploading' })
                    }}
                    onCancel={() => void i.upload?.cancel()}
                    onOpen={() => close(false)}
                  />
                ))}
              </ul>
            )}
            <FieldSet disabled={active}>
              <FieldLegend variant="label">For these videos</FieldLegend>
              <FieldGroup className="gap-4">
                <Field>
                  <FieldLabel id="privacy-label">Who can watch</FieldLabel>
                  <ToggleGroup
                    aria-labelledby="privacy-label"
                    variant="outline"
                    value={[opts.privacy]}
                    onValueChange={(v: string[]) =>
                      v[0] && setOpts({ ...opts, privacy: v[0] as Options['privacy'] })
                    }
                    className="w-full"
                  >
                    <ToggleGroupItem value="signed" className="flex-1">
                      Private
                    </ToggleGroupItem>
                    <ToggleGroupItem value="public" className="flex-1">
                      Public
                    </ToggleGroupItem>
                  </ToggleGroup>
                  <FieldDescription>
                    {opts.privacy === 'signed'
                      ? 'Plays only with a token your app signs.'
                      : 'Anyone with the link or the embed can watch.'}
                  </FieldDescription>
                </Field>
                <Field orientation="horizontal">
                  <FieldContent>
                    <FieldLabel htmlFor="up-subs">Generate subtitles</FieldLabel>
                    <FieldDescription>Made from the audio once the video is ready.</FieldDescription>
                  </FieldContent>
                  <Switch
                    id="up-subs"
                    aria-label="Generate subtitles"
                    checked={opts.subtitles || opts.chapters}
                    onCheckedChange={(v) =>
                      setOpts({ ...opts, subtitles: v, chapters: v ? opts.chapters : false })
                    }
                  />
                </Field>
                <Field orientation="horizontal" data-disabled={!llm || undefined}>
                  <FieldContent>
                    <FieldLabel htmlFor="up-chapters">Generate chapters</FieldLabel>
                    <FieldDescription>
                      {llm ? (
                        'An LLM titles chapters from the subtitles.'
                      ) : (
                        <>
                          Needs an LLM.{' '}
                          <Link
                            to="/captions"
                            onClick={() => close(false)}
                            className="text-link underline-offset-4 hover:underline"
                          >
                            Connect one
                          </Link>
                        </>
                      )}
                    </FieldDescription>
                  </FieldContent>
                  <Switch
                    id="up-chapters"
                    aria-label="Generate chapters"
                    disabled={!llm}
                    checked={opts.chapters}
                    onCheckedChange={(v) => setOpts({ ...opts, chapters: v })}
                  />
                </Field>
                <Collapsible>
                  <CollapsibleTrigger render={<Button variant="link" size="sm" className="px-0" />}>
                    Advanced
                  </CollapsibleTrigger>
                  <CollapsibleContent>
                    <FieldGroup className="grid gap-4 pt-3 sm:grid-cols-2">
                      <Field>
                        <FieldLabel htmlFor="up-app">App</FieldLabel>
                        <NativeSelect
                          id="up-app"
                          value={opts.app}
                          onChange={(e) => setOpts({ ...opts, app: e.target.value })}
                        >
                          <NativeSelectOption value="">Dashboard</NativeSelectOption>
                          {(apps.data?.data ?? [])
                            .filter((a) => !a.builtin && !a.disabled)
                            .map((a) => (
                              <NativeSelectOption key={a.id} value={a.id}>
                                {a.name}
                              </NativeSelectOption>
                            ))}
                        </NativeSelect>
                      </Field>
                      <Field data-disabled={items.length > 1 || undefined}>
                        <FieldLabel htmlFor="up-ref">Reference</FieldLabel>
                        <Input
                          id="up-ref"
                          placeholder="Your own ID, optional"
                          maxLength={255}
                          disabled={items.length > 1}
                          value={opts.passthrough}
                          onChange={(e) => setOpts({ ...opts, passthrough: e.target.value })}
                        />
                      </Field>
                    </FieldGroup>
                  </CollapsibleContent>
                </Collapsible>
              </FieldGroup>
            </FieldSet>
          </div>
          <DialogFooter>
            {doneItems.length === 1 && doneItems[0]?.assetId && (
              <Button
                variant="outline"
                render={
                  <Link
                    to={videoPath(doneItems[0].assetId, doneItems[0].appId)}
                    onClick={() => close(false)}
                  />
                }
              >
                Open video
              </Button>
            )}
            <Button onClick={() => void start()} disabled={pending === 0 || active}>
              <UploadIcon data-icon="inline-start" />
              {pending > 1 ? `Upload ${pending} videos` : 'Upload video'}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </UploadContext.Provider>
  )
}

function Dropzone({ onFiles, compact }: { onFiles: (f: File[]) => void; compact: boolean }) {
  const [over, setOver] = React.useState(false)
  const input = React.useRef<HTMLInputElement>(null)
  return (
    <label
      className={cn(
        'flex cursor-pointer flex-col items-center justify-center gap-1.5 rounded-lg border border-dashed text-center transition-colors',
        compact ? 'px-4 py-4' : 'px-4 py-10',
        over ? 'border-primary bg-primary/5' : 'bg-muted/40 hover:bg-muted/70',
      )}
      onDragOver={(e) => {
        e.preventDefault()
        setOver(true)
      }}
      onDragLeave={() => setOver(false)}
      onDrop={(e) => {
        e.preventDefault()
        setOver(false)
        onFiles([...e.dataTransfer.files])
      }}
    >
      <UploadIcon className="size-5 text-muted-foreground" aria-hidden="true" />
      <span className="text-sm font-medium">
        {compact ? 'Add more videos' : 'Drop videos here, or choose files'}
      </span>
      {!compact && <span className="text-sm text-muted-foreground">MP4, MOV, MKV and WebM all work.</span>}
      <input
        ref={input}
        type="file"
        accept="video/*"
        multiple
        aria-label="Video file"
        className="sr-only"
        onChange={(e) => {
          onFiles([...(e.currentTarget.files ?? [])])
          e.currentTarget.value = ''
        }}
      />
    </label>
  )
}

function FileRow({
  item,
  onTitle,
  onRemove,
  onPause,
  onResume,
  onCancel,
  onOpen,
}: {
  item: Item
  onTitle: (t: string) => void
  onRemove: () => void
  onPause: () => void
  onResume: () => void
  onCancel: () => void
  onOpen: () => void
}) {
  const pct = Math.round(item.progress * 100)
  const editable = item.state === 'ready' || item.state === 'failed'
  return (
    <li className="flex flex-col gap-2 rounded-lg border p-3">
      <div className="flex items-center gap-2">
        {editable ? (
          <Input
            aria-label={`Title for ${item.file.name}`}
            value={item.title}
            maxLength={200}
            onChange={(e) => onTitle(e.target.value)}
            className="h-7 min-w-0 flex-1"
          />
        ) : (
          <span className="min-w-0 flex-1 truncate text-sm font-medium">{item.title || item.file.name}</span>
        )}
        <span className="shrink-0 text-xs text-muted-foreground tabular-nums">
          {item.state === 'uploading' && `${pct}%`}
          {item.state === 'paused' && `Paused, ${pct}%`}
          {item.state === 'done' && 'Uploaded'}
          {item.state === 'cancelled' && 'Cancelled'}
          {item.state === 'failed' && <span className="text-destructive">Failed</span>}
        </span>
        {item.state === 'uploading' && (
          <Button variant="ghost" size="icon-xs" aria-label="Pause" onClick={onPause}>
            <PauseIcon />
          </Button>
        )}
        {item.state === 'paused' && (
          <Button variant="ghost" size="icon-xs" aria-label="Resume" onClick={onResume}>
            <PlayIcon />
          </Button>
        )}
        {(item.state === 'uploading' || item.state === 'paused') && (
          <Button variant="ghost" size="icon-xs" aria-label="Cancel upload" onClick={onCancel}>
            <XIcon />
          </Button>
        )}
        {(editable || item.state === 'cancelled') && (
          <Button variant="ghost" size="icon-xs" aria-label={`Remove ${item.file.name}`} onClick={onRemove}>
            <XIcon />
          </Button>
        )}
        {item.state === 'done' && item.assetId && (
          <Button
            variant="link"
            size="xs"
            render={<Link to={videoPath(item.assetId, item.appId)} onClick={onOpen} />}
          >
            <CheckIcon data-icon="inline-start" />
            Open
          </Button>
        )}
      </div>
      {(item.state === 'uploading' || item.state === 'paused' || item.state === 'done') && (
        <Progress value={pct} aria-label={`Upload progress for ${item.file.name}`} />
      )}
      {item.error && <p className="text-xs text-destructive">{item.error}</p>}
    </li>
  )
}
