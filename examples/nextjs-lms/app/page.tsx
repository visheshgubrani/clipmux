import Link from 'next/link'
import { label, listLessons } from '@/lib/lessons'
import { currentUser } from '@/lib/session'
import { canTeach } from '@/lib/users'
import { addLesson } from './actions'

export const dynamic = 'force-dynamic'

export default async function Home() {
  const [lessons, user] = await Promise.all([listLessons(), currentUser()])
  return (
    <>
      <h1>Lessons</h1>
      {lessons.length === 0 ? <p>No lessons yet.</p> : null}
      <ul>
        {lessons.map((l) => (
          <li key={l.id}>
            <Link href={`/lessons/${l.id}`}>{l.title}</Link> <span className="status">{label(l.status)}</span>
          </li>
        ))}
      </ul>
      {canTeach(user) ? (
        <form action={addLesson}>
          <h2>Add a lesson</h2>
          <label>
            Title <input name="title" required maxLength={200} />
          </label>{' '}
          <button type="submit">Add lesson</button>
        </form>
      ) : (
        <p className="status">Sign in as Ada, the instructor, to add lessons and upload their videos.</p>
      )}
    </>
  )
}
