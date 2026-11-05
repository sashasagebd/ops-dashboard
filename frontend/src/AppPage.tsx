import { useCallback, useState, type FormEvent, type ReactNode } from 'react'
import {
  appLogURL,
  createDatabase,
  deleteEnvVar,
  deployCommit,
  deployLatest,
  deploymentLogURL,
  fetchApp,
  fetchAppMetrics,
  fetchHistory,
  rollback,
  setAutoDeploy,
  setEnvVar,
  setPublicName,
  startBackup,
  type AppDetail,
  type Deployment,
  type Series,
} from './api'
import { BACKUP_BADGE, DATABASE_BADGE, IN_PROGRESS, shortSHA, STATUS_BADGE } from './appFormat'
import { AppBadges, AppLink, EventsList, PublicLink } from './appParts'
import { formatBytes, formatCPU, formatDuration, formatFileSize } from './format'
import { LogViewer } from './LogViewer'
import { appHref, type AppTab } from './route'
import { seriesMax } from './series'
import { Sparkline } from './Sparkline'
import { ActionError, Card, Loaded, PageHeader, TabPanel, Tabs } from './ui'
import { useAction } from './useAction'
import { usePolling } from './usePolling'

// Often enough to follow a deploy through its stages (building, starting,
// healthy), which take seconds to minutes each.
const APP_REFRESH_MS = 3_000
// Metrics change once a minute (a step), so there's no point asking often.
const METRICS_REFRESH_MS = 30_000

const SHA_PATTERN = /^[0-9a-f]{40}$/
// One part of a hostname, as the platform checks a public name.
const PUBLIC_NAME_PATTERN = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/

const TABS: readonly { id: AppTab; label: string }[] = [
  { id: 'overview', label: 'Overview' },
  { id: 'deployments', label: 'Deployments' },
  { id: 'logs', label: 'Logs' },
  { id: 'database', label: 'Database' },
  { id: 'settings', label: 'Settings' },
]

// AppPage is one app: a header that stays on every tab (status, addresses,
// Deploy latest), then the tab chosen in the URL.
export function AppPage({ id, tab }: { id: number; tab: AppTab }) {
  // Bumped after each change, to show its effect without waiting for the
  // next poll.
  const [refreshKey, setRefreshKey] = useState(0)
  const refresh = () => setRefreshKey((k) => k + 1)
  const fetcher = useCallback((signal: AbortSignal) => fetchApp(id, signal), [id])
  const state = usePolling(fetcher, APP_REFRESH_MS, refreshKey)
  // The deploy whose log the Deployments tab shows; null: the latest. Kept
  // here, not in the tab, so it survives switching tabs.
  const [shownLog, setShownLog] = useState<number | null>(null)
  const showLog = (deploymentID: number) => {
    setShownLog(deploymentID)
    window.location.hash = appHref(id, 'deployments')
  }
  // A deploy or rollback just started: watch its log.
  const onDeployed = (d: Deployment) => {
    showLog(d.id)
    refresh()
  }

  return (
    <Loaded state={state} what="the app">
      {(app, checkedAt) => (
        <>
          <AppHeader app={app} now={checkedAt} onDeployed={onDeployed} />
          <Tabs label={app.name} tabs={TABS} current={tab} hrefFor={(t) => appHref(id, t)} />
          <TabPanel current={tab}>
            {tab === 'overview' && <OverviewTab app={app} now={checkedAt} onShowLog={showLog} />}
            {tab === 'deployments' && (
              <DeploymentsTab
                app={app}
                now={checkedAt}
                onDeployed={onDeployed}
                shownLog={shownLog ?? app.deployments.at(0)?.id ?? null}
                onShowLog={setShownLog}
              />
            )}
            {tab === 'logs' && <LogsTab app={app} />}
            {tab === 'database' && <DatabaseTab app={app} now={checkedAt} onDone={refresh} />}
            {tab === 'settings' && <SettingsTab app={app} onDone={refresh} />}
          </TabPanel>
        </>
      )}
    </Loaded>
  )
}

