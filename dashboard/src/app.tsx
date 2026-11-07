import { useCallback, useEffect, useState } from 'react'
import { BrowserRouter, Link, Navigate, NavLink, Route, Routes, useLocation, useNavigate } from 'react-router'
import { api, type Setup } from './api.ts'
import { Account } from './pages/account.tsx'
import { AppDetail, Apps } from './pages/apps.tsx'
import { AssetPage } from './pages/asset.tsx'
import { Delivery } from './pages/delivery.tsx'
import { Jobs } from './pages/jobs.tsx'
import { Library } from './pages/library.tsx'
import { Login } from './pages/login.tsx'
import { SetupWizard } from './pages/setup.tsx'
import { Upload } from './pages/upload.tsx'
import { ConnectPC, Workers } from './pages/workers.tsx'

type Session = { email: string } | null

export function App() {
  const [setup, setSetup] = useState<Setup | null>(null)
  const [session, setSession] = useState<Session | undefined>(undefined)
  const refresh = useCallback(async () => {
    setSetup(await api<Setup>('/v1/setup'))
    setSession(await api<{ email: string }>('/v1/session').catch(() => null))
  }, [])
  useEffect(() => {
    void refresh()
  }, [refresh])
  if (!setup || session === undefined) return <p className="p-8 text-sm text-zinc-500">Loading…</p>
  const needsSetup =
    !setup.owner_exists || !setup.urls.confirmed || !setup.setup_test || setup.setup_test.status !== 'passed'
  return (
    <BrowserRouter>
      {session && <Nav email={session.email} onSignOut={refresh} />}
      <main className="mx-auto grid max-w-5xl gap-6 p-6">
        <Routes>
          <Route
            path="/setup"
            element={<SetupWizard setup={setup} signedIn={!!session} onChange={refresh} />}
          />
          <Route path="/login" element={session ? <AfterSignIn /> : <Login onSignIn={refresh} />} />
          {session ? (
            <>
              <Route path="/" element={needsSetup ? <Navigate to="/setup" /> : <Navigate to="/library" />} />
              <Route path="/library" element={<Library />} />
              <Route path="/upload" element={<Upload />} />
              <Route path="/assets/:id" element={<AssetPage />} />
              <Route path="/apps" element={<Apps />} />
              <Route path="/apps/:id" element={<AppDetail keysAllowed={setup.keys_allowed} />} />
              <Route path="/delivery" element={<Delivery />} />
              <Route path="/workers" element={<Workers />} />
              <Route path="/jobs" element={<Jobs />} />
              <Route path="/workers/connect" element={<ConnectPC />} />
              <Route path="/account" element={<Account />} />
            </>
          ) : (
            <Route path="*" element={setup.owner_exists ? <ToSignIn /> : <Navigate to="/setup" />} />
          )}
        </Routes>
      </main>
    </BrowserRouter>
  )
}

// ToSignIn sends a signed-out visitor to sign in and back, so a link such as
// the one `clipmux worker` opens still lands on its page afterwards.
function ToSignIn() {
  const loc = useLocation()
  const next = loc.pathname === '/' ? '' : `?next=${encodeURIComponent(loc.pathname + loc.search)}`
  return <Navigate to={`/login${next}`} replace />
}

function AfterSignIn() {
  const next = new URLSearchParams(useLocation().search).get('next') ?? '/'
  // Only paths on this dashboard, never another origin.
  return <Navigate to={next.startsWith('/') && !next.startsWith('//') ? next : '/'} replace />
}

function Nav({ email, onSignOut }: { email: string; onSignOut: () => void }) {
  const navigate = useNavigate()
  const link = ({ isActive }: { isActive: boolean }) =>
    isActive ? 'font-semibold' : 'text-zinc-600 hover:text-zinc-900'
  return (
    <header className="border-b border-zinc-200 bg-white">
      <nav className="mx-auto flex max-w-5xl items-center gap-6 px-6 py-3 text-sm">
        <Link to="/" className="font-bold">
          ClipMux
        </Link>
        <NavLink to="/library" className={link}>
          Library
        </NavLink>
        <NavLink to="/upload" className={link}>
          Upload
        </NavLink>
        <NavLink to="/apps" className={link}>
          Apps
        </NavLink>
        <NavLink to="/delivery" className={link}>
          Delivery
        </NavLink>
        <NavLink to="/jobs" className={link}>
          Jobs
        </NavLink>
        <NavLink to="/workers" className={link} end>
          Workers
        </NavLink>
        <NavLink to="/setup" className={link}>
          Setup
        </NavLink>
        <NavLink to="/account" className={({ isActive }) => `ml-auto ${link({ isActive })}`}>
          {email}
        </NavLink>
        <button
          type="button"
          className="text-zinc-600 hover:text-zinc-900"
          onClick={async () => {
            await api('/v1/session', { method: 'DELETE' })
            onSignOut()
            navigate('/login')
          }}
        >
          Sign out
        </button>
      </nav>
    </header>
  )
}
