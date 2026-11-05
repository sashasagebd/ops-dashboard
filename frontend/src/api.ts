// Types and fetchers for the dashboard's Go API, and for the platform's API,
// which the dashboard forwards.

// Container matches containerResponse in backend/internal/server/server.go.
export type Container = {
  id: string
  name: string
  image: string
  state: string // machine-readable, e.g. "running", "exited"
  // From the container's healthcheck; null if it has none or isn't running.
  health: 'starting' | 'healthy' | 'unhealthy' | null
  status: string // human-readable, e.g. "Up 3 hours"
  startedAt: string | null // RFC 3339; null if never started
  finishedAt: string | null // RFC 3339; null if never stopped
  cpuPercent: number | null // share of the whole host, 0–100; null until two samples
  memoryBytes: number | null // null if not running
  memoryLimitBytes: number | null // host memory if no limit is set
}

// HostStats matches hostResponse in the same Go file. Disk fields mean what
// df's columns do: df's "Use%" is used / (used + available), not used / total.
export type HostStats = {
  cpuPercent: number | null // 0–100, all cores together; null until two samples
  memoryBytes: number // used, i.e. total minus available
  memoryTotalBytes: number
  diskUsedBytes: number
  diskAvailableBytes: number
  diskTotalBytes: number
}

// ContainersResponse matches containersResponse in the same Go file.
export type ContainersResponse = {
  updatedAt: string // RFC 3339; when the server last polled Docker successfully
  stale: boolean // the latest poll failed, so the data is from updatedAt
  host: HostStats | null // null if the host couldn't be read that poll
  containers: Container[]
}

// HistoryWindow is a window GET /api/history accepts.
export type HistoryWindow = '1h' | '24h'

// Series is one value per step, oldest first; null is a gap (nothing recorded
// in that step), never 0.
export type Series = (number | null)[]

// HostHistory matches hostHistoryResponse in the same Go file.
export type HostHistory = {
  cpuPercent: Series // step average, 0–100
  cpuPercentMax: Series // step maximum, so short spikes still show
  memoryBytes: Series // used, step average
  diskUsedBytes: Series
}

// ContainerHistory matches containerHistoryResponse in the same Go file.
export type ContainerHistory = {
  cpuPercent: Series
  cpuPercentMax: Series
  memoryBytes: Series
}

// HistoryResponse matches historyResponse in the same Go file. Every series
// has the same length; value i covers the step starting at
// start + i * stepSeconds, and the last one is still filling.
export type HistoryResponse = {
  start: string // RFC 3339; start of the first step
  stepSeconds: number
  host: HostHistory
  containers: Record<string, ContainerHistory> // by container name
  // The deploy platform's apps, by app name: each sums its containers, which
  // change name with every deploy.
  apps: Record<string, ContainerHistory>
}

// Platform types match the response types in backend/internal/api/api.go,
// which the dashboard forwards /api/platform/ to.

// DeploymentStatus moves forward only: queued → building → starting →
// healthy or failed (a rollback skips building); healthy → superseded when a
// newer one goes live, or rolled_back when a rollback replaces it.
export type DeploymentStatus = 'queued' | 'building' | 'starting' | 'healthy' | 'failed' | 'superseded' | 'rolled_back'

// Deployment matches deploymentResponse.
export type Deployment = {
  id: number
  appId: number
  commitSha: string
  status: DeploymentStatus
  imageTag: string | null // null until built
  error: string | null // why it failed; null unless failed
  rollbackOf: number | null // the deployment whose image a rollback runs
  rollbackable: boolean // can be rolled back to (only in an app's deployment lists)
  createdAt: string // RFC 3339
  startedAt: string | null
  finishedAt: string | null
}

// PlatformApp matches appResponse.
export type PlatformApp = {
  id: number
  name: string
  repoUrl: string
  branch: string
  hostname: string // where it's served, <name>.<APPS_DOMAIN>
  port: number // inside its container
  healthCheckPath: string
  memoryLimitBytes: number
  cpuLimitMillicores: number // 1000 = one core
  sourceDir: string // folder in the repo with the Dockerfile; '' is the root
  autoDeploy: boolean // pushes to branch deploy it
  stopped: boolean // stopped on purpose: not running, not reachable, no deploys
  nextCommitSha: string | null // a pushed commit waiting for the deploy in progress
  createdAt: string
  currentDeployment: Deployment | null // the live one; null if nothing is live
  // Public on the internet (through Cloudflare) at <publicName>.<domain>;
  // both null if tailnet-only. publicHostname is also null while the server
  // has no public domain set up.
  publicName: string | null
  publicHostname: string | null
  requestsLastHour: number | null // from Caddy, tailnet and public together; null without readings yet
}

