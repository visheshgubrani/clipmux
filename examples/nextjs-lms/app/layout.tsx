import type { Metadata } from 'next'
import Link from 'next/link'
import type { ReactNode } from 'react'
import { currentUser } from '@/lib/session'
import { USERS } from '@/lib/users'
import { signIn } from './actions'
import './style.css'

export const metadata: Metadata = { title: 'Course · ClipMux example' }

export default async function RootLayout({ children }: { children: ReactNode }) {
  const user = await currentUser()
  return (
    <html lang="en">
      <body>
        <header>
          <Link href="/">Video course</Link>
          <form action={signIn}>
            <label>
              Signed in as{' '}
              <select name="user" defaultValue={user?.id ?? ''} aria-label="Signed in as">
                <option value="">nobody</option>
                {USERS.map((u) => (
                  <option key={u.id} value={u.id}>
                    {u.name}
                  </option>
                ))}
              </select>
            </label>{' '}
            <button type="submit">Switch</button>
          </form>
        </header>
        <main>{children}</main>
      </body>
    </html>
  )
}
