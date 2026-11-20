import { notFound } from 'next/navigation'
import { getLesson } from '@/lib/lessons'
import { currentUser } from '@/lib/session'
import { canTeach, canWatch } from '@/lib/users'

export const dynamic = 'force-dynamic'

export default async function LessonPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = await params
  const [lesson, user] = await Promise.all([getLesson(id), currentUser()])
  if (!lesson) notFound()
  return (
    <>
      <h1>{lesson.title}</h1>
      <section aria-label="Video">
        {canWatch(user) ? (
          <p className="status">This lesson has no video yet.</p>
        ) : (
          <p className="notice">Sign in as an enrolled student to watch this lesson.</p>
        )}
      </section>
      {canTeach(user) ? (
        <section>
          <h2>Lesson video</h2>
          <p className="status">Video upload is coming soon.</p>
        </section>
      ) : null}
    </>
  )
}
