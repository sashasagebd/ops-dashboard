import { useCallback, useState, type FormEvent, type ReactNode } from 'react'
import {
  appLogURL,
  createApp,
  createDatabase,
  deleteEnvVar,
  deployCommit,
  deployLatest,
  deploymentLogURL,
  fetchApp,
  fetchApps,
  fetchEvents,
  rollback,
  setAutoDeploy,
  setEnvVar,
  type AppDetail,
  type Deployment,
  type DeploymentStatus,
  type EventKind,
  type PlatformApp,
  type PlatformEvent,
} from './api'
import { formatBytes, formatDuration } from './format'
import { LogViewer } from './LogViewer'
import { appHref } from './route'
import { usePolling, type PollState } from './usePolling'

const APPS_REFRESH_MS = 10_000
// Often enough to follow a deploy through its stages (building, starting,
// healthy), which take seconds to minutes each.
const APP_REFRESH_MS = 3_000

const STATUS_BADGE: Record<DeploymentStatus, string> = {
  queued: 'warn',
  building: 'warn',
  starting: 'warn',
  healthy: 'up',
  failed: 'down',
  superseded: 'neutral',
  rolled_back: 'neutral',
}

// How each kind of reconciler event looks: what was fixed is a warning (the
// app was down, briefly); housekeeping is neutral; what keeps going wrong is
// a fault.
const EVENT_BADGE: Record<EventKind, string> = {
  restarted: 'warn',
  recreated: 'warn',
  crash_looping: 'down',
  removed_orphan: 'neutral',
  routes_reapplied: 'neutral',
  fix_failed: 'down',
  database_created: 'up',
}

const EVENT_LABEL: Record<EventKind, string> = {
  restarted: 'restarted',
  recreated: 'recreated',
  crash_looping: 'crash-looping',
  removed_orphan: 'cleaned up',
  routes_reapplied: 'routes fixed',
  fix_failed: 'fix failed',
  database_created: 'database created',
}

const IN_PROGRESS: ReadonlySet<DeploymentStatus> = new Set(['queued', 'building', 'starting'])

const SHA_PATTERN = /^[0-9a-f]{40}$/

// AppsPage lists the platform's apps and registers new ones.
export function AppsPage() {
  const state = usePolling(fetchApps, APPS_REFRESH_MS)
  const events = usePolling(fetchEvents, APPS_REFRESH_MS)
  return (
    <>
      <h2>Apps</h2>
      <Loaded state={state} what="apps">
        {(apps) => <AppsTable apps={apps} />}
      </Loaded>
      <h2>Recent events</h2>
      <p className="muted">
        What the platform found wrong and put right: apps that stopped, containers that went missing, routes that
        changed.
      </p>
      <Loaded state={events} what="events">
        {(es, checkedAt) => <EventsList events={es} now={checkedAt} emptyText="Nothing has needed fixing." />}
      </Loaded>
      <h2>Register an app</h2>
      <NewAppForm />
    </>
  )
}

