import { useQuery, useQueryClient } from '@tanstack/react-query'
import { UploadIcon } from 'lucide-react'
import * as React from 'react'
import { Navigate, Outlet, Route, Routes, useLocation, useNavigate } from 'react-router'
import { whenSignedOut } from '@/api'
import { AppSidebar } from '@/components/app-sidebar'
import { ErrorBoundary } from '@/components/error-boundary'
import { ErrorState } from '@/components/query-state'
import { Button } from '@/components/ui/button'
import { SidebarInset, SidebarProvider, SidebarTrigger } from '@/components/ui/sidebar'
import { Skeleton } from '@/components/ui/skeleton'
import { UploadProvider, useUpload } from '@/components/upload'
import { Mark } from '@/components/wordmark'
import { AnalyticsPage } from '@/pages/analytics'
import { AppDetail, Apps } from '@/pages/apps'
import { Captions } from '@/pages/captions'
import { ConnectApp } from '@/pages/connect'
import { ConnectPC } from '@/pages/connect-pc'
import { Delivery } from '@/pages/delivery'
import { Encoding } from '@/pages/encoding'
import { Home } from '@/pages/home'
import { Jobs } from '@/pages/jobs'
import { Login } from '@/pages/login'
import { NotFound } from '@/pages/not-found'
import { Settings } from '@/pages/settings'
import { SetupWizard } from '@/pages/setup'
import { Storage } from '@/pages/storage'
import { UploadPage } from '@/pages/upload'
import { VideoPage } from '@/pages/video'
import { Videos } from '@/pages/videos'
import { q } from '@/queries'

export function App() {
  const setup = useQuery(q.setup())
  const session = useQuery(q.session())
  const qc = useQueryClient()
  const navigate = useNavigate()
  const loc = useLocation()

  // A request that finds the session gone sends the owner to sign in and back.
  React.useEffect(() => {
    whenSignedOut(() => {
      qc.setQueryData(['session'], null)
      const here = window.location.pathname + window.location.search
      if (!here.startsWith('/login') && !here.startsWith('/setup'))
        navigate(`/login?next=${encodeURIComponent(here)}`, { replace: true })
    })
  }, [qc, navigate])

  if (setup.isPending || session.isPending) return <Boot />
  if (setup.isError || session.isError)
    return (
      <div className="mx-auto w-full max-w-lg px-4 pt-24">
        <ErrorState
          what="ClipMux"
          error={setup.error ?? session.error}
          onRetry={() => {
            void setup.refetch()
            void session.refetch()
          }}
          retrying={setup.isFetching || session.isFetching}
        />
      </div>
    )

  const s = setup.data
  const signedIn = !!session.data
  return (
    <ErrorBoundary resetKey={loc.pathname}>
      <Routes>
        <Route path="/setup" element={<SetupWizard setup={s} signedIn={signedIn} />} />
        <Route path="/login" element={signedIn ? <AfterSignIn /> : <Login />} />
        {signedIn ? (
          <Route element={<Shell email={session.data?.email ?? ''} />}>
            {/* Until the URLs are set, the first-run flow is the place to be. */}
            <Route path="/" element={s.urls.confirmed ? <Home /> : <Navigate to="/setup" replace />} />
            <Route path="/videos" element={<Videos />} />
            <Route path="/videos/:id" element={<VideoPage />} />
            <Route path="/upload" element={<UploadPage />} />
            <Route path="/analytics" element={<AnalyticsPage />} />
            <Route path="/jobs" element={<Jobs />} />
            <Route path="/encoding" element={<Encoding />} />
            <Route path="/workers/connect" element={<ConnectPC />} />
            <Route path="/delivery" element={<Delivery />} />
            <Route path="/storage" element={<Storage />} />
            <Route path="/captions" element={<Captions />} />
            <Route path="/apps" element={<Apps server={s.urls.public_url} keysAllowed={s.keys_allowed} />} />
            <Route
              path="/apps/:id"
              element={<AppDetail server={s.urls.public_url} keysAllowed={s.keys_allowed} />}
            />
            <Route path="/connect" element={<ConnectApp keysAllowed={s.keys_allowed} />} />
            <Route path="/settings" element={<Settings setup={s} />} />
            {/* Where older links and the CLI point. */}
            <Route path="/library" element={<Navigate to="/videos" replace />} />
            <Route path="/assets/:id" element={<AssetRedirect />} />
            <Route path="/workers" element={<Navigate to="/encoding" replace />} />
            <Route path="/account" element={<Navigate to="/settings?tab=account" replace />} />
            <Route path="*" element={<NotFound />} />
          </Route>
        ) : (
          <Route path="*" element={s.owner_exists ? <ToSignIn /> : <Navigate to="/setup" replace />} />
        )}
      </Routes>
    </ErrorBoundary>
  )
}

function Shell({ email }: { email: string }) {
  const loc = useLocation()
  return (
    <UploadProvider>
      <SidebarProvider>
        <AppSidebar email={email} />
        <SidebarInset>
          <MobileBar />
          <ErrorBoundary resetKey={loc.pathname}>
            <Outlet />
          </ErrorBoundary>
        </SidebarInset>
      </SidebarProvider>
    </UploadProvider>
  )
}

/** Phones: the sidebar becomes a sheet behind this bar. */
function MobileBar() {
  const upload = useUpload()
  return (
    <div className="sticky top-0 z-10 flex h-14 items-center justify-between border-b bg-sidebar px-2 md:hidden">
      <SidebarTrigger className="size-10" aria-label="Open menu" />
      <span className="flex items-center gap-2">
        <Mark className="size-5" />
        <span className="font-semibold [font-stretch:118%]">ClipMux</span>
      </span>
      <Button size="icon-lg" aria-label="Upload" onClick={() => upload.open()}>
        <UploadIcon />
      </Button>
    </div>
  )
}

function Boot() {
  return (
    <div
      className="flex min-h-svh items-center justify-center"
      role="status"
      aria-busy="true"
      aria-label="Loading ClipMux"
    >
      <Skeleton className="size-10 rounded-lg" />
    </div>
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

function AssetRedirect() {
  const loc = useLocation()
  return <Navigate to={loc.pathname.replace(/^\/assets\//, '/videos/') + loc.search} replace />
}
