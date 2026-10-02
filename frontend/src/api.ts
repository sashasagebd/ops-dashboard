// Types and fetchers for the dashboard's Go API.

// Container matches containerResponse in backend/internal/server/server.go.
export type Container = {
  id: string
  name: string
  image: string
  state: string // machine-readable, e.g. "running", "exited"
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

export async function fetchContainers(signal?: AbortSignal): Promise<ContainersResponse> {
  const res = await fetch('/api/containers', { signal })
  if (!res.ok) {
    // The API sends {"error": "..."} on failure; fall back to the status line
    // if the body is something else (e.g. a proxy error page).
    const body: unknown = await res.json().catch(() => null)
    throw new Error(errorMessage(body) ?? `Request failed: ${res.status} ${res.statusText}`)
  }
  return (await res.json()) as ContainersResponse
}

function errorMessage(body: unknown): string | undefined {
  if (typeof body === 'object' && body !== null && 'error' in body && typeof body.error === 'string') {
    return body.error
  }
  return undefined
}
