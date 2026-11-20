'use server'

import { cookies } from 'next/headers'
import { redirect } from 'next/navigation'
import { createLesson } from '@/lib/lessons'
import { canTeach, USER_COOKIE, userById } from '@/lib/users'

export async function signIn(form: FormData) {
  const user = userById(String(form.get('user') ?? ''))
  const jar = await cookies()
  if (user) jar.set(USER_COOKIE, user.id, { httpOnly: true, sameSite: 'lax', path: '/' })
  else jar.delete(USER_COOKIE)
  redirect(String(form.get('back') ?? '/'))
}

export async function addLesson(form: FormData) {
  const jar = await cookies()
  if (!canTeach(userById(jar.get(USER_COOKIE)?.value))) throw new Error('only the instructor adds lessons')
  const title = String(form.get('title') ?? '').trim()
  if (!title) return
  const lesson = await createLesson(title.slice(0, 200))
  redirect(`/lessons/${lesson.id}`)
}
