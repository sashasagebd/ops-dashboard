// Pieces about apps shared by the Overview, Apps and app pages.
import type { PlatformApp, PlatformEvent } from './api'
import { appState, EVENT_BADGE, EVENT_LABEL, shortSHA } from './appFormat'
import { formatDuration } from './format'
import { appHref } from './route'

// Apps are served over plain HTTP on the tailnet; their public addresses
// (PublicLink) are HTTPS, which Cloudflare adds.
export function AppLink({ hostname }: { hostname: string }) {
  return (
    <a href={`http://${hostname}/`} target="_blank" rel="noreferrer">
      {hostname}
    </a>
  )
}

export function PublicLink({ hostname }: { hostname: string }) {
  return (
    <a href={`https://${hostname}/`} target="_blank" rel="noreferrer">
      {hostname}
    </a>
  )
}

export function AppBadges({ app }: { app: PlatformApp }) {
  const s = appState(app)
  return (
    <>
      <span className={`badge badge-${s.badge}`}>{s.label}</span>
      {app.publicHostname !== null && <span className="badge badge-info">Public</span>}
    </>
  )
}

// AppCard is an app at a glance: status, address, live commit and traffic.
// The name links to its page; the address opens the app itself.
export function AppCard({ app }: { app: PlatformApp }) {
  const live = app.currentDeployment
  return (
    <article className="card app-card" aria-label={app.name}>
      <div className="app-card-top">
        <a className="app-card-name" href={appHref(app.id)}>
          {app.name}
        </a>
        <AppBadges app={app} />
      </div>
      <div className="app-card-links">
        {app.publicHostname !== null && <PublicLink hostname={app.publicHostname} />}
        <AppLink hostname={app.hostname} />
      </div>
      <dl className="app-card-facts">
        <div>
          <dt>Commit</dt>
          <dd>{live === null ? <span className="muted">not deployed</span> : <span className="mono">{shortSHA(live.commitSha)}</span>}</dd>
        </div>
        <div>
          <dt>Requests (1h)</dt>
          <dd>{app.requestsLastHour === null ? <span className="muted">—</span> : app.requestsLastHour}</dd>
        </div>
        <div>
          <dt>Auto-deploy</dt>
          <dd>{app.autoDeploy ? 'on' : 'off'}</dd>
        </div>
      </dl>
    </article>
  )
}

// EventsList is the reconciler's events, newest first; a crash's last lines
// of output fold out under it.
export function EventsList({ events, now, emptyText }: { events: PlatformEvent[]; now: number; emptyText: string }) {
  if (events.length === 0) return <p className="muted">{emptyText}</p>
  return (
    <ul className="events">
      {events.map((e) => (
        <li key={e.id}>
          <div className="event-line">
            <span className={`badge badge-${EVENT_BADGE[e.kind]}`}>{EVENT_LABEL[e.kind]}</span>
            <span className="event-message">{e.message}</span>
            <span className="event-time" title={e.at}>
              {formatDuration(now - Date.parse(e.at))} ago
            </span>
          </div>
          {e.output.length > 0 && (
            <details>
              <summary>Its last {e.output.length} lines of output</summary>
              <pre className="event-output">{e.output.join('\n')}</pre>
            </details>
          )}
        </li>
      ))}
    </ul>
  )
}
