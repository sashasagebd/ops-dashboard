// Types and fetchers for the dashboard's Go API.

// Container matches containerResponse in backend/internal/server/server.go.
export type Container = {
  id: string
  name: string
  image: string
  state: string // machine-readable, e.g. "running", "exited"
  status: string // human-readable, e.g. "Up 3 hours"
}

export async function fetchContainers(signal?: AbortSignal): Promise<Container[]> {
  const res = await fetch('/api/containers', { signal })
  if (!res.ok) {
    // The API sends {"error": "..."} on failure; fall back to the status line
    // if the body is something else (e.g. a proxy error page).
    const body: unknown = await res.json().catch(() => null)
    throw new Error(errorMessage(body) ?? `Request failed: ${res.status} ${res.statusText}`)
  }
  return (await res.json()) as Container[]
}

function errorMessage(body: unknown): string | undefined {
  if (typeof body === 'object' && body !== null && 'error' in body && typeof body.error === 'string') {
    return body.error
  }
  return undefined
}
