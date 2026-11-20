import { cookies } from 'next/headers'
import { USER_COOKIE, type User, userById } from './users'

/** The signed-in user in a server component. */
export async function currentUser(): Promise<User | null> {
  return userById((await cookies()).get(USER_COOKIE)?.value)
}
