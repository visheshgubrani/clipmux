import { infiniteQueryOptions, QueryClient, queryOptions } from '@tanstack/react-query'
import {
  type Agent,
  type Analytics,
  type APIKey,
  ApiError,
  type App,
  type Asset,
  api,
  type ChaptersStatus,
  type Cloudflare,
  type Counts,
  type EdgeAnalytics,
  type Job,
  type LibraryAsset,
  type Moves,
  type Page,
  type Provider,
  type Range,
  type Routing,
  type Session,
  type Setup,
  type SetupTest,
  type ShareLink,
  type SigningKey,
  type SpeechStatus,
  type StorageConfig,
  type WebhookDelivery,
  type WebhookEndpoint,
} from './api.ts'

// Server state for every page (docs/design.md, "Implementation notes"):
// one cache, one retry rule, polling only while something is moving.
export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // A 4xx is an answer, not a blip: show it at once. Anything else gets
      // one more try before the page shows its error and Try again.
      retry: (count, err) => !(err instanceof ApiError && err.status < 500) && count < 1,
      refetchOnWindowFocus: false,
      staleTime: 5_000,
    },
  },
})

const busyAsset = (a: Asset) =>
  a.status === 'waiting_upload' || a.status === 'processing' || a.tracks.some((t) => t.status === 'preparing')

export interface LibraryFilter {
  q?: string
  status?: string
  app_id?: string
  include_deleted?: boolean
}

function search(params: Record<string, string | number | boolean | undefined>) {
  const s = new URLSearchParams()
  for (const [k, v] of Object.entries(params))
    if (v !== undefined && v !== '' && v !== false) s.set(k, String(v))
  const out = s.toString()
  return out ? `?${out}` : ''
}