function AppsTable({ apps }: { apps: PlatformApp[] }) {
  if (apps.length === 0) return <p>No apps yet.</p>
  return (
    <div className="table-scroll">
      <table>
        <thead>
          <tr>
            <th scope="col">Name</th>
            <th scope="col">Address</th>
            <th scope="col">Live</th>
          </tr>
        </thead>
        <tbody>
          {apps.map((a) => (
            <tr key={a.id}>
              <td>
                <a href={appHref(a.id)}>{a.name}</a>
              </td>
              <td>
                <AppLink hostname={a.hostname} />
              </td>
              <td>
                {a.currentDeployment === null ? (
                  <span className="muted">not deployed</span>
                ) : (
                  <span className="mono">{shortSHA(a.currentDeployment.commitSha)}</span>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function NewAppForm() {
  const [name, setName] = useState('')
  const [repoUrl, setRepoUrl] = useState('')
  const [sourceDir, setSourceDir] = useState('')
  const [port, setPort] = useState('8080')
  const [healthCheckPath, setHealthCheckPath] = useState('/')
  const [action, run] = useAction()

  const onSubmit = (e: FormEvent) => {
    e.preventDefault()
    void run(async () => {
      const app = await createApp({ name, repoUrl, sourceDir, port: Number(port), healthCheckPath })
      window.location.hash = appHref(app.id)
    })
  }

  return (
    <form className="form" onSubmit={onSubmit}>
      <Field label="Name" hint="Lowercase letters, digits and hyphens. Also the app's subdomain.">
        <input value={name} onChange={(e) => setName(e.target.value)} required />
      </Field>
      <Field label="GitHub repository" hint="https://github.com/owner/repo">
        <input type="url" value={repoUrl} onChange={(e) => setRepoUrl(e.target.value)} required />
      </Field>
      <Field label="Source directory" hint="Folder with the Dockerfile; empty for the repo's root.">
        <input value={sourceDir} onChange={(e) => setSourceDir(e.target.value)} placeholder="e.g. examples/hello" />
      </Field>
      <Field label="Port" hint="The port the app listens on inside its container.">
        <input type="number" min={1} max={65535} value={port} onChange={(e) => setPort(e.target.value)} required />
      </Field>
      <Field label="Health check path" hint="Must answer 2xx or 3xx before the app goes live.">
        <input value={healthCheckPath} onChange={(e) => setHealthCheckPath(e.target.value)} required />
      </Field>
      <p className="muted">Each app gets 256 MiB of memory and half a CPU core.</p>
      <div>
        <button type="submit" disabled={action.status === 'busy'}>
          Register
        </button>
      </div>
      <ActionError state={action} />
    </form>
  )
}

// LogChoice is which log the app page shows: a deploy's, or the live
// container's output.
type LogChoice = { kind: 'deployment'; id: number } | { kind: 'app' }

// AppPage is one app: what's live, deploying, logs, past deploys and env
// vars.
export function AppPage({ id }: { id: number }) {
  // Bumped after each change, to show its effect without waiting for the
  // next poll.
  const [refreshKey, setRefreshKey] = useState(0)
  const refresh = () => setRefreshKey((k) => k + 1)
  const fetcher = useCallback((signal: AbortSignal) => fetchApp(id, signal), [id])
  const state = usePolling(fetcher, APP_REFRESH_MS, refreshKey)
  // null: the latest deploy's log.
  const [log, setLog] = useState<LogChoice | null>(null)
  const onDeployed = (d: Deployment) => {
    setLog({ kind: 'deployment', id: d.id }) // watch the new deploy
    refresh()
  }

  return (
    <>
      <p>
        <a href="#/apps">← Apps</a>
      </p>
      <Loaded state={state} what="the app">
        {(app, checkedAt) => (
          <>
            <h2>{app.name}</h2>
            <AppSummary app={app} />
            <h3>Deploy</h3>
            <DeployForm app={app} onDeployed={onDeployed} />
            <AutoDeploy app={app} onDone={refresh} />
            <h3>Logs</h3>
            <Logs app={app} choice={log} onChoose={setLog} />
            <h3>Deployments</h3>
            <DeploymentsTable
              app={app}
              now={checkedAt}
              onRolledBack={onDeployed}
              shownLog={log?.kind === 'deployment' ? log.id : (app.deployments.at(0)?.id ?? null)}
              onShowLog={(id) => setLog({ kind: 'deployment', id })}
            />
            <h3>Events</h3>
            <EventsList events={app.events} now={checkedAt} emptyText="Nothing has needed fixing for this app." />
            <h3>Database</h3>
            <Database app={app} onDone={refresh} />
            <h3>Environment variables</h3>
            <EnvVars app={app} onDone={refresh} />
          </>
        )}
      </Loaded>
    </>
  )
}

function AppSummary({ app }: { app: AppDetail }) {
  const live = app.currentDeployment
  return (
    <dl className="facts">
      <dt>Address</dt>
      <dd>
        <AppLink hostname={app.hostname} />
      </dd>
      <dt>Live</dt>
      <dd>
        {live === null ? (
          <span className="muted">nothing yet</span>
        ) : (
          <span className="mono">{shortSHA(live.commitSha)}</span>
        )}
      </dd>
      <dt>Code</dt>
      <dd>
        <a href={app.repoUrl}>{app.repoUrl.replace(/^https:\/\/github\.com\//, '')}</a>
        {app.sourceDir !== '' && <span className="mono"> /{app.sourceDir}</span>}
      </dd>
      <dt>Runs</dt>
      <dd>
        port {app.port}, health check <span className="mono">{app.healthCheckPath}</span>,{' '}
        {formatBytes(app.memoryLimitBytes)}, {app.cpuLimitMillicores / 1000} CPU
      </dd>
    </dl>
  )
}

function DeployForm({ app, onDeployed }: { app: AppDetail; onDeployed: (d: Deployment) => void }) {
  const [sha, setSHA] = useState('')
  const [action, run] = useAction()
  const deploying = app.deployments.some((d) => IN_PROGRESS.has(d.status))
  const valid = SHA_PATTERN.test(sha)

  const onSubmit = (e: FormEvent) => {
    e.preventDefault()
    void run(async () => {
      const d = await deployCommit(app.id, sha)
      setSHA('')
      onDeployed(d)
    })
  }
  const onLatest = () => {
    void run(async () => onDeployed(await deployLatest(app.id)))
  }

  return (
    <form className="form-inline" onSubmit={onSubmit}>
      <button type="button" onClick={onLatest} disabled={deploying || action.status === 'busy'}>
        Deploy latest from <span className="mono">{app.branch}</span>
      </button>
      <span className="muted">or a commit:</span>
      <label>
        <span className="visually-hidden">Commit SHA</span>
        <input
          className="mono"
          value={sha}
          onChange={(e) => setSHA(e.target.value.trim().toLowerCase())}
          placeholder="full 40-character commit SHA"
          size={44}
          spellCheck={false}
        />
      </label>
      <button type="submit" disabled={!valid || deploying || action.status === 'busy'}>
        Deploy
      </button>
      {deploying && <span className="muted">A deploy is in progress.</span>}
      <ActionError state={action} />
    </form>
  )
}

// AutoDeploy is the push-to-deploy switch, and the pushed commit waiting for
// the deploy in progress, if there is one.
function AutoDeploy({ app, onDone }: { app: AppDetail; onDone: () => void }) {
  const [action, run] = useAction()
  const onChange = (on: boolean) => {
    void run(async () => {
      await setAutoDeploy(app.id, on)
      onDone()
    })
  }
  return (
    <div className="auto-deploy">
      <label>
        <input
          type="checkbox"
          checked={app.autoDeploy}
          disabled={action.status === 'busy'}
          onChange={(e) => onChange(e.target.checked)}
        />{' '}
        Deploy automatically on every push to <span className="mono">{app.branch}</span>
      </label>
      {!app.autoDeploy && rolledBackTo(app) !== null && (
        <p className="muted">
          Turned off by the rollback to #{rolledBackTo(app)}, so that a push doesn't replace it. Turn it back on once
          the fix is pushed.
        </p>
      )}
      {app.nextCommitSha !== null && (
        <p className="muted">
          Pushed while a deploy was running: <span className="mono">{shortSHA(app.nextCommitSha)}</span> deploys
          next.
        </p>
      )}
      <ActionError state={action} />
    </div>
  )
}

// rolledBackTo is the deployment the app's latest deploy rolled back to, or
// null if its latest deploy isn't a rollback.
function rolledBackTo(app: AppDetail): number | null {
  return app.deployments.at(0)?.rollbackOf ?? null
}

// Database is the app's own Postgres: a button to add one, or what it is and
// whether it's running.
function Database({ app, onDone }: { app: AppDetail; onDone: () => void }) {
  const [action, run] = useAction()
  const db = app.database
  if (db === null) {
    const onAdd = () => {
      const question =
        `Add a Postgres database for ${app.name}? It reaches the app as DATABASE_URL from its next deploy, ` +
        `and it can't be removed from the dashboard.`
      if (!window.confirm(question)) return
      void run(async () => {
        await createDatabase(app.id)
        onDone()
      })
    }
    return (
      <div>
        <p className="muted">
          No database. Add one and the platform runs a Postgres for this app alone, keeps it running and keeps its data.
        </p>
        <button type="button" onClick={onAdd} disabled={action.status === 'busy'}>
          Add a database
        </button>
        <ActionError state={action} />
      </div>
    )
  }
  // A live version deployed before the database was added has no
  // DATABASE_URL yet.
  const live = app.currentDeployment
  const needsDeploy = live === null || Date.parse(live.createdAt) < Date.parse(db.createdAt)
  return (
    <>
      <dl className="facts">
        <dt>Postgres 18</dt>
        <dd>
          <span className="mono">{db.name}</span>{' '}
          <span className={`badge badge-${DATABASE_BADGE[db.state] ?? 'warn'}`}>
            {db.state === 'creating' ? 'being created' : db.state}
          </span>
        </dd>
      </dl>
      <p className="muted">
        The app gets it as <span className="mono">DATABASE_URL</span>; the password is never shown.
        {needsDeploy && ' Deploy the app to start using it.'}
      </p>
    </>
  )
}

// How a database's state looks; anything else (stopped, missing) is a
// warning, since the reconciler is bringing it back.
const DATABASE_BADGE: Record<string, string> = {
  running: 'up',
  unknown: 'neutral',
}

function EnvVars({ app, onDone }: { app: AppDetail; onDone: () => void }) {
  const [key, setKey] = useState('')
  const [value, setValue] = useState('')
  const [action, run] = useAction()

  const onSubmit = (e: FormEvent) => {
    e.preventDefault()
    void run(async () => {
      await setEnvVar(app.id, key, value)
      setKey('')
      setValue('')
      onDone()
    })
  }
  const onDelete = (k: string) => {
    // The value can't be read back, so removing it can't be undone.
    if (!window.confirm(`Remove ${k}? Its value can't be recovered.`)) return
    void run(async () => {
      await deleteEnvVar(app.id, k)
      onDone()
    })
  }

  return (
    <>
      <p className="muted">
        Values are stored encrypted and can't be shown again. Changes apply from the next deploy.
      </p>
      {app.envKeys.length > 0 && (
        <ul className="env-list">
          {app.envKeys.map((k) => (
            <li key={k}>
              <span className="mono">{k}</span>
              <button type="button" onClick={() => onDelete(k)} disabled={action.status === 'busy'}>
                Remove
              </button>
            </li>
          ))}
        </ul>
      )}
      <form className="form-inline" onSubmit={onSubmit}>
        <label>
          <span className="visually-hidden">Name</span>
          <input className="mono" value={key} onChange={(e) => setKey(e.target.value)} placeholder="NAME" required />
        </label>
        <label>
          <span className="visually-hidden">Value</span>
          {/* A password field: not echoed on screen, and browsers don't
              keep it in autofill history. */}
          <input
            type="password"
            autoComplete="off"
            value={value}
            onChange={(e) => setValue(e.target.value)}
            placeholder="value"
          />
        </label>
        <button type="submit" disabled={action.status === 'busy'}>
          Set
        </button>
        <ActionError state={action} />
      </form>
    </>
  )
}

// Logs shows one log at a time, chosen with a toggle: a deploy's (the latest
// unless another was picked from the table) or the live app's output.
function Logs({
  app,
  choice,
  onChoose,
}: {
  app: AppDetail
  choice: LogChoice | null
  onChoose: (c: LogChoice) => void
}) {
  const latest = app.deployments.at(0)
  const shown: LogChoice | null = choice ?? (latest === undefined ? null : { kind: 'deployment', id: latest.id })
  if (shown === null) return <p className="muted">Logs appear here once the app has been deployed.</p>
  const deploymentID = shown.kind === 'deployment' ? shown.id : latest?.id
  const live = app.currentDeployment

  return (
    <>
      <div className="toggle" role="group" aria-label="Which log">
        <button
          type="button"
          aria-pressed={shown.kind === 'deployment'}
          disabled={deploymentID === undefined}
          onClick={() => deploymentID !== undefined && onChoose({ kind: 'deployment', id: deploymentID })}
        >
          Deploy #{deploymentID}
        </button>
        <button
          type="button"
          aria-pressed={shown.kind === 'app'}
          disabled={live === null}
          onClick={() => onChoose({ kind: 'app' })}
        >
          App output
        </button>
      </div>
      {shown.kind === 'deployment' ? (
        <LogViewer
          key={shown.id}
          url={deploymentLogURL(app.id, shown.id)}
          label={`Deploy #${shown.id} log`}
          endedText="End of the deploy's log."
          emptyText="No log for this deploy: only an app's last 10 deploys keep theirs."
        />
      ) : live === null ? (
        <p className="muted">Nothing is live, so there's no app output.</p>
      ) : (
        // Keyed by the live deployment, so a new one going live starts a
        // new stream (the old container's has ended).
        <LogViewer
          key={`app-${live.id}`}
          url={appLogURL(app.id)}
          label="App output"
          endedText="Output stopped: the container stopped, or a deploy replaced it."
          emptyText="No output."
          restartable
        />
      )}
    </>
  )
}

function DeploymentsTable({
  app,
  now,
  onRolledBack,
  shownLog,
  onShowLog,
}: {
  app: AppDetail
  now: number
  onRolledBack: (d: Deployment) => void
  shownLog: number | null
  onShowLog: (id: number) => void
}) {
  const [action, run] = useAction()
  const deployments = app.deployments
  if (deployments.length === 0) return <p>No deploys yet.</p>
  const deploying = deployments.some((d) => IN_PROGRESS.has(d.status))

  const onRollback = (d: Deployment) => {
    const question =
      `Roll back to #${d.id} (${shortSHA(d.commitSha)})? Its image runs again, with today's env vars, ` +
      `and auto-deploy is turned off so that a push doesn't replace it.`
    if (!window.confirm(question)) return
    void run(async () => onRolledBack(await rollback(app.id, d.id)))
  }

  return (
    <div className="table-scroll">
      <table>
        <thead>
          <tr>
            <th scope="col">#</th>
            <th scope="col">Commit</th>
            <th scope="col">Status</th>
            <th scope="col">Started</th>
            <th scope="col">
              <span className="visually-hidden">Log</span>
            </th>
            <th scope="col">
              <span className="visually-hidden">Rollback</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {deployments.map((d) => (
            <tr key={d.id}>
              <td>{d.id}</td>
              <td>
                <span className="mono" title={d.commitSha}>
                  {shortSHA(d.commitSha)}
                </span>
                {d.rollbackOf !== null && <div className="muted">rollback of #{d.rollbackOf}</div>}
              </td>
              <td>
                <span className={`badge badge-${STATUS_BADGE[d.status]}`}>{d.status.replace('_', ' ')}</span>
                {d.error !== null && <div className="deploy-error">{d.error}</div>}
              </td>
              <td title={d.createdAt}>{formatDuration(now - Date.parse(d.createdAt))} ago</td>
              <td>
                <button
                  type="button"
                  aria-pressed={shownLog === d.id}
                  aria-label={`Show deploy #${d.id}'s log`}
                  onClick={() => onShowLog(d.id)}
                >
                  Log
                </button>
              </td>
              <td>
                {d.rollbackable && (
                  <button
                    type="button"
                    aria-label={`Roll back to deploy #${d.id}`}
                    disabled={deploying || action.status === 'busy'}
                    onClick={() => onRollback(d)}
                  >
                    Roll back
                  </button>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      <ActionError state={action} />
    </div>
  )
}

// EventsList is the reconciler's events, newest first; a crash's last lines
// of output fold out under it.
function EventsList({ events, now, emptyText }: { events: PlatformEvent[]; now: number; emptyText: string }) {
  if (events.length === 0) return <p className="muted">{emptyText}</p>
  return (
    <ul className="events">
      {events.map((e) => (
        <li key={e.id}>
          <span className={`badge badge-${EVENT_BADGE[e.kind]}`}>{EVENT_LABEL[e.kind]}</span> {e.message}{' '}
          <span className="muted" title={e.at}>
            {formatDuration(now - Date.parse(e.at))} ago
          </span>
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

// Loaded shows a polled resource's loading and error states, and children
// once it has loaded. If a refresh fails later, the last data stays up
// under a banner.
function Loaded<T>({
  state,
  what,
  children,
}: {
  state: PollState<T>
  what: string
  children: (data: T, checkedAt: number) => ReactNode
}) {
  switch (state.status) {
    case 'loading':
      return <p>Loading…</p>
    case 'error':
      return <p role="alert">Couldn't load {what}: {state.message}</p>
    case 'ok':
      return (
        <>
          {state.refreshError !== null && (
            <p role="status" className="banner">
              Couldn't refresh: {state.refreshError}. Showing the last data loaded.
            </p>
          )}
          {children(state.data, state.checkedAt)}
        </>
      )
  }
}

function Field({ label, hint, children }: { label: string; hint: string; children: ReactNode }) {
  return (
    <label className="field">
      <span>{label}</span>
      {children}
      <span className="muted field-hint">{hint}</span>
    </label>
  )
}

// Apps are served over plain HTTP: on the tailnet for now, and behind
// Cloudflare (which adds HTTPS) once they're public.
function AppLink({ hostname }: { hostname: string }) {
  return (
    <a href={`http://${hostname}/`} target="_blank" rel="noreferrer">
      {hostname}
    </a>
  )
}

function shortSHA(sha: string): string {
  return sha.slice(0, 7)
}

// ActionState is a button's request: idle, in flight, or failed with the
// platform's message.
type ActionState = { status: 'idle' } | { status: 'busy' } | { status: 'error'; message: string }

function useAction() {
  const [state, setState] = useState<ActionState>({ status: 'idle' })
  const run = async (fn: () => Promise<void>) => {
    setState({ status: 'busy' })
    try {
      await fn()
      setState({ status: 'idle' })
    } catch (err: unknown) {
      setState({ status: 'error', message: err instanceof Error ? err.message : String(err) })
    }
  }
  return [state, run] as const
}

function ActionError({ state }: { state: ActionState }) {
  if (state.status !== 'error') return null
  return (
    <p role="alert" className="action-error">
      {state.message}
    </p>
  )
}
