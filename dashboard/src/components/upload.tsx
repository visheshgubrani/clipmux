import {
  type Asset,
  ClipMuxUpload,
  type CreatedUpload,
  type RememberedUpload,
  rememberUpload,
  type UploadStore,
} from '@clipmux/uploader'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { CheckIcon, PauseIcon, PlayIcon, UploadIcon, XIcon } from 'lucide-react'
import * as React from 'react'
import { Link } from 'react-router'
import { del, LADDER, post, type Rung } from '@/api'
import { RenditionPicker, renditionsBody, renditionsNote, videoShortSide } from '@/components/renditions'
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
import { toast } from '@/components/ui/toast'
import { ToggleGroup, ToggleGroupItem } from '@/components/ui/toggle-group'
import { videoPath } from '@/lib/format'
import { notify } from '@/lib/notify'
import { cn } from '@/lib/utils'
import { q } from '@/queries'

// Upload (docs/design.md, D135): several files at once, each its own
// ordinary POST /v1/uploads as the chosen app (the Dashboard app by
// default), sent in parts straight to storage. A browser can't read a file
// again after a reload, so a reload lists the stopped uploads and adding the
// same file again resumes it.

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
  /** The video's short side, when the browser could read it. */
  shortSide?: number
}

interface Options {
  privacy: 'signed' | 'public'
  subtitles: boolean
  chapters: boolean
  app: string
  passthrough: string
  renditions: Rung[]
}

/** An upload a reload stopped: its file must be added again to go on. */
interface Stopped extends RememberedUpload {
  key: string
  /** The share storage already holds, when ClipMux could say. */
  progress?: number
}

// Where the uploader remembers each file's upload until it completes.
const store: UploadStore = (() => {
  try {
    return window.localStorage
  } catch {
    const m = new Map<string, string>()
    return {
      getItem: (k) => m.get(k) ?? null,
      setItem: (k, v) => void m.set(k, v),
      removeItem: (k) => void m.delete(k),
      get length() {
        return m.size
      },
      key: (i) => [...m.keys()][i] ?? null,
    }
  }
})()
const remembered = rememberUpload(store)

const titleFrom = (name: string) =>
  name
    .replace(/\.[^.]+$/, '')
    .replace(/[_-]+/g, ' ')
    .trim()
    .slice(0, 200)
const fileKey = (f: { name: string; size: number; lastModified: number }) =>
  `${f.name}:${f.size}:${f.lastModified}`

// An upload's own routes, with its token. Not api(): a refused token is a
// 401 there, which api() would take for the session ending.
const withToken = (u: CreatedUpload, path: string, init: RequestInit = {}) =>
  fetch(`${location.origin}/v1/uploads/${encodeURIComponent(u.upload_id)}${path}`, {
    ...init,
    headers: { Authorization: `Bearer ${u.upload_token}`, 'Content-Type': 'application/json' },
  })

// Whether a remembered upload is still open, and how much of it is stored;
// 'over' when ClipMux has ended it (completed, cancelled, expired, deleted).
async function check(r: RememberedUpload): Promise<'over' | { progress?: number }> {
  try {
    const res = await withToken(r.upload, '/parts')
    if (res.ok) {
      const { parts } = (await res.json()) as { parts: { size: number }[] }
      return { progress: parts.reduce((n, p) => n + p.size, 0) / Math.max(r.size, 1) }
    }
    if (res.status < 500 && res.status !== 429) return 'over'
  } catch {
    // ClipMux didn't answer: keep it
  }
  return {}
}