export const q = {
  setup: () => queryOptions({ queryKey: ['setup'], queryFn: () => api<Setup>('/v1/setup') }),
  // Signed out is an answer here, not an error.
  session: () =>
    queryOptions({
      queryKey: ['session'],
      queryFn: () =>
        api<Session>('/v1/session').catch((e) => {
          if (e instanceof ApiError && e.status === 401) return null
          throw e
        }),
    }),
  setupTest: () =>
    queryOptions({
      queryKey: ['setup-test'],
      queryFn: () =>
        api<SetupTest>('/v1/setup-test/latest').catch((e) => {
          if (e instanceof ApiError && e.status === 404) return null
          throw e
        }),
      refetchInterval: (query) => {
        const s = query.state.data?.status
        return s && s !== 'passed' && s !== 'failed' ? 2000 : false
      },
    }),

  library: (f: LibraryFilter, limit = 48) =>
    infiniteQueryOptions({
      queryKey: ['library', f, limit],
      queryFn: ({ pageParam }) =>
        api<Page<LibraryAsset>>(`/v1/library${search({ ...f, limit, cursor: pageParam })}`),
      initialPageParam: undefined as string | undefined,
      getNextPageParam: (last) => last.next_cursor,
      refetchInterval: (query) =>
        query.state.data?.pages.some((p) => p.data.some(busyAsset)) ? 5000 : false,
    }),
  // One page of the library, for lists that don't page (Home).
  latest: (f: LibraryFilter, limit: number) =>
    queryOptions({
      queryKey: ['library', f, limit, 'first'],
      queryFn: () => api<Page<LibraryAsset>>(`/v1/library${search({ ...f, limit })}`),
      refetchInterval: (query) => (query.state.data?.data.some(busyAsset) ? 5000 : false),
    }),
  counts: (app_id?: string) =>
    queryOptions({
      queryKey: ['library-counts', app_id ?? ''],
      queryFn: () => api<Counts>(`/v1/library/counts${search({ app_id })}`),
    }),
  asset: (id: string, app?: string) =>
    queryOptions({
      queryKey: ['asset', id, app ?? ''],
      queryFn: () => api<Asset>(`/v1/assets/${id}`, { app }),
      refetchInterval: (query) => (query.state.data && busyAsset(query.state.data) ? 3000 : false),
    }),
  assetJobs: (id: string, enabled: boolean) =>
    queryOptions({
      queryKey: ['jobs', { asset_id: id }],
      queryFn: () => api<Page<Job>>(`/v1/jobs${search({ asset_id: id, limit: 5 })}`),
      enabled,
      refetchInterval: enabled ? 3000 : false,
    }),

  jobs: (f: { status?: string; asset_id?: string; agent_id?: string }) =>
    infiniteQueryOptions({
      queryKey: ['jobs', f],
      queryFn: ({ pageParam }) => api<Page<Job>>(`/v1/jobs${search({ ...f, limit: 50, cursor: pageParam })}`),
      initialPageParam: undefined as string | undefined,
      getNextPageParam: (last) => last.next_cursor,
      // Follow running jobs closely, and pick up new ones while nothing runs.
      refetchInterval: (query) =>
        query.state.data?.pages.some((p) =>
          p.data.some((j) => j.status === 'queued' || j.status === 'running'),
        )
          ? 3000
          : 10000,
    }),

  apps: () => queryOptions({ queryKey: ['apps'], queryFn: () => api<Page<App>>('/v1/apps') }),
  apiKeys: (app: string) =>
    queryOptions({
      queryKey: ['apps', app, 'keys'],
      queryFn: () => api<Page<APIKey>>(`/v1/apps/${app}/api-keys`),
    }),
  signingKeys: (app: string) =>
    queryOptions({
      queryKey: ['apps', app, 'signing'],
      queryFn: () => api<Page<SigningKey>>(`/v1/apps/${app}/signing-keys`),
    }),
  webhooks: (app: string) =>
    queryOptions({
      queryKey: ['apps', app, 'webhooks'],
      queryFn: () => api<Page<WebhookEndpoint>>(`/v1/apps/${app}/webhook-endpoints`),
    }),
  deliveries: (endpoint: string) =>
    queryOptions({
      queryKey: ['deliveries', endpoint],
      queryFn: () => api<Page<WebhookDelivery>>(`/v1/webhook-endpoints/${endpoint}/deliveries`),
      refetchInterval: 10000,
    }),

  agents: () =>
    queryOptions({
      queryKey: ['agents'],
      queryFn: () => api<Page<Agent>>('/v1/agents'),
      refetchInterval: 10000,
    }),
  routing: () => queryOptions({ queryKey: ['routing'], queryFn: () => api<Routing>('/v1/settings/routing') }),
  executors: () =>
    queryOptions({
      queryKey: ['executors'],
      queryFn: () => api<Page<Provider>>('/v1/executors'),
      refetchInterval: 10000,
    }),
  speech: () =>
    queryOptions({ queryKey: ['speech'], queryFn: () => api<SpeechStatus>('/v1/settings/speech') }),
  chapters: () =>
    queryOptions({ queryKey: ['chapters'], queryFn: () => api<ChaptersStatus>('/v1/settings/chapters') }),
  cloudflare: () =>
    queryOptions({ queryKey: ['cloudflare'], queryFn: () => api<Cloudflare>('/v1/cloudflare') }),
  moves: () =>
    queryOptions({
      queryKey: ['moves'],
      queryFn: () => api<Moves>('/v1/storage/moves'),
      refetchInterval: (query) => (query.state.data?.active ? 5000 : false),
    }),
  storage: () =>
    queryOptions({ queryKey: ['storage'], queryFn: () => api<Page<StorageConfig>>('/v1/storage') }),

  shares: (asset: string, app?: string) =>
    queryOptions({
      queryKey: ['shares', asset],
      queryFn: () => api<Page<ShareLink>>(`/v1/assets/${asset}/shares`, app ? { app } : {}),
    }),
  edgeAnalytics: () =>
    queryOptions({
      queryKey: ['edge-analytics'],
      queryFn: () => api<EdgeAnalytics>('/v1/cloudflare/analytics'),
    }),

  // A server older than the analytics API answers 404: "not collecting",
  // which pages show as an empty state rather than an error.
  analytics: (range: Range, app_id?: string, asset?: string) =>
    queryOptions({
      queryKey: ['analytics', range, app_id ?? '', asset ?? ''],
      queryFn: () =>
        api<Analytics>(
          asset
            ? `/v1/analytics/assets/${asset}${search({ range })}`
            : `/v1/analytics${search({ range, app_id })}`,
        ).catch((e) => {
          if (e instanceof ApiError && e.status === 404) return null
          throw e
        }),
    }),
}