function AppHeader({ app, now, onDeployed }: { app: AppDetail; now: number; onDeployed: (d: Deployment) => void }) {
  const [action, run] = useAction()
  const live = app.currentDeployment
  const deploying = app.deployments.some((d) => IN_PROGRESS.has(d.status))
  const rolledBack = rolledBackTo(app)
  const onLatest = () => {
    void run(async () => onDeployed(await deployLatest(app.id)))
  }
  return (
    <PageHeader
      crumbs={
        <>
          <a href="#/apps">Apps</a> / {app.name}
        </>
      }
      title={app.name}
      badges={
        <>
          <AppBadges app={app} />
          {deploying && <span className="badge badge-warn">Deploying</span>}
        </>
      }
      subtitle={
        <ul className="meta">
          {live !== null && (
            <li>
              <span className="mono">{shortSHA(live.commitSha)}</span> live since{' '}
              {formatDuration(now - Date.parse(live.finishedAt ?? live.createdAt))} ago
            </li>
          )}
          {app.publicHostname !== null && (
            <li>
              <PublicLink hostname={app.publicHostname} />
            </li>
          )}
          <li>
            <AppLink hostname={app.hostname} /> <span className="muted">(tailnet)</span>
          </li>
          <li>
            {app.autoDeploy ? (
              <>
                Auto-deploy on <span className="mono">{app.branch}</span>
              </>
            ) : rolledBack !== null ? (
              `Auto-deploy off: rolled back to #${rolledBack}`
            ) : (
              'Auto-deploy off'
            )}
          </li>
          {app.nextCommitSha !== null && (
            <li>
              <span className="mono">{shortSHA(app.nextCommitSha)}</span> deploys next
            </li>
          )}
        </ul>
      }
      actions={
        <>
          <button type="button" className="primary" onClick={onLatest} disabled={deploying || action.status === 'busy'}>
            Deploy latest
          </button>
          <ActionError state={action} />
        </>
      }
    />
  )
}

// rolledBackTo is the deployment the app's latest deploy rolled back to, or
// null if its latest deploy isn't a rollback.
function rolledBackTo(app: AppDetail): number | null {
  return app.deployments.at(0)?.rollbackOf ?? null
}

// --- Overview ---