// AppDetail matches appDetailResponse.
export type AppDetail = PlatformApp & {
  envKeys: string[] // names only: values are write-only
  deployments: Deployment[] // newest first, at most 20
  events: PlatformEvent[] // the reconciler's, newest first, at most 20
  database: AppDatabase | null // null if the app has none
  backups: Backup[] // its database's, newest first, at most 7
  publicDomain: string | null // what a public name goes in front of; null if public hostnames aren't set up
}

// Backup matches backupResponse: one pg_dump of an app's database, a file in
// the backup directory under the app's name.
export type Backup = {
  id: number
  status: 'queued' | 'running' | 'done' | 'failed'
  file: string | null // the dump's name; null until done
  sizeBytes: number | null // null until done
  error: string | null // null unless failed
  createdAt: string
  finishedAt: string | null
}

// AppDatabase matches databaseResponse: an app's own Postgres. Its password
// is never shown; the app gets it inside DATABASE_URL.
export type AppDatabase = {
  name: string // its container, volume and network, e.g. ops-db-hello
  createdAt: string // when it was added
  // 'creating' until the platform first creates its container, then Docker's
  // state ('running', 'exited', ...), 'missing' while it's created again, or
  // 'unknown' if Docker couldn't be asked.
  state: string
}

// EventKind is what the reconciler did about something it found wrong.
export type EventKind =
  | 'restarted' // a stopped app container started again
  | 'recreated' // a missing app container created again
  | 'crash_looping' // restarted again, backing off: it keeps stopping
  | 'removed_orphan' // a container no live deployment uses, removed
  | 'routes_reapplied' // Caddy's routes didn't match, re-applied
  | 'fix_failed' // one of the above, tried and failed
  | 'database_created' // an app's database created for the first time

// PlatformEvent matches eventResponse.
export type PlatformEvent = {
  id: number
  at: string // RFC 3339
  appId: number | null // null if it isn't about one app
  kind: EventKind
  message: string
  output: string[] // a crashed container's last lines, oldest first
}

// NewApp is the body of POST /api/platform/apps. Optional fields left out
// get the platform's defaults.
export type NewApp = {
  name: string
  repoUrl: string
  port: number
  branch?: string
  healthCheckPath?: string
  sourceDir?: string
  memoryLimitBytes?: number
  cpuLimitMillicores?: number
}

// AppMetrics matches metricsResponse: an app's request numbers, one value
// per step from start, oldest first, null where there was no reading.
export type AppMetrics = {
  start: string
  stepSeconds: number
  requests: Series
  errors: Series // 5xx responses
  p95Ms: Series // 95th percentile response time; null also with no requests
}

// LogLine matches logLineResponse: one line of a deploy's log (the
// platform's stage messages, or Docker's build output) or of an app's output.
// It's the data of each message in a log stream (Server-Sent Events).
export type LogLine = {
  at: string
  stream: 'platform' | 'build' | 'stdout' | 'stderr'
  line: string
}

// deploymentLogURL is a deploy's log stream: what's stored, then new lines
// until the deploy has finished, then an "end" event.
export function deploymentLogURL(appId: number, deploymentId: number): string {
  return `/api/platform/apps/${appId}/deployments/${deploymentId}/logs`
}

// appLogURL is the live container's output: recent lines, then new ones,
// then an "end" event if the container stops or is replaced.
export function appLogURL(appId: number): string {
  return `/api/platform/apps/${appId}/logs`
}

export function fetchContainers(signal?: AbortSignal): Promise<ContainersResponse> {
  return getJSON<ContainersResponse>('/api/containers', signal)
}

export function fetchHistory(window: HistoryWindow, signal?: AbortSignal): Promise<HistoryResponse> {
  return getJSON<HistoryResponse>(`/api/history?window=${window}`, signal)
}

export function fetchApps(signal?: AbortSignal): Promise<PlatformApp[]> {
  return getJSON<PlatformApp[]>('/api/platform/apps', signal)
}

export function fetchApp(id: number, signal?: AbortSignal): Promise<AppDetail> {
  return getJSON<AppDetail>(`/api/platform/apps/${id}`, signal)
}

// fetchAppMetrics is an app's request numbers over the last hour or day.
export function fetchAppMetrics(appId: number, window: HistoryWindow, signal?: AbortSignal): Promise<AppMetrics> {
  return getJSON<AppMetrics>(`/api/platform/apps/${appId}/metrics?window=${window}`, signal)
}

// fetchEvents is the reconciler's most recent events, for every app.
export function fetchEvents(signal?: AbortSignal): Promise<PlatformEvent[]> {
  return getJSON<PlatformEvent[]>('/api/platform/events', signal)
}

