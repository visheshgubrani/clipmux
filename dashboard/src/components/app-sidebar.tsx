import { useQuery, useQueryClient } from '@tanstack/react-query'
import {
  ActivityIcon,
  ChartLineIcon,
  ChevronsUpDownIcon,
  ClapperboardIcon,
  CodeXmlIcon,
  CpuIcon,
  DatabaseIcon,
  GlobeIcon,
  HouseIcon,
  LogOutIcon,
  MonitorIcon,
  MoonIcon,
  SettingsIcon,
  SubtitlesIcon,
  SunIcon,
  UploadIcon,
} from 'lucide-react'
import { NavLink, useLocation, useNavigate } from 'react-router'
import { api, type Job, type Page } from '@/api'
import { Button } from '@/components/ui/button'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarGroupContent,
  SidebarGroupLabel,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuBadge,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarRail,
  useSidebar,
} from '@/components/ui/sidebar'
import { useUpload } from '@/components/upload'
import { Mark } from '@/components/wordmark'
import { type Theme, useTheme } from '@/lib/theme'

const main = [
  { to: '/', label: 'Home', icon: HouseIcon, end: true },
  { to: '/videos', label: 'Videos', icon: ClapperboardIcon },
  { to: '/analytics', label: 'Analytics', icon: ChartLineIcon },
  { to: '/jobs', label: 'Jobs', icon: ActivityIcon },
]

const connections = [
  { to: '/encoding', label: 'Encoding', icon: CpuIcon },
  { to: '/storage', label: 'Storage', icon: DatabaseIcon },
  { to: '/delivery', label: 'Delivery', icon: GlobeIcon },
  { to: '/captions', label: 'Captions & AI', icon: SubtitlesIcon },
  { to: '/apps', label: 'Apps & API', icon: CodeXmlIcon },
]

function NavItem({
  to,
  label,
  icon: Icon,
  end,
  badge,
}: {
  to: string
  label: string
  icon: typeof HouseIcon
  end?: boolean
  badge?: number | undefined
}) {
  const { pathname } = useLocation()
  const { setOpenMobile } = useSidebar()
  const active = end ? pathname === to : pathname === to || pathname.startsWith(`${to}/`)
  return (
    <SidebarMenuItem>
      <SidebarMenuButton
        isActive={active}
        tooltip={label}
        render={<NavLink to={to} end={end ?? false} onClick={() => setOpenMobile(false)} />}
      >
        <Icon />
        <span>{label}</span>
      </SidebarMenuButton>
      {badge ? <SidebarMenuBadge>{badge}</SidebarMenuBadge> : null}
    </SidebarMenuItem>
  )
}

const themes: { value: Theme; label: string; icon: typeof SunIcon }[] = [
  { value: 'light', label: 'Light', icon: SunIcon },
  { value: 'dark', label: 'Dark', icon: MoonIcon },
  { value: 'system', label: 'System', icon: MonitorIcon },
]

export function AppSidebar({ email }: { email: string }) {
  const upload = useUpload()
  const navigate = useNavigate()
  const qc = useQueryClient()
  const [theme, setTheme] = useTheme()
  const { setOpenMobile } = useSidebar()
  const running = useQuery({
    queryKey: ['jobs', { status: 'running', badge: true }],
    queryFn: () => api<Page<Job>>('/v1/jobs?status=running&limit=50'),
    refetchInterval: 10_000,
  })
  const signOut = async () => {
    await api('/v1/session', { method: 'DELETE' }).catch(() => undefined)
    qc.setQueryData(['session'], null)
    qc.removeQueries({ predicate: (q) => q.queryKey[0] !== 'session' && q.queryKey[0] !== 'setup' })
    navigate('/login')
  }
  return (
    <Sidebar collapsible="icon">
      <SidebarHeader className="gap-3">
        <NavLink
          to="/"
          className="flex items-center gap-2.5 rounded-md px-1.5 py-1 outline-none"
          onClick={() => setOpenMobile(false)}
        >
          <Mark className="size-6" />
          <span className="text-[16px] font-semibold [font-stretch:118%] group-data-[collapsible=icon]:hidden">
            ClipMux
          </span>
        </NavLink>
        <Button
          className="w-full group-data-[collapsible=icon]:size-8 group-data-[collapsible=icon]:p-0"
          onClick={() => {
            setOpenMobile(false)
            upload.open()
          }}
          aria-label="Upload"
        >
          <UploadIcon data-icon="inline-start" />
          <span className="group-data-[collapsible=icon]:hidden">Upload</span>
        </Button>
      </SidebarHeader>
      <SidebarContent>
        <nav aria-label="Main" className="contents">
          <SidebarGroup>
            <SidebarGroupContent>
              <SidebarMenu>
                {main.map((i) => (
                  <NavItem
                    key={i.to}
                    {...i}
                    badge={i.to === '/jobs' ? running.data?.data.length : undefined}
                  />
                ))}
              </SidebarMenu>
            </SidebarGroupContent>
          </SidebarGroup>
          <SidebarGroup>
            <SidebarGroupLabel>Connections</SidebarGroupLabel>
            <SidebarGroupContent>
              <SidebarMenu>
                {connections.map((i) => (
                  <NavItem key={i.to} {...i} />
                ))}
              </SidebarMenu>
            </SidebarGroupContent>
          </SidebarGroup>
          <SidebarGroup className="mt-auto">
            <SidebarGroupContent>
              <SidebarMenu>
                <NavItem to="/settings" label="Settings" icon={SettingsIcon} />
              </SidebarMenu>
            </SidebarGroupContent>
          </SidebarGroup>
        </nav>
      </SidebarContent>
      <SidebarFooter>
        <SidebarMenu>
          <SidebarMenuItem>
            <DropdownMenu>
              <DropdownMenuTrigger
                render={<SidebarMenuButton size="lg" tooltip={email} aria-label="Account" />}
              >
                <span
                  aria-hidden="true"
                  className="flex size-7 shrink-0 items-center justify-center rounded-md bg-accent text-xs font-semibold uppercase"
                >
                  {email.slice(0, 1)}
                </span>
                <span className="min-w-0 flex-1 truncate text-left text-sm">{email}</span>
                <ChevronsUpDownIcon className="text-muted-foreground" />
              </DropdownMenuTrigger>
              <DropdownMenuContent side="top" align="start" className="min-w-56">
                <DropdownMenuGroup>
                  <DropdownMenuLabel className="truncate">{email}</DropdownMenuLabel>
                </DropdownMenuGroup>
                <DropdownMenuSeparator />
                <DropdownMenuGroup>
                  <DropdownMenuLabel>Theme</DropdownMenuLabel>
                  <DropdownMenuRadioGroup value={theme} onValueChange={(v) => setTheme(v as Theme)}>
                    {themes.map((t) => (
                      <DropdownMenuRadioItem key={t.value} value={t.value}>
                        <t.icon />
                        {t.label}
                      </DropdownMenuRadioItem>
                    ))}
                  </DropdownMenuRadioGroup>
                </DropdownMenuGroup>
                <DropdownMenuSeparator />
                <DropdownMenuGroup>
                  <DropdownMenuItem onClick={() => navigate('/settings')}>
                    <SettingsIcon />
                    Settings
                  </DropdownMenuItem>
                  <DropdownMenuItem onClick={() => void signOut()}>
                    <LogOutIcon />
                    Sign out
                  </DropdownMenuItem>
                </DropdownMenuGroup>
              </DropdownMenuContent>
            </DropdownMenu>
          </SidebarMenuItem>
        </SidebarMenu>
      </SidebarFooter>
      <SidebarRail />
    </Sidebar>
  )
}
