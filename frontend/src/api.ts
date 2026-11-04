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
  nextCommitSha: string | null // a pushed commit waiting for the deploy in progress
  createdAt: string
  currentDeployment: Deployment | null // the live one; null if nothing is live
}

// AppDetail matches appDetailResponse.
export type AppDetail = PlatformApp & {
  envKeys: string[] // names only: values are write-only
  deployments: Deployment[] // newest first, at most 20
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
