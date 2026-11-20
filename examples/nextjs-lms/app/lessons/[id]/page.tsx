import { notFound } from 'next/navigation'
import { getLesson, label } from '@/lib/lessons'
import { currentUser } from '@/lib/session'
import { canTeach } from '@/lib/users'
import { LessonVideo } from './lesson-video'

export const dynamic = 'force-dynamic'

export default async function LessonPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = await params
  const [lesson, user] = await Promise.all([getLesson(id), currentUser()])
  if (!lesson) notFound()
  return (
    <>
      <h1>{lesson.title}</h1>
      <p className="status" role="status" aria-label="Video status">
        {label(lesson.status)}
      </p>
      {/* Only the playback ID reaches the browser; the route decides who may watch. */}
      <LessonVideo
        lessonId={lesson.id}
        playbackId={lesson.status === 'ready' ? lesson.playbackId : undefined}
        canUpload={canTeach(user)}
        waiting={lesson.status === 'waiting_upload' || lesson.status === 'processing'}
      />
    </>
  )
}
