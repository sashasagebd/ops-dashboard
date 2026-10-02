import { fetchContainers, type ContainersResponse } from './api'
import { formatBytes, formatCPU, formatDuration, uptimeText } from './format'
import { usePolling, type PollState } from './usePolling'

// Matches the backend's default POLL_INTERVAL; refreshing faster would only
// fetch the same snapshot again.
const REFRESH_MS = 5000

// If the server's data is older than this, its poller has stopped updating
// even though it says nothing's wrong (e.g. stuck on a hung Docker call).
// Several missed polls, so one slow poll doesn't flash a warning.
const STALE_AFTER_MS = 30_000

function App() {
  const state = usePolling(fetchContainers, REFRESH_MS)

  return (
    <main>
      <h1>Containers</h1>
      <ContainerList state={state} />
    </main>
  )
}

function ContainerList({ state }: { state: PollState<ContainersResponse> }) {
  switch (state.status) {
    case 'loading':
      return <p>Loading…</p>
    case 'error':
      return <p role="alert">Couldn't load containers: {state.message}</p>
    case 'ok': {
      // checkedAt is the "now" uptimes and ages are measured against. It's
      // captured when each fetch finishes rather than read during render,
      // which keeps rendering pure and every row consistent with the others.
      const { data, checkedAt } = state
      return (
        <>
          <StaleBanner state={state} />
          {data.containers.length === 0 ? (
            <p>No containers.</p>
          ) : (
            // The wrapper scrolls sideways on narrow screens instead of the
            // whole page.
            <div className="table-scroll">
              <table>
                <thead>
                  <tr>
                    <th scope="col">Name</th>
                    <th scope="col">Image</th>
                    <th scope="col">State</th>
                    <th scope="col">Uptime</th>
                    <th scope="col" className="num">
                      CPU
                    </th>
                    <th scope="col" className="num">
                      Memory
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {data.containers.map((c) => (
                    <tr key={c.id}>
                      <td>{c.name}</td>
                      <td className="mono">{c.image}</td>
                      <td>
                        <span className={`badge badge-${c.state === 'running' ? 'up' : 'down'}`}>{c.state}</span>
                      </td>
                      {/* Docker's own status text stays available on hover: it
                          includes health ("(healthy)") and exit codes. */}
                      <td title={c.status}>{uptimeText(c, checkedAt)}</td>
                      <td className="num" title="Share of the whole host's CPU">
                        {formatCPU(c.cpuPercent)}
                      </td>
                      <td className="num">{formatBytes(c.memoryBytes)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </>
      )
    }
  }
}

// StaleBanner warns when the table may not reflect what's running now, and
// says which link in the chain is broken.
function StaleBanner({ state }: { state: Extract<PollState<ContainersResponse>, { status: 'ok' }> }) {
  const { data, checkedAt, refreshError } = state
  const age = checkedAt - Date.parse(data.updatedAt)

  let reason: string | null = null
  if (refreshError !== null) reason = "Can't reach the dashboard server"
  else if (data.stale) reason = "The dashboard can't reach Docker"
  else if (age > STALE_AFTER_MS) reason = 'Updates from Docker have stopped'
  if (reason === null) return null

  return (
    <p role="status" className="banner">
      {reason}. Showing data from {formatDuration(age)} ago.
    </p>
  )
}

export default App
