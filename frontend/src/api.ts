// Types and fetchers for the dashboard's Go API.

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

export function fetchContainers(signal?: AbortSignal): Promise<ContainersResponse> {
  return getJSON<ContainersResponse>('/api/containers', signal)
}

export function fetchHistory(window: HistoryWindow, signal?: AbortSignal): Promise<HistoryResponse> {
  return getJSON<HistoryResponse>(`/api/history?window=${window}`, signal)
}

async function getJSON<T>(url: string, signal?: AbortSignal): Promise<T> {
  const res = await fetch(url, { signal })
  if (!res.ok) {
    // The API sends {"error": "..."} on failure; fall back to the status line
    // if the body is something else (e.g. a proxy error page).
    const body: unknown = await res.json().catch(() => null)
    throw new Error(errorMessage(body) ?? `Request failed: ${res.status} ${res.statusText}`)
  }
  return (await res.json()) as T
}

function errorMessage(body: unknown): string | undefined {
  if (typeof body === 'object' && body !== null && 'error' in body && typeof body.error === 'string') {
    return body.error
  }
  return undefined
}
