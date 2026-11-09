import { useRef, useState, type FormEvent } from 'react'
import { createApp, fetchApps, fetchEvents } from './api'
import { AppCard, EventsList } from './appParts'
import { clearFlash, readFlash, type Flash } from './flash'
import { appHref } from './route'
import { ActionError, Card, Field, Loaded, PageHeader } from './ui'
import { useAction } from './useAction'
import { usePolling } from './usePolling'

const APPS_REFRESH_MS = 10_000

// AppsPage lists the platform's apps and registers new ones.
export function AppsPage() {
  const state = usePolling(fetchApps, APPS_REFRESH_MS)
  const events = usePolling(fetchEvents, APPS_REFRESH_MS)
  const form = useRef<HTMLDivElement>(null)
  const toForm = () => {
    form.current?.scrollIntoView({ behavior: 'smooth', block: 'start' })
    form.current?.querySelector('input')?.focus({ preventScroll: true })
  }

  return (
    <>
      <PageHeader
        title="Apps"
        subtitle="Deployed from GitHub, each at its own address."
        actions={
          <button type="button" className="primary" onClick={toForm}>
            New app
          </button>
        }
      />
      <div className="stack">
        <FlashNotice />
        <Loaded state={state} what="apps">
          {(apps) =>
            apps.length === 0 ? (
              <p className="muted">No apps yet.</p>
            ) : (
              <div className="card-grid">
                {apps.map((a) => (
                  <AppCard key={a.id} app={a} />
                ))}
              </div>
            )
          }
        </Loaded>
        <Card title="Recent events">
          <p className="muted card-intro">
            What the platform found wrong and put right: apps that stopped, containers that went missing, routes that
            changed.
          </p>
          <Loaded state={events} what="events">
            {(es, checkedAt) => <EventsList events={es} now={checkedAt} emptyText="Nothing has needed fixing." />}
          </Loaded>
        </Card>
        <div ref={form}>
          <Card title="Register an app">
            <NewAppForm />
          </Card>
        </div>
      </div>
    </>
  )
}

// FlashNotice shows what a deletion left behind, until it's dismissed.
function FlashNotice() {
  const [flash, setFlashState] = useState<Flash | null>(readFlash)
  if (flash === null) return null
  const dismiss = () => {
    clearFlash()
    setFlashState(null)
  }
  return (
    <div className="notice" role="status">
      <div className="notice-head">
        <strong>{flash.title}</strong>
        <button type="button" className="small" onClick={dismiss}>
          Dismiss
        </button>
      </div>
      {flash.lines.map((l) => (
        <p key={l}>{l}</p>
      ))}
      {flash.commands.length > 0 && <pre className="command">{flash.commands.join('\n')}</pre>}
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
      <div className="form-row">
        <Field label="Port" hint="Inside its container.">
          <input type="number" min={1} max={65535} value={port} onChange={(e) => setPort(e.target.value)} required />
        </Field>
        <Field label="Health check path" hint="Must answer 2xx or 3xx to go live.">
          <input value={healthCheckPath} onChange={(e) => setHealthCheckPath(e.target.value)} required />
        </Field>
      </div>
      <p className="muted">Each app gets 256 MiB of memory and half a CPU core.</p>
      <div>
        <button type="submit" className="primary" disabled={action.status === 'busy'}>
          Register
        </button>
      </div>
      <ActionError state={action} />
    </form>
  )
}
