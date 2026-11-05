import { useCallback } from 'react'
import { fetchApps, fetchContainers, fetchEvents, fetchHistory } from './api'
import { AppCard, EventsList } from './appParts'
import { HISTORY_REFRESH_MS, REFRESH_MS, StaleBanner } from './Containers'
import { formatBytes, formatCPU } from './format'
import { HostSummary } from './HostSummary'
import { Card, Loaded, PageHeader } from './ui'
import { usePolling } from './usePolling'

const APPS_REFRESH_MS = 10_000
// The busiest containers shown here; the Containers page has them all.
const TOP_CONTAINERS = 5
const RECENT_EVENTS = 5

// OverviewPage is the landing page: the server, every app, and what the
// platform fixed lately, on one screen.
export function OverviewPage() {
  const containers = usePolling(fetchContainers, REFRESH_MS)
  const history = usePolling(
    useCallback((signal: AbortSignal) => fetchHistory('1h', signal), []),
    HISTORY_REFRESH_MS,
  )
  const apps = usePolling(fetchApps, APPS_REFRESH_MS)
  const events = usePolling(fetchEvents, APPS_REFRESH_MS)

  const counts =
    containers.status === 'ok'
      ? (() => {
          const all = containers.data.containers
          const stopped = all.filter((c) => c.state !== 'running').length
          return `${all.length} containers${stopped > 0 ? `, ${stopped} stopped` : ''}`
        })()
      : undefined

  return (
    <>
      <PageHeader title="Overview" subtitle={counts} />
      <div className="stack">
        {containers.status === 'ok' && <StaleBanner state={containers} />}

        <section aria-labelledby="overview-server" className="stack-tight">
          <h2 id="overview-server" className="section-title">
            Server
          </h2>
          <Loaded state={containers} what="the server">
            {(data) => (
              <HostSummary
                host={data.host}
                history={history.status === 'ok' ? history.data.host : null}
                windowLabel="last hour"
              />
            )}
          </Loaded>
        </section>

        <section aria-labelledby="overview-apps" className="stack-tight">
          <div className="section-head">
            <h2 id="overview-apps" className="section-title">
              Apps
            </h2>
            <a href="#/apps">All apps</a>
          </div>
          <Loaded state={apps} what="apps">
            {(list) =>
              list.length === 0 ? (
                <p className="muted">No apps yet.</p>
              ) : (
                <div className="card-grid">
                  {list.map((a) => (
                    <AppCard key={a.id} app={a} />
                  ))}
                </div>
              )
            }
          </Loaded>
        </section>

        <div className="columns">
          <Card title="Recent events" action={<a href="#/apps">All events</a>}>
            <Loaded state={events} what="events">
              {(es, checkedAt) => (
                <EventsList events={es.slice(0, RECENT_EVENTS)} now={checkedAt} emptyText="Nothing has needed fixing." />
              )}
            </Loaded>
          </Card>
          <Card title="Busiest containers" action={<a href="#/containers">All containers</a>}>
            <Loaded state={containers} what="containers">
              {(data) => {
                const top = data.containers
                  .filter((c) => c.state === 'running')
                  .sort((a, b) => (b.memoryBytes ?? 0) - (a.memoryBytes ?? 0))
                  .slice(0, TOP_CONTAINERS)
                return top.length === 0 ? (
                  <p className="muted">Nothing running.</p>
                ) : (
                  <ul className="list">
                    {top.map((c) => (
                      <li key={c.id}>
                        <span className="dot dot-up" aria-hidden="true" />
                        <span className="list-main">{c.name}</span>
                        <span className="muted">
                          {formatBytes(c.memoryBytes)} · {formatCPU(c.cpuPercent)}
                        </span>
                      </li>
                    ))}
                  </ul>
                )
              }}
            </Loaded>
          </Card>
        </div>
      </div>
    </>
  )
}