export function createApp(app: NewApp): Promise<PlatformApp> {
  return sendJSON<PlatformApp>('POST', '/api/platform/apps', app)
}

// deployCommit starts a deploy; it runs in the background, so the returned
// deployment is still queued.
export function deployCommit(appId: number, commitSha: string): Promise<Deployment> {
  return sendJSON<Deployment>('POST', `/api/platform/apps/${appId}/deployments`, { commitSha })
}

// deployLatest deploys the newest commit on the app's branch, which the
// platform looks up on GitHub.
export function deployLatest(appId: number): Promise<Deployment> {
  return sendJSON<Deployment>('POST', `/api/platform/apps/${appId}/deployments/latest`)
}

// rollback rolls the app back to an earlier deployment: a new deployment
// that runs its image, with no build. It also turns the app's auto-deploy
// off.
export function rollback(appId: number, deploymentId: number): Promise<Deployment> {
  return sendJSON<Deployment>('POST', `/api/platform/apps/${appId}/deployments/${deploymentId}/rollback`)
}

// createDatabase gives the app a Postgres of its own. The platform creates it
// within seconds; the app gets DATABASE_URL from its next deploy.
export function createDatabase(appId: number): Promise<AppDatabase> {
  return sendJSON<AppDatabase>('POST', `/api/platform/apps/${appId}/database`)
}

// startBackup backs up the app's database now; it runs in the background.
export function startBackup(appId: number): Promise<Backup> {
  return sendJSON<Backup>('POST', `/api/platform/apps/${appId}/backups`)
}

// setPublicName makes the app public at <name>.<publicDomain> at once, or
// tailnet-only again with ''.
export function setPublicName(appId: number, publicName: string): Promise<PlatformApp> {
  return sendJSON<PlatformApp>('PATCH', `/api/platform/apps/${appId}`, { publicName })
}

// stopApp takes the app offline: its container is removed (its memory freed)
// and its addresses stop answering. Its database keeps running.
export function stopApp(appId: number): Promise<PlatformApp> {
  return sendJSON<PlatformApp>('POST', `/api/platform/apps/${appId}/stop`)
}

// startApp runs the app's live version again, with no build.
export function startApp(appId: number): Promise<PlatformApp> {
  return sendJSON<PlatformApp>('POST', `/api/platform/apps/${appId}/start`)
}

// DeletedApp matches deletedResponse: what deleting left on the server for
// the owner to remove by hand, if they want.
export type DeletedApp = {
  name: string
  databaseVolume: string | null // its database's data, a Docker volume; null if it had no database
}

export function deleteApp(appId: number): Promise<DeletedApp> {
  return sendJSON<DeletedApp>('DELETE', `/api/platform/apps/${appId}`)
}

export function setAutoDeploy(appId: number, autoDeploy: boolean): Promise<PlatformApp> {
  return sendJSON<PlatformApp>('PATCH', `/api/platform/apps/${appId}`, { autoDeploy })
}

export async function setEnvVar(appId: number, key: string, value: string): Promise<void> {
  await sendJSON('PUT', `/api/platform/apps/${appId}/env/${encodeURIComponent(key)}`, { value })
}

export async function deleteEnvVar(appId: number, key: string): Promise<void> {
  await sendJSON('DELETE', `/api/platform/apps/${appId}/env/${encodeURIComponent(key)}`)
}

async function getJSON<T>(url: string, signal?: AbortSignal): Promise<T> {
  const res = await fetch(url, { signal })
  if (!res.ok) throw await failure(res)
  return (await res.json()) as T
}

// sendJSON makes a request that changes something. Bodies are always sent as
// JSON: the platform refuses anything else, which is part of its CSRF
// protection. Resolves to the response's JSON, or undefined for 204.
async function sendJSON<T = undefined>(
  method: 'POST' | 'PUT' | 'PATCH' | 'DELETE',
  url: string,
  body?: unknown,
): Promise<T> {
  const res = await fetch(url, {
    method,
    headers: body === undefined ? undefined : { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  if (!res.ok) throw await failure(res)
  if (res.status === 204) return undefined as T
  return (await res.json()) as T
}

// failure turns an error response into an Error. The APIs send
// {"error": "..."}; fall back to the status line if the body is something
// else (e.g. a proxy error page).
async function failure(res: Response): Promise<Error> {
  const body: unknown = await res.json().catch(() => null)
  return new Error(errorMessage(body) ?? `Request failed: ${res.status} ${res.statusText}`)
}

function errorMessage(body: unknown): string | undefined {
  if (typeof body === 'object' && body !== null && 'error' in body && typeof body.error === 'string') {
    return body.error
  }
  return undefined
}
