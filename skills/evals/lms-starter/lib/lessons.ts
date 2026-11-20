// The lessons, kept in one JSON file so the starter needs no database.

import { mkdir, readFile, rename, writeFile } from 'node:fs/promises'
import { join } from 'node:path'

export interface Lesson {
  id: string
  title: string
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
export function locked<T>(fn: (lessons: Lesson[]) => Promise<T> | T): Promise<T> {
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

export function createLesson(title: string): Promise<Lesson> {
  return locked((lessons) => {
    const lesson: Lesson = { id: String(lessons.length + 1), title }
    lessons.push(lesson)
    return lesson
  })
}
