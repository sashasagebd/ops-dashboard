import { useCallback, useState } from 'react'
import {
  fetchContainers,
  fetchHistory,
  type Container,
  type ContainerHistory,
  type ContainersResponse,
  type HistoryResponse,
  type HistoryWindow,
} from './api'
import { formatBytes, formatCPU, formatDuration, uptimeText } from './format'
import { HostSummary } from './HostSummary'
import { seriesMax } from './series'
import { Sparkline } from './Sparkline'
import { usePolling, type PollState } from './usePolling'

// Matches the backend's default POLL_INTERVAL; refreshing faster would only
// fetch the same snapshot again.
const REFRESH_MS = 5000

// If the server's data is older than this, its poller has stopped updating
// even though it says nothing's wrong (e.g. stuck on a hung Docker call).
// Several missed polls, so one slow poll doesn't flash a warning.
const STALE_AFTER_MS = 30_000

// History is kept in 1-minute steps, so fetching it more often than this
// would mostly return the same points. It's a separate, slower poll from the
// live numbers: a day of history is far too much to resend every 5 seconds.
const HISTORY_REFRESH_MS = 60_000

const WINDOW_LABEL: Record<HistoryWindow, string> = { '1h': 'last hour', '24h': 'last 24 hours' }

// Badge colour per health status. "running" alone only means the process is
// alive; the health badge says whether the app inside is actually working,
// so "unhealthy" is red even though the container is up.
const HEALTH_BADGE = { healthy: 'up', starting: 'warn', unhealthy: 'down' } as const

function App() {
  const state = usePolling(fetchContainers, REFRESH_MS)

  // Changing the period gives usePolling a new fetcher, which restarts its
  // poll straight away; the previous period's lines stay up until then.
  const [period, setPeriod] = useState<HistoryWindow>('1h')
  const fetchWindow = useCallback((signal: AbortSignal) => fetchHistory(period, signal), [period])
  const historyState = usePolling(fetchWindow, HISTORY_REFRESH_MS)
  // History is extra: if it can't load, the page just has no trend lines,
  // rather than an error of its own (the stale banner already covers a
  // server that can't be reached).
  const history = historyState.status === 'ok' ? historyState.data : null

  return (
    <main>
      <header className="page-header">
        <h1>Ops Dashboard</h1>
        <WindowToggle value={period} onChange={setPeriod} />
      </header>
      <Dashboard state={state} history={history} windowLabel={WINDOW_LABEL[period]} />
    </main>
  )
}

// WindowToggle picks how far back the trend lines go.
function WindowToggle({ value, onChange }: { value: HistoryWindow; onChange: (w: HistoryWindow) => void }) {
  return (
    <div className="toggle" role="group" aria-label="Trend period">
      {(['1h', '24h'] as const).map((w) => (
        <button key={w} type="button" aria-pressed={w === value} onClick={() => onChange(w)}>
          {w}
        </button>
      ))}
    </div>
  )
}

type DashboardProps = {
  state: PollState<ContainersResponse>
  history: HistoryResponse | null // null until it has loaded, or if it can't
  windowLabel: string // e.g. "last hour", for the trend lines' names
}

function Dashboard({ state, history, windowLabel }: DashboardProps) {
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
          {/* Above both sections: it applies to all the data on the page. */}
          <StaleBanner state={state} />
          <h2>Server</h2>
          <HostSummary host={data.host} history={history?.host ?? null} windowLabel={windowLabel} />
          <h2>Containers</h2>
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
                  {data.containers.map((c) => {
                    // By name, like the history itself, so a container
                    // recreated by a deploy keeps its line.
                    const trend = history?.containers[c.name]
                    return (
                      <tr key={c.id}>
                        <td>{c.name}</td>
                        <td className="mono">{c.image}</td>
                        <td>
                          <span className="badges">
                            <span className={`badge badge-${c.state === 'running' ? 'up' : 'down'}`}>{c.state}</span>
                            {c.health !== null && (
                              <span className={`badge badge-${HEALTH_BADGE[c.health]}`}>{c.health}</span>
                            )}
                          </span>
                        </td>
                        {/* Docker's own status text stays available on hover: it
                            includes health ("(healthy)") and exit codes. */}
                        <td title={c.status}>{uptimeText(c, checkedAt)}</td>
                        <td className="num" title="Share of the whole host's CPU">
                          <CPUCell container={c} trend={trend} windowLabel={windowLabel} />
                        </td>
                        <td className="num">
                          <MemoryCell container={c} trend={trend} windowLabel={windowLabel} />
                        </td>
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            </div>
          )}
        </>
      )
    }
  }
}

type CellProps = { container: Container; trend: ContainerHistory | undefined; windowLabel: string }

// CPUCell is the live CPU % with its trend line, on the same 0–100 scale.
function CPUCell({ container: c, trend, windowLabel }: CellProps) {
  return (
    <span className="cell-trend">
      {trend && (
        <Sparkline
          values={trend.cpuPercent}
          peaks={trend.cpuPercentMax}
          max={100}
          label={`${c.name} CPU, ${windowLabel}`}
          format={formatCPU}
        />
      )}
      <span>{formatCPU(c.cpuPercent)}</span>
    </span>
  )
}

// MemoryCell is the live memory use with its trend line, scaled to the
// container's limit (the host's memory if it has none), so a full-height line
// means it's at its limit. A stopped container has no limit in the live data;
// its old line is then scaled to its own highest point.
function MemoryCell({ container: c, trend, windowLabel }: CellProps) {
  return (
    <span className="cell-trend">
      {trend && (
        <Sparkline
          values={trend.memoryBytes}
          max={c.memoryLimitBytes ?? seriesMax(trend.memoryBytes)}
          label={`${c.name} memory, ${windowLabel}`}
          format={formatBytes}
        />
      )}
      <span>{formatBytes(c.memoryBytes)}</span>
    </span>
  )
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
