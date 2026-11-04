import { fireEvent, render, screen, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { AppDetail, Deployment, PlatformApp, PlatformEvent } from './api'
import App from './App'
import { FakeEventSource } from './test/fakeEventSource'

const SHA = '0123456789abcdef0123456789abcdef01234567'

function deployment(overrides: Partial<Deployment> = {}): Deployment {
  return {
    id: 7,
    appId: 1,
    commitSha: SHA,
    status: 'healthy',
    imageTag: `ops-platform/hello:${SHA}`,
    error: null,
    rollbackOf: null,
    rollbackable: false,
    createdAt: new Date(Date.now() - 5 * 60_000).toISOString(),
    startedAt: null,
    finishedAt: null,
    ...overrides,
  }
}

function app(overrides: Partial<PlatformApp> = {}): PlatformApp {
  return {
    id: 1,
    name: 'hello',
    repoUrl: 'https://github.com/o/repo',
    branch: 'main',
    hostname: 'hello.apps.test',
    port: 8080,
    healthCheckPath: '/healthz',
    memoryLimitBytes: 256 * 1024 * 1024,
    cpuLimitMillicores: 500,
    sourceDir: 'examples/hello',
    autoDeploy: true,
    nextCommitSha: null,
    createdAt: '2026-11-01T00:00:00Z',
    currentDeployment: null,
    ...overrides,
  }
}

function detail(overrides: Partial<AppDetail> = {}): AppDetail {
  return { ...app(), envKeys: [], deployments: [], events: [], database: null, ...overrides }
}

function event(overrides: Partial<PlatformEvent> = {}): PlatformEvent {
  return {
    id: 1,
    at: new Date(Date.now() - 2 * 60_000).toISOString(),
    appId: 1,
    kind: 'restarted',
    message: 'Restarted hello: its container app-hello-7 had exited with code 137',
    output: [],
    ...overrides,
  }
}

type Handler = (init: RequestInit | undefined) => [number, unknown]

// mockFetch answers "METHOD /path" with a handler; anything else is a 404,
// apart from the Apps page's events, which are none unless a test says.
// It returns the mock, whose calls show what was sent.
function mockFetch(routes: Record<string, Handler>) {
  const all: Record<string, Handler> = { 'GET /api/platform/events': () => [200, []], ...routes }
  const fetchMock = vi.fn((url: string, init?: RequestInit) => {
    const handler = all[`${init?.method ?? 'GET'} ${url}`]
    const [status, body] = handler ? handler(init) : [404, { error: 'not found' }]
    return Promise.resolve({
      ok: status < 400,
      status,
      statusText: '',
      json: () => Promise.resolve(body),
    } as Response)
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

function sent(fetchMock: ReturnType<typeof mockFetch>, method: string, url: string) {
  return fetchMock.mock.calls.filter(([u, init]) => u === url && (init?.method ?? 'GET') === method)
}

beforeEach(() => {
  window.location.hash = ''
  FakeEventSource.instances = []
  vi.stubGlobal('EventSource', FakeEventSource)
})

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe('Apps page', () => {
  it('lists apps with their address and live commit', async () => {
    mockFetch({
      'GET /api/platform/apps': () => [
        200,
        [app(), app({ id: 2, name: 'api', hostname: 'api.apps.test', currentDeployment: deployment() })],
      ],
    })
    window.location.hash = '#/apps'
    render(<App />)

    const rows = await screen.findAllByRole('row')
    expect(within(rows[1]).getByRole('link', { name: 'hello' })).toHaveAttribute('href', '#/apps/1')
    expect(within(rows[1]).getByRole('link', { name: 'hello.apps.test' })).toHaveAttribute(
      'href',
      'http://hello.apps.test/',
    )
    expect(within(rows[1]).getByText('not deployed')).toBeInTheDocument()
    expect(within(rows[2]).getByText('0123456')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Apps' })).toHaveAttribute('aria-current', 'page')
  })

  it('registers an app as JSON and opens it', async () => {
    const fetchMock = mockFetch({
      'GET /api/platform/apps': () => [200, []],
      'POST /api/platform/apps': () => [201, app({ id: 5 })],
      'GET /api/platform/apps/5': () => [200, detail({ id: 5 })],
    })
    window.location.hash = '#/apps'
    render(<App />)
    await screen.findByText('No apps yet.')

    fireEvent.change(screen.getByLabelText(/^Name/), { target: { value: 'hello' } })
    fireEvent.change(screen.getByLabelText(/^GitHub repository/), { target: { value: 'https://github.com/o/repo' } })
    fireEvent.change(screen.getByLabelText(/^Source directory/), { target: { value: 'examples/hello' } })
    fireEvent.click(screen.getByRole('button', { name: 'Register' }))

    // Opens the new app's page.
    expect(await screen.findByRole('heading', { name: 'hello' })).toBeInTheDocument()
    expect(window.location.hash).toBe('#/apps/5')
    const [[, init]] = sent(fetchMock, 'POST', '/api/platform/apps')
    expect(init?.headers).toEqual({ 'Content-Type': 'application/json' })
    expect(JSON.parse(init?.body as string)).toEqual({
      name: 'hello',
      repoUrl: 'https://github.com/o/repo',
      sourceDir: 'examples/hello',
      port: 8080,
      healthCheckPath: '/',
    })
  })

  it("shows what the platform has fixed recently, with a crash's last lines", async () => {
    mockFetch({
      'GET /api/platform/apps': () => [200, [app()]],
      'GET /api/platform/events': () => [
        200,
        [
          event({ id: 3, kind: 'crash_looping', message: 'hello keeps stopping', output: ['listening', 'panic: nil map'] }),
          event({ id: 2, appId: null, kind: 'routes_reapplied', message: "Caddy's routes didn't match" }),
        ],
      ],
    })
    window.location.hash = '#/apps'
    render(<App />)

    const looping = await screen.findByText('crash-looping')
    expect(looping).toHaveClass('badge-down')
    expect(screen.getByText('routes fixed')).toHaveClass('badge-neutral')
    expect(screen.getByText(/hello keeps stopping/)).toBeInTheDocument()
    // The output folds out under its event; the routes event has none.
    expect(screen.getAllByText(/lines of output/)).toHaveLength(1)
    fireEvent.click(screen.getByText('Its last 2 lines of output'))
    expect(screen.getByText(/panic: nil map/)).toBeVisible()
  })

  it('says when nothing has needed fixing', async () => {
    mockFetch({ 'GET /api/platform/apps': () => [200, []] })
    window.location.hash = '#/apps'
    render(<App />)
    expect(await screen.findByText('Nothing has needed fixing.')).toBeInTheDocument()
  })

  it("shows the platform's error when registering fails", async () => {
    mockFetch({
      'GET /api/platform/apps': () => [200, []],
      'POST /api/platform/apps': () => [409, { error: 'conflict: an app with that name already exists' }],
    })
    window.location.hash = '#/apps'
    render(<App />)
    await screen.findByText('No apps yet.')

    fireEvent.change(screen.getByLabelText(/^Name/), { target: { value: 'hello' } })
    fireEvent.change(screen.getByLabelText(/^GitHub repository/), { target: { value: 'https://github.com/o/repo' } })
    fireEvent.click(screen.getByRole('button', { name: 'Register' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('an app with that name already exists')
    expect(window.location.hash).toBe('#/apps')
  })
})

describe('App page', () => {
  it('shows env var names and deployments, with why one failed', async () => {
    mockFetch({
      'GET /api/platform/apps/1': () => [
        200,
        detail({
          envKeys: ['GREETING', 'TOKEN'],
          currentDeployment: deployment(),
          deployments: [
            deployment({ id: 8, status: 'failed', error: 'build failed: npm ci returned 1' }),
            deployment(),
          ],
        }),
      ],
    })
    window.location.hash = '#/apps/1'
    render(<App />)

    expect(await screen.findByRole('heading', { name: 'hello' })).toBeInTheDocument()
    expect(screen.getByText('GREETING')).toBeInTheDocument()
    expect(screen.getByText('TOKEN')).toBeInTheDocument()
    expect(screen.getByText('failed')).toHaveClass('badge-down')
    expect(screen.getByText('healthy')).toHaveClass('badge-up')
    expect(screen.getByText('build failed: npm ci returned 1')).toBeInTheDocument()
    expect(screen.getByText('/examples/hello', { exact: false })).toBeInTheDocument()
  })

  it('deploys a commit and refreshes straight away', async () => {
    let deployed = false
    const fetchMock = mockFetch({
      'GET /api/platform/apps/1': () => [
        200,
        detail({ deployments: deployed ? [deployment({ id: 8, status: 'queued' })] : [] }),
      ],
      'POST /api/platform/apps/1/deployments': () => {
        deployed = true
        return [202, deployment({ id: 8, status: 'queued' })]
      },
    })
    window.location.hash = '#/apps/1'
    render(<App />)
    await screen.findByText('No deploys yet.')

    const deploy = screen.getByRole('button', { name: 'Deploy' })
    const input = screen.getByLabelText('Commit SHA')
    fireEvent.change(input, { target: { value: 'main' } })
    expect(deploy).toBeDisabled() // not a full SHA
    // Pasted with stray whitespace and capitals: cleaned up.
    fireEvent.change(input, { target: { value: ` ${SHA.toUpperCase()} ` } })
    fireEvent.click(deploy)

    expect(await screen.findByText('queued')).toHaveClass('badge-warn')
    expect(JSON.parse(sent(fetchMock, 'POST', '/api/platform/apps/1/deployments')[0][1]?.body as string)).toEqual({
      commitSha: SHA,
    })
    // While it's in progress, another deploy can't be started.
    expect(screen.getByText('A deploy is in progress.')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Deploy' })).toBeDisabled()
    // Its log opens, to watch it live.
    expect(FakeEventSource.latest().url).toBe('/api/platform/apps/1/deployments/8/logs')
  })

  it("shows the latest deploy's log, another deploy's, or the app's output", async () => {
    mockFetch({
      'GET /api/platform/apps/1': () => [
        200,
        detail({
          currentDeployment: deployment(),
          deployments: [deployment({ id: 8, status: 'building' }), deployment()],
        }),
      ],
    })
    window.location.hash = '#/apps/1'
    render(<App />)

    // The latest by default.
    expect(await screen.findByRole('button', { name: 'Deploy #8' })).toHaveAttribute('aria-pressed', 'true')
    const deployLog = FakeEventSource.latest()
    expect(deployLog.url).toBe('/api/platform/apps/1/deployments/8/logs')
    deployLog.open()
    deployLog.send({ stream: 'build', line: 'Step 1/9 : FROM golang:1.25' })
    expect(await screen.findByLabelText('Deploy #8 log')).toHaveTextContent('Step 1/9')

    // An older one, from the table.
    fireEvent.click(screen.getByRole('button', { name: "Show deploy #7's log" }))
    expect(FakeEventSource.latest().url).toBe('/api/platform/apps/1/deployments/7/logs')
    expect(deployLog.readyState).toBe(FakeEventSource.CLOSED) // the one it replaced
    expect(screen.getByRole('button', { name: "Show deploy #7's log" })).toHaveAttribute('aria-pressed', 'true')

    // The live container's output.
    fireEvent.click(screen.getByRole('button', { name: 'App output' }))
    const output = FakeEventSource.latest()
    expect(output.url).toBe('/api/platform/apps/1/logs')
    output.open()
    output.send({ stream: 'stdout', line: 'listening on :8080' })
    expect(await screen.findByLabelText('App output')).toHaveTextContent('listening on :8080')
  })

  it("deploys the latest commit on the app's branch", async () => {
    const fetchMock = mockFetch({
      'GET /api/platform/apps/1': () => [200, detail()],
      'POST /api/platform/apps/1/deployments/latest': () => [202, deployment({ id: 9, status: 'queued' })],
    })
    window.location.hash = '#/apps/1'
    render(<App />)

    fireEvent.click(await screen.findByRole('button', { name: 'Deploy latest from main' }))
    await vi.waitFor(() => expect(FakeEventSource.latest().url).toBe('/api/platform/apps/1/deployments/9/logs'))
    expect(sent(fetchMock, 'POST', '/api/platform/apps/1/deployments/latest')).toHaveLength(1)
  })

  it('turns push-to-deploy off and on, and shows a push waiting to deploy', async () => {
    let auto = true
    const fetchMock = mockFetch({
      'GET /api/platform/apps/1': () => [200, detail({ autoDeploy: auto, nextCommitSha: auto ? SHA : null })],
      'PATCH /api/platform/apps/1': (init) => {
        auto = (JSON.parse(init?.body as string) as { autoDeploy: boolean }).autoDeploy
        return [200, app({ autoDeploy: auto })]
      },
    })
    window.location.hash = '#/apps/1'
    render(<App />)

    const box = await screen.findByRole('checkbox', { name: 'Deploy automatically on every push to main' })
    expect(box).toBeChecked()
    expect(screen.getByText('0123456')).toBeInTheDocument() // the waiting push, short

    fireEvent.click(box)
    await vi.waitFor(() => expect(screen.getByRole('checkbox')).not.toBeChecked())
    expect(JSON.parse(sent(fetchMock, 'PATCH', '/api/platform/apps/1')[0][1]?.body as string)).toEqual({
      autoDeploy: false,
    })
    expect(screen.queryByText('0123456')).not.toBeInTheDocument()
  })

  it('rolls back to an earlier version, after asking', async () => {
    let rolledBack = false
    const live = deployment({ id: 8 })
    const earlier = deployment({ id: 7, status: 'superseded', rollbackable: true })
    const rb = deployment({ id: 9, status: 'queued', rollbackOf: 7 })
    const fetchMock = mockFetch({
      'GET /api/platform/apps/1': () => [
        200,
        rolledBack
          ? detail({ autoDeploy: false, currentDeployment: live, deployments: [rb, live, earlier] })
          : detail({ currentDeployment: live, deployments: [live, earlier] }),
      ],
      'POST /api/platform/apps/1/deployments/7/rollback': () => {
        rolledBack = true
        return [202, rb]
      },
    })
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false)
    window.location.hash = '#/apps/1'
    render(<App />)

    // Only versions that can be rolled back to have the button.
    const button = await screen.findByRole('button', { name: 'Roll back to deploy #7' })
    expect(screen.queryByRole('button', { name: 'Roll back to deploy #8' })).not.toBeInTheDocument()

    // Cancelled: nothing sent.
    fireEvent.click(button)
    expect(confirm).toHaveBeenCalledWith(expect.stringContaining("today's env vars"))
    expect(sent(fetchMock, 'POST', '/api/platform/apps/1/deployments/7/rollback')).toHaveLength(0)

    confirm.mockReturnValue(true)
    fireEvent.click(button)
    expect(await screen.findByText('rollback of #7')).toBeInTheDocument()
    expect(sent(fetchMock, 'POST', '/api/platform/apps/1/deployments/7/rollback')).toHaveLength(1)
    // Its log opens, auto-deploy says why it's off, and no other rollback
    // can start while this one runs.
    expect(FakeEventSource.latest().url).toBe('/api/platform/apps/1/deployments/9/logs')
    expect(screen.getByText(/Turned off by the rollback to #7/)).toBeInTheDocument()
    expect(screen.getByRole('checkbox')).not.toBeChecked()
    expect(screen.getByRole('button', { name: 'Roll back to deploy #7' })).toBeDisabled()
  })

  it('shows why a rollback was refused', async () => {
    mockFetch({
      'GET /api/platform/apps/1': () => [
        200,
        detail({ deployments: [deployment({ id: 8 }), deployment({ id: 7, status: 'superseded', rollbackable: true })] }),
      ],
      'POST /api/platform/apps/1/deployments/7/rollback': () => [
        409,
        { error: "can't roll back to this deployment: its image is no longer kept" },
      ],
    })
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    window.location.hash = '#/apps/1'
    render(<App />)

    fireEvent.click(await screen.findByRole('button', { name: 'Roll back to deploy #7' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('its image is no longer kept')
  })

  it("shows the app's own events", async () => {
    mockFetch({
      'GET /api/platform/apps/1': () => [200, detail({ events: [event({ kind: 'recreated', message: 'Recreated hello' })] })],
    })
    window.location.hash = '#/apps/1'
    render(<App />)
    expect(await screen.findByText('recreated')).toHaveClass('badge-warn')
    expect(screen.getByText(/Recreated hello/)).toBeInTheDocument()
    expect(screen.getByText('2m ago', { exact: false })).toBeInTheDocument()
  })

  it('adds a database, after asking', async () => {
    let added = false
    const fetchMock = mockFetch({
      'GET /api/platform/apps/1': () => [
        200,
        detail({
          currentDeployment: deployment(),
          database: added ? { name: 'ops-db-hello', createdAt: new Date().toISOString(), state: 'creating' } : null,
        }),
      ],
      'POST /api/platform/apps/1/database': () => {
        added = true
        return [201, { name: 'ops-db-hello', createdAt: new Date().toISOString(), state: 'creating' }]
      },
    })
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(true)
    window.location.hash = '#/apps/1'
    render(<App />)

    fireEvent.click(await screen.findByRole('button', { name: 'Add a database' }))
    expect(confirm).toHaveBeenCalledWith(expect.stringContaining('DATABASE_URL from its next deploy'))
    expect(await screen.findByText('being created')).toHaveClass('badge-warn')
    expect(screen.getByText('ops-db-hello')).toBeInTheDocument()
    expect(sent(fetchMock, 'POST', '/api/platform/apps/1/database')).toHaveLength(1)
    // The live version predates it, so it says to deploy.
    expect(screen.getByText(/Deploy the app to start using it/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Add a database' })).not.toBeInTheDocument()
  })

  it('shows a running database the live version already uses', async () => {
    const created = new Date(Date.now() - 60 * 60_000).toISOString()
    mockFetch({
      'GET /api/platform/apps/1': () => [
        200,
        detail({ currentDeployment: deployment(), database: { name: 'ops-db-hello', createdAt: created, state: 'running' } }),
      ],
    })
    window.location.hash = '#/apps/1'
    render(<App />)
    expect(await screen.findByText('running')).toHaveClass('badge-up')
    expect(screen.queryByText(/Deploy the app to start using it/)).not.toBeInTheDocument()
  })

  it('has no app output to show when nothing is live', async () => {
    mockFetch({
      'GET /api/platform/apps/1': () => [200, detail({ deployments: [deployment({ status: 'failed' })] })],
    })
    window.location.hash = '#/apps/1'
    render(<App />)
    expect(await screen.findByRole('button', { name: 'App output' })).toBeDisabled()
  })

  it('sets and removes env vars without ever reading a value back', async () => {
    let keys = ['OLD']
    const fetchMock = mockFetch({
      'GET /api/platform/apps/1': () => [200, detail({ envKeys: keys })],
      'PUT /api/platform/apps/1/env/GREETING': () => {
        keys = [...keys, 'GREETING']
        return [204, null]
      },
      'DELETE /api/platform/apps/1/env/OLD': () => {
        keys = keys.filter((k) => k !== 'OLD')
        return [204, null]
      },
    })
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    window.location.hash = '#/apps/1'
    render(<App />)
    await screen.findByText('OLD')

    fireEvent.change(screen.getByPlaceholderText('NAME'), { target: { value: 'GREETING' } })
    fireEvent.change(screen.getByPlaceholderText('value'), { target: { value: 'hi' } })
    fireEvent.click(screen.getByRole('button', { name: 'Set' }))
    expect(await screen.findByText('GREETING')).toBeInTheDocument()
    expect(JSON.parse(sent(fetchMock, 'PUT', '/api/platform/apps/1/env/GREETING')[0][1]?.body as string)).toEqual({
      value: 'hi',
    })

    fireEvent.click(within(screen.getByText('OLD').closest('li')!).getByRole('button', { name: 'Remove' }))
    await vi.waitFor(() => expect(screen.queryByText('OLD')).not.toBeInTheDocument())
    expect(window.confirm).toHaveBeenCalled()
  })

  it('shows an error if the app does not exist', async () => {
    mockFetch({})
    window.location.hash = '#/apps/9'
    render(<App />)
    expect(await screen.findByRole('alert')).toHaveTextContent("Couldn't load the app: not found")
  })
})

describe('navigation', () => {
  it('follows the hash', async () => {
    mockFetch({ 'GET /api/platform/apps': () => [200, []] })
    window.location.hash = '#/nope'
    render(<App />)
    expect(screen.getByText('No such page.')).toBeInTheDocument()

    window.location.hash = '#/apps'
    expect(await screen.findByText('No apps yet.')).toBeInTheDocument()
  })
})
