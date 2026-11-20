// Stand-in accounts. A real LMS has sign-in and enrolments; this one has
// three people and a cookie that says which one you are, so the access
// rules have something to decide on.

export type Role = 'instructor' | 'student' | 'visitor'

export interface User {
  id: string
  name: string
  role: Role
}

export const USERS: User[] = [
  { id: 'ada', name: 'Ada (instructor)', role: 'instructor' },
  { id: 'ben', name: 'Ben (enrolled student)', role: 'student' },
  { id: 'cy', name: 'Cy (not enrolled)', role: 'visitor' },
]

export const USER_COOKIE = 'lms_user'

export function userById(id: string | undefined): User | null {
  return USERS.find((u) => u.id === id) ?? null
}

/** The signed-in user, from a Fetch request's cookie header. */
export function userFromRequest(req: Request): User | null {
  const cookies = req.headers.get('cookie') ?? ''
  const match = cookies.split(/;\s*/).find((c) => c.startsWith(`${USER_COOKIE}=`))
  return userById(match?.slice(USER_COOKIE.length + 1))
}

/** Who may upload a lesson's video. */
export const canTeach = (user: User | null) => user?.role === 'instructor'

/** Who may watch: the instructor and enrolled students. */
export const canWatch = (user: User | null) => user?.role === 'instructor' || user?.role === 'student'