// A video whose upload was cancelled here has nothing in it, so it is
// deleted rather than left in the library (D135).
async function deleteCancelled(asset: Asset | null): Promise<void> {
  if (!asset?.id) return
  try {
    await del(`/v1/assets/${asset.id}`, typeof asset.app_id === 'string' ? asset.app_id : undefined)
  } catch (err) {
    notify.failed("Couldn't remove the cancelled video", err)
  }
}

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
  const [stopped, setStopped] = React.useState<Stopped[]>([])
  const [opts, setOpts] = React.useState<Options>({
    privacy: 'signed',
    subtitles: false,
    chapters: false,
    app: '',
    passthrough: '',
    renditions: [...LADDER],
  })
  const qc = useQueryClient()
  const apps = useQuery({ ...q.apps(), enabled: open })
  const chapters = useQuery({ ...q.chapters(), enabled: open })
  const llm = !!chapters.data?.connection

  const add = React.useCallback((files: File[]) => {
    const videos = files.filter(
      (f) => f.type.startsWith('video/') || /\.(mp4|mov|mkv|webm|m4v|avi|mxf|ts)$/i.test(f.name),
    )
    // A stopped upload's file is back: Upload resumes it from its memory.
    const added = new Set(videos.map(fileKey))
    setStopped((cur) => cur.filter((s) => !added.has(s.key)))
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
    // Each video's size, so the renditions above it show as skipped.
    for (const f of videos)
      void videoShortSide(f).then(
        (side) =>
          side && setItems((cur) => cur.map((i) => (i.key === fileKey(f) ? { ...i, shortSide: side } : i))),
      )
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

  const refreshLibrary = () => {
    void qc.invalidateQueries({ queryKey: ['library'] })
    void qc.invalidateQueries({ queryKey: ['library-counts'] })
  }

  // After a reload: the uploads this browser remembers that ClipMux still
  // has open. Ones it has ended are forgotten, so they start afresh.
  const checked = React.useRef(false)
  React.useEffect(() => {
    if (checked.current) return
    checked.current = true
    const found = remembered.list()
    if (found.length === 0) return
    void Promise.all(found.map(async (r) => ({ r, now: await check(r) }))).then((results) => {
      const open: Stopped[] = []
      for (const { r, now } of results) {
        if (now === 'over') remembered.forget(r)
        else open.push({ ...r, key: fileKey(r), ...now })
      }
      if (open.length === 0) return
      setStopped(open)
      toast.add({
        title:
          open.length === 1
            ? 'An upload stopped when the page reloaded'
            : `${open.length} uploads stopped when the page reloaded`,
        description: 'Add the file again to continue where it stopped.',
        actionProps: { children: 'Show', onClick: () => setOpen(true) },
      })
    })
  }, [])

  // Ends a stopped upload for good: cancelled, its empty video deleted.
  const discard = async (s: Stopped) => {
    let res: Response
    try {
      res = await withToken(s.upload, '/cancel', { method: 'POST', body: '{}' })
      if (res.status >= 500) throw new Error(`ClipMux answered HTTP ${res.status}`)
    } catch (err) {
      notify.failed(`Couldn't discard ${s.name}`, err)
      return
    }
    // Anything but 2xx means it had already ended.
    if (res.ok) await deleteCancelled((await res.json().catch(() => null)) as Asset | null)
    remembered.forget(s)
    setStopped((cur) => cur.filter((x) => x.key !== s.key))
    refreshLibrary()
  }

  const cancel = async (item: Item) => {
    const asset = await item.upload?.cancel()
    await deleteCancelled(asset ?? null)
    refreshLibrary()
  }

  const run = async (item: Item) => {
    update(item.key, { state: 'uploading', progress: 0 })
    try {
      const app = opts.app || undefined
      const upload = new ClipMuxUpload(item.file, {
        endpoint: location.origin,
        store,
        // A remembered upload ClipMux has ended is replaced by a new one.
        // Not given the signal: creating is quick, and a cancel meanwhile
        // waits for it, so the new upload is cancelled, not left waiting.
        create: () =>
          post<CreatedUpload>(
            '/v1/uploads',
            {
              size: item.file.size,
              playback_policy: opts.privacy,
              ...(item.title ? { title: item.title } : {}),
              ...(opts.passthrough && items.length === 1 ? { passthrough: opts.passthrough } : {}),
              ...(opts.subtitles || opts.chapters
                ? { generate: { subtitles: opts.subtitles || opts.chapters, chapters: opts.chapters && llm } }
                : {}),
              ...renditionsBody(opts.renditions),
            },
            app,
          ),
        onProgress: (p) => update(item.key, { progress: p.bytesUploaded / p.bytesTotal }),
      })
      update(item.key, { upload })
      const asset = await upload.start()
      update(item.key, {
        state: 'done',
        progress: 1,
        assetId: asset.id,
        // A resumed upload may belong to another app than the one picked now.
        ...(typeof asset.app_id === 'string' ? { appId: asset.app_id } : app ? { appId: app } : {}),
      })
      refreshLibrary()
    } catch (err) {
      if ((err as { code?: string }).code === 'cancelled') {
        update(item.key, { state: 'cancelled' })
        return
      }
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
  // Leaving or reloading would stop them: the browser asks first.
  React.useEffect(() => {
    if (!active) return
    const stay = (e: BeforeUnloadEvent) => e.preventDefault()
    window.addEventListener('beforeunload', stay)
    return () => window.removeEventListener('beforeunload', stay)
  }, [active])
  const shownStopped = stopped.filter((s) => !items.some((i) => i.key === s.key))
  // The largest video's short side, known only when every one was read.
  const maxHeight =
    items.length > 0 && items.every((i) => i.shortSide)
      ? Math.max(...items.map((i) => i.shortSide ?? 0))
      : undefined
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
              Uploads keep going while this tab is open. After a reload, add the same file again and it picks
              up where it stopped.
            </DialogDescription>
          </DialogHeader>
          {/* Room on the right for the switches' wider hit areas, which would
              otherwise overflow and let focus scroll the form sideways. */}
          <div className="-mr-3 flex max-h-[60vh] flex-col gap-5 overflow-y-auto pr-3">
            {shownStopped.length > 0 && (
              <section aria-labelledby="stopped-label" className="flex flex-col gap-2">
                <p id="stopped-label" className="text-sm text-muted-foreground">
                  Stopped by a reload. Add each file again to continue.
                </p>
                <ul className="flex flex-col gap-2">
                  {shownStopped.map((s) => (
                    <li key={s.key} className="flex items-center gap-2 rounded-lg border border-dashed p-3">
                      <span className="min-w-0 flex-1 truncate text-sm font-medium">{s.name}</span>
                      {/* Only finished parts count: bytes in flight at the reload are gone. */}
                      {!!s.progress && (
                        <span className="shrink-0 text-xs text-muted-foreground tabular-nums">
                          {Math.round(s.progress * 100)}% uploaded
                        </span>
                      )}
                      <Button
                        variant="ghost"
                        size="xs"
                        aria-label={`Discard ${s.name}`}
                        onClick={() => void discard(s)}
                      >
                        Discard
                      </Button>
                    </li>
                  ))}
                </ul>
              </section>
            )}
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
                    onCancel={() => void cancel(i)}
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
                <Field>
                  <FieldLabel id="renditions-label">Resolutions</FieldLabel>
                  <RenditionPicker
                    labelledBy="renditions-label"
                    value={opts.renditions}
                    maxHeight={maxHeight}
                    onChange={(renditions) => setOpts({ ...opts, renditions })}
                  />
                  <FieldDescription>{renditionsNote(opts.renditions, maxHeight)}</FieldDescription>
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
