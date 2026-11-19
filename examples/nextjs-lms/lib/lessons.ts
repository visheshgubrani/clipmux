// The lessons, kept in one JSON file so the example needs no database.
// Swap this for your ORM: the ClipMux side only needs a lesson's status,
// asset ID and playback ID, looked up by the passthrough `lesson_<id>`.

import { mkdir, readFile, rename, writeFile } from 'node:fs/promises'
import { join } from 'node:path'
import type { Asset } from '@clipmux/node'

export interface Lesson {
  id: string
  title: string
  /** ClipMux's asset status, or `none` before an upload. */
  status: 'none' | Asset['status']
  assetId?: string | undefined
  playbackId?: string | undefined
}

const dir = () => process.env.LMS_DATA_DIR ?? join(process.cwd(), '.data')
const file = () => join(dir(), 'lessons.json')

async function load(): Promise<Lesson[]> {
  try {
    return JSON.parse(await readFile(file(), 'utf8')) as Lesson[]
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === 'ENOENT') return []
    throw err
  }
}

async function save(lessons: Lesson[]): Promise<void> {
  await mkdir(dir(), { recursive: true })
  const tmp = `${file()}.${process.pid}.tmp`
  await writeFile(tmp, JSON.stringify(lessons, null, 2))
  await rename(tmp, file())
}

// One writer at a time within this process.
let queue: Promise<unknown> = Promise.resolve()
function locked<T>(fn: (lessons: Lesson[]) => Promise<T> | T): Promise<T> {
  const run = queue.then(async () => {
    const lessons = await load()
    const out = await fn(lessons)
    await save(lessons)
    return out
  })
  queue = run.catch(() => {})
  return run
}

export const listLessons = () => load()

export async function getLesson(id: string): Promise<Lesson | null> {
  return (await load()).find((l) => l.id === id) ?? null
}

export async function getLessonByPlaybackId(playbackId: string): Promise<Lesson | null> {
  return (await load()).find((l) => l.playbackId === playbackId) ?? null
}

export function createLesson(title: string): Promise<Lesson> {
  return locked((lessons) => {
    const lesson: Lesson = { id: String(lessons.length + 1), title, status: 'none' }
    lessons.push(lesson)
    return lesson
  })
}

export const passthroughFor = (lesson: Lesson) => `lesson_${lesson.id}`

/** Stores what a webhook says about the lesson's video. */
export function updateFromAsset(
  passthrough: string | null | undefined,
  update: Pick<Lesson, 'status' | 'assetId' | 'playbackId'>,
): Promise<boolean> {
  return locked((lessons) => {
    const lesson = lessons.find((l) => passthroughFor(l) === passthrough)
    if (!lesson) return false
    Object.assign(lesson, update)
    return true
  })
}

/** The status as the pages show it. */
export function label(status: string): string {
  switch (status) {
    case 'none':
      return 'No video yet'
    case 'waiting_upload':
      return 'Uploading'
    case 'processing':
      return 'Processing'
    case 'ready':
      return 'Ready'
    case 'errored':
      return 'Failed to encode'
    default:
      return status
  }
}