function OverviewTab({ app, now, onShowLog }: { app: AppDetail; now: number; onShowLog: (id: number) => void }) {
  const live = app.currentDeployment
  return (
    <div className="stack">
      <Metrics app={app} />
      <div className="columns">
        <Card title="Live version">
          {live === null ? (
            <p className="muted">Nothing is live yet. Deploy latest to start.</p>
          ) : (
            <>
              <dl className="facts">
                <dt>Deployment</dt>
                <dd>
                  #{live.id} · <span className="mono">{shortSHA(live.commitSha)}</span>{' '}
                  {live.rollbackOf !== null && <span className="muted">(rollback of #{live.rollbackOf})</span>}
                </dd>
                <dt>Went live</dt>
                <dd>{formatDuration(now - Date.parse(live.finishedAt ?? live.createdAt))} ago</dd>
                {live.imageTag !== null && (
                  <>
                    <dt>Image</dt>
                    <dd className="mono">{live.imageTag}</dd>
                  </>
                )}
              </dl>
              <div className="card-actions">
                <button type="button" onClick={() => onShowLog(live.id)}>
                  View its build log
                </button>
              </div>
            </>
          )}
        </Card>
        <Card title="Events">
          <EventsList events={app.events} now={now} emptyText="Nothing has needed fixing for this app." />
        </Card>
        <Card title="About">
          <dl className="facts">
            <dt>Code</dt>
            <dd>
              <a href={app.repoUrl}>{app.repoUrl.replace(/^https:\/\/github\.com\//, '')}</a>
              {app.sourceDir !== '' && <span className="mono"> /{app.sourceDir}</span>}
            </dd>
            <dt>Runs</dt>
            <dd>
              port {app.port}, health check <span className="mono">{app.healthCheckPath}</span>
            </dd>
            <dt>Limits</dt>
            <dd>
              {formatBytes(app.memoryLimitBytes)}, {app.cpuLimitMillicores / 1000} CPU
            </dd>
            <dt>Database</dt>
            <dd>
              {app.database === null ? (
                <span className="muted">none</span>
              ) : (
                <a href={appHref(app.id, 'database')}>{app.database.name}</a>
              )}
            </dd>
          </dl>
        </Card>
      </div>
    </div>
  )
}

// Metrics is the app's last hour: CPU and memory from the dashboard's own
// history (summed over its containers, which change name each deploy), and
// requests from the platform (counted by Caddy).
function Metrics({ app }: { app: AppDetail }) {
  const history = usePolling(useCallback((signal: AbortSignal) => fetchHistory('1h', signal), []), METRICS_REFRESH_MS)
  const requests = usePolling(
    useCallback((signal: AbortSignal) => fetchAppMetrics(app.id, '1h', signal), [app.id]),
    METRICS_REFRESH_MS,
  )
  const usage = history.status === 'ok' ? history.data.apps[app.name] : undefined
  const req = requests.status === 'ok' && requests.data.requests.some((v) => v !== null) ? requests.data : undefined
  const p95 = req ? last(req.p95Ms) : null

  return (
    <div className="stat-grid">
      <Stat label="CPU" value={usage ? formatCPU(last(usage.cpuPercent)) : null} detail="of the server, now">
        {usage && (
          <Sparkline
            className="stat-trend"
            values={usage.cpuPercent}
            peaks={usage.cpuPercentMax}
            max={Math.max(1, seriesMax(usage.cpuPercentMax))}
            label={`${app.name} CPU, last hour`}
            format={formatCPU}
          />
        )}
      </Stat>
      <Stat
        label="Memory"
        value={usage ? formatBytes(last(usage.memoryBytes)) : null}
        detail={`of ${formatBytes(app.memoryLimitBytes)}`}
      >
        {usage && (
          <Sparkline
            className="stat-trend"
            values={usage.memoryBytes}
            max={app.memoryLimitBytes}
            label={`${app.name} memory, last hour`}
            format={formatBytes}
          />
        )}
      </Stat>
      <Stat
        label="Requests (1h)"
        value={req ? String(sum(req.requests)) : null}
        detail={req ? `${sum(req.errors)} 5xx` : 'counted once a minute'}
      >
        {req && (
          <Sparkline
            className="stat-trend"
            values={req.requests}
            max={Math.max(1, seriesMax(req.requests))}
            label={`${app.name} requests a minute, last hour`}
            format={(v) => `${Math.round(v)}/min`}
          />
        )}
      </Stat>
      <Stat
        label="Response time (p95)"
        value={p95 === null ? null : `${p95} ms`}
        detail={req ? 'tailnet and public' : 'counted once a minute'}
      >
        {req && (
          <Sparkline
            className="stat-trend"
            values={req.p95Ms}
            max={Math.max(1, seriesMax(req.p95Ms))}
            label={`${app.name} p95 response time, last hour`}
            format={(v) => `${v} ms`}
          />
        )}
      </Stat>
    </div>
  )
}

function Stat({
  label,
  value,
  detail,
  children,
}: {
  label: string
  value: string | null // null: no data yet
  detail: string
  children?: ReactNode
}) {
  return (
    <section className="card stat" aria-label={label}>
      <h2 className="stat-label">{label}</h2>
      {value === null ? <p className="stat-value muted">no data yet</p> : <p className="stat-value">{value}</p>}
      <p className="stat-detail">{detail}</p>
      {children}
    </section>
  )
}

// last is a series' newest value that isn't a gap, or null.
function last(values: Series): number | null {
  for (let i = values.length - 1; i >= 0; i--) {
    const v = values[i]
    if (v !== null) return v
  }
  return null
}

function sum(values: Series): number {
  return values.reduce<number>((total, v) => total + (v ?? 0), 0)
}

// --- Deployments ---

function DeploymentsTab({
  app,
  now,
  onDeployed,
  shownLog,
  onShowLog,
}: {
  app: AppDetail
  now: number
  onDeployed: (d: Deployment) => void
  shownLog: number | null
  onShowLog: (id: number) => void
}) {
  return (
    <div className="stack">
      <Card title="Deploy a commit">
        <DeployForm app={app} onDeployed={onDeployed} />
      </Card>
      <Card title="Deployments">
        <DeploymentsTable app={app} now={now} onRolledBack={onDeployed} shownLog={shownLog} onShowLog={onShowLog} />
      </Card>
      {shownLog !== null && (
        <Card title={`Build log · #${shownLog}`}>
          <LogViewer
            key={shownLog}
            url={deploymentLogURL(app.id, shownLog)}
            label={`Deploy #${shownLog} log`}
            endedText="End of the deploy's log."
            emptyText="No log for this deploy: only an app's last 10 deploys keep theirs."
          />
        </Card>
      )}
    </div>
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

  return (
    <form className="form-inline" onSubmit={onSubmit}>
      <label className="grow">
        <span className="visually-hidden">Commit SHA</span>
        <input
          className="mono"
          value={sha}
          onChange={(e) => setSHA(e.target.value.trim().toLowerCase())}
          placeholder="full 40-character commit SHA"
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
  if (deployments.length === 0) return <p className="muted">No deploys yet.</p>
  const deploying = deployments.some((d) => IN_PROGRESS.has(d.status))

  const onRollback = (d: Deployment) => {
    const question =
      `Roll back to #${d.id} (${shortSHA(d.commitSha)})? Its image runs again, with today's env vars, ` +
      `and auto-deploy is turned off so that a push doesn't replace it.`
    if (!window.confirm(question)) return
    void run(async () => onRolledBack(await rollback(app.id, d.id)))
  }

  return (
    <>
      <div className="table-scroll">
        <table>
          <thead>
            <tr>
              <th scope="col">#</th>
              <th scope="col">Commit</th>
              <th scope="col">Status</th>
              <th scope="col">Started</th>
              <th scope="col">
                <span className="visually-hidden">Actions</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {deployments.map((d) => (
              <tr key={d.id} className={shownLog === d.id ? 'selected' : undefined}>
                <td className="strong">{d.id}</td>
                <td>
                  <span className="mono" title={d.commitSha}>
                    {shortSHA(d.commitSha)}
                  </span>
                  {d.rollbackOf !== null && <div className="muted small">rollback of #{d.rollbackOf}</div>}
                </td>
                <td>
                  <span className={`badge badge-${STATUS_BADGE[d.status]}`}>{d.status.replace('_', ' ')}</span>
                  {d.error !== null && <div className="error-text small">{d.error}</div>}
                </td>
                <td title={d.createdAt}>{formatDuration(now - Date.parse(d.createdAt))} ago</td>
                <td>
                  <div className="row-actions">
                    <button
                      type="button"
                      className="small"
                      aria-pressed={shownLog === d.id}
                      aria-label={`Show deploy #${d.id}'s log`}
                      onClick={() => onShowLog(d.id)}
                    >
                      Log
                    </button>
                    {d.rollbackable && (
                      <button
                        type="button"
                        className="small"
                        aria-label={`Roll back to deploy #${d.id}`}
                        disabled={deploying || action.status === 'busy'}
                        onClick={() => onRollback(d)}
                      >
                        Roll back
                      </button>
                    )}
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <ActionError state={action} />
    </>
  )
}

// --- Logs ---

function LogsTab({ app }: { app: AppDetail }) {
  const live = app.currentDeployment
  return (
    <Card title="Live output">
      {live === null ? (
        <p className="muted">Nothing is live, so there's no app output.</p>
      ) : (
        // Keyed by the live deployment, so a new one going live starts a new
        // stream (the old container's has ended).
        <LogViewer
          key={`app-${live.id}`}
          url={appLogURL(app.id)}
          label="App output"
          endedText="Output stopped: the container stopped, or a deploy replaced it."
          emptyText="No output."
          restartable
        />
      )}
    </Card>
  )
}

// --- Database ---

function DatabaseTab({ app, now, onDone }: { app: AppDetail; now: number; onDone: () => void }) {
  return (
    <div className="stack">
      <Card title="Database">
        <Database app={app} onDone={onDone} />
      </Card>
      {app.database !== null && app.database.state !== 'creating' && (
        <Card title="Backups">
          <Backups app={app} now={now} onDone={onDone} />
        </Card>
      )}
    </div>
  )
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
      <>
        <p className="muted card-intro">
          No database. Add one and the platform runs a Postgres for this app alone, keeps it running and keeps its data.
        </p>
        <button type="button" className="primary" onClick={onAdd} disabled={action.status === 'busy'}>
          Add a database
        </button>
        <ActionError state={action} />
      </>
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
      <p className="muted card-note">
        The app gets it as <span className="mono">DATABASE_URL</span>; the password is never shown.
        {needsDeploy && ' Deploy the app to start using it.'}
      </p>
    </>
  )
}

// Backups is the app's database backups, newest first, and a button to make
// one now.
function Backups({ app, now, onDone }: { app: AppDetail; now: number; onDone: () => void }) {
  const [action, run] = useAction()
  const busy = app.backups.some((b) => b.status === 'queued' || b.status === 'running')
  const onBackUp = () => {
    void run(async () => {
      await startBackup(app.id)
      onDone()
    })
  }
  return (
    <>
      <p className="muted card-intro">
        Daily, the newest 7 kept, in <span className="mono">~/backups/{app.name}/</span> on the server. The README says
        how to restore one.
      </p>
      <div className="form-inline">
        <button type="button" onClick={onBackUp} disabled={busy || action.status === 'busy'}>
          Back up now
        </button>
        {busy && <span className="muted">A backup is running.</span>}
        <ActionError state={action} />
      </div>
      {app.backups.length === 0 ? (
        <p className="muted">No backups yet.</p>
      ) : (
        <div className="table-scroll">
          <table>
            <thead>
              <tr>
                <th scope="col">When</th>
                <th scope="col">Status</th>
                <th scope="col">File</th>
                <th scope="col">Size</th>
              </tr>
            </thead>
            <tbody>
              {app.backups.map((b) => (
                <tr key={b.id}>
                  <td title={b.createdAt}>{formatDuration(now - Date.parse(b.createdAt))} ago</td>
                  <td>
                    <span className={`badge badge-${BACKUP_BADGE[b.status]}`}>{b.status}</span>
                    {b.error !== null && <div className="error-text small">{b.error}</div>}
                  </td>
                  <td className="mono">{b.file ?? ''}</td>
                  <td>{b.sizeBytes === null ? '' : formatFileSize(b.sizeBytes)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  )
}

// --- Settings ---

function SettingsTab({ app, onDone }: { app: AppDetail; onDone: () => void }) {
  return (
    <div className="stack">
      <Card title="Public access">
        <PublicAccess app={app} onDone={onDone} />
      </Card>
      <Card title="Push to deploy">
        <AutoDeploy app={app} onDone={onDone} />
      </Card>
      <Card title="Environment variables">
        <EnvVars app={app} onDone={onDone} />
      </Card>
    </div>
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
    <>
      <label className="check">
        <input
          type="checkbox"
          checked={app.autoDeploy}
          disabled={action.status === 'busy'}
          onChange={(e) => onChange(e.target.checked)}
        />
        <span>
          Deploy automatically on every push to <span className="mono">{app.branch}</span>
        </span>
      </label>
      {!app.autoDeploy && rolledBackTo(app) !== null && (
        <p className="muted card-note">
          Turned off by the rollback to #{rolledBackTo(app)}, so that a push doesn't replace it. Turn it back on once
          the fix is pushed.
        </p>
      )}
      <ActionError state={action} />
    </>
  )
}

// PublicAccess makes the app public on the internet, at a name of its own
// under the server's public domain, or tailnet-only again.
function PublicAccess({ app, onDone }: { app: AppDetail; onDone: () => void }) {
  const [name, setName] = useState('')
  const [action, run] = useAction()
  const domain = app.publicDomain
  const save = (publicName: string) => {
    void run(async () => {
      await setPublicName(app.id, publicName)
      setName('')
      onDone()
    })
  }
  const onSubmit = (e: FormEvent) => {
    e.preventDefault()
    if (!window.confirm(`Make ${app.name} public? Anyone on the internet can reach it at https://${name}.${domain}/.`)) {
      return
    }
    save(name)
  }
  const onPrivate = () => {
    if (!window.confirm(`Make ${app.name} tailnet-only? https://${app.publicHostname}/ stops working at once.`)) return
    save('')
  }

  if (domain === null) {
    return (
      <p className="muted">
        Public hostnames aren't set up on this server (<span className="mono">PUBLIC_DOMAIN</span>), so apps are
        tailnet-only.
      </p>
    )
  }
  return (
    <>
      {app.publicHostname === null ? (
        <p className="muted card-intro">
          No, tailnet only. Give it a public name to serve it on the internet, through Cloudflare.
        </p>
      ) : (
        <div className="form-inline card-intro">
          <span>
            Public at <PublicLink hostname={app.publicHostname} />
          </span>
          <button type="button" onClick={onPrivate} disabled={action.status === 'busy'}>
            Make tailnet-only
          </button>
        </div>
      )}
      <form className="form-inline" onSubmit={onSubmit}>
        <span className="suffix-input">
          <label>
            <span className="visually-hidden">Public name</span>
            <input
              className="mono"
              value={name}
              onChange={(e) => setName(e.target.value.trim().toLowerCase())}
              placeholder={app.publicName ?? 'e.g. api'}
              pattern={PUBLIC_NAME_PATTERN.source}
              title="Lowercase letters, digits and inner hyphens"
              spellCheck={false}
            />
          </label>
          <span className="mono muted">.{domain}</span>
        </span>
        <button type="submit" className="primary" disabled={!PUBLIC_NAME_PATTERN.test(name) || action.status === 'busy'}>
          {app.publicHostname === null ? 'Make public' : 'Change'}
        </button>
        <ActionError state={action} />
      </form>
    </>
  )
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
      <p className="muted card-intro">
        Values are stored encrypted and can't be shown again. Changes apply from the next deploy.
      </p>
      {app.envKeys.length > 0 && (
        <ul className="list env-list">
          {app.envKeys.map((k) => (
            <li key={k}>
              <span className="mono list-main">{k}</span>
              <button type="button" className="small" onClick={() => onDelete(k)} disabled={action.status === 'busy'}>
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
