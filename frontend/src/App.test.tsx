import { act, render, screen, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { Container, ContainersResponse, HostStats } from './api'
import App from './App'

const NOW = new Date('2026-10-01T12:00:00Z')

function container(overrides: Partial<Container> = {}): Container {
  return {
    id: 'm1',
    name: 'mc',
    image: 'itzg/minecraft-server',
    state: 'running',
    status: 'Up 2 days (healthy)',
    startedAt: '2026-09-29T08:00:00Z',
    finishedAt: null,
    cpuPercent: 0.356,
    memoryBytes: 5_418_610_688,
    memoryLimitBytes: 16_112_287_744,
    ...overrides,
  }
}

const GiB = 1024 ** 3

function hostStats(overrides: Partial<HostStats> = {}): HostStats {
  return {
    cpuPercent: 4.2,
    memoryBytes: 6 * GiB,
    memoryTotalBytes: 15 * GiB,
    // 10 GiB reserved for root: in neither used nor available, like df.
    diskUsedBytes: 50 * GiB,
    diskAvailableBytes: 150 * GiB,
    diskTotalBytes: 210 * GiB,
    ...overrides,
  }
}

function snapshot(containers: Container[], overrides: Partial<ContainersResponse> = {}): ContainersResponse {
  return { updatedAt: NOW.toISOString(), stale: false, host: null, containers, ...overrides }
}

// mockFetch answers successive calls with successive [status, body] pairs,
// repeating the last one. A body can be a function, called per request (e.g.
// to stamp the current fake time). It returns just the parts of a Response
// that fetchContainers uses, so the body resolves immediately under fake
// timers.
function mockFetch(...responses: [number, unknown][]) {
  let call = 0
  const fetchMock = vi.fn(() => {
    const [status, body] = responses[Math.min(call++, responses.length - 1)]
    const json: unknown = typeof body === 'function' ? (body as () => unknown)() : body
    return Promise.resolve({
      ok: status < 400,
      status,
      statusText: '',
      json: () => Promise.resolve(json),
    } as Response)
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

// advance moves the fake clock forward and lets the resulting fetches and
// re-renders finish.
async function advance(ms: number) {
  await act(() => vi.advanceTimersByTimeAsync(ms))
}

function setHidden(hidden: boolean) {
  Object.defineProperty(document, 'hidden', { configurable: true, get: () => hidden })
  document.dispatchEvent(new Event('visibilitychange'))
}

beforeEach(() => {
  // Only timers and Date are faked; promises still resolve normally.
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'Date'] })
  vi.setSystemTime(NOW)
})

afterEach(() => {
  vi.unstubAllGlobals()
  vi.useRealTimers()
  // Remove the override so document.hidden reads jsdom's own value again.
  Reflect.deleteProperty(document, 'hidden')
})

describe('App', () => {
  it('lists containers with uptime, CPU and memory', async () => {
    const fetchMock = mockFetch([
      200,
      snapshot([
        container(),
        container({
          id: 'b1',
          name: 'discordbot',
          image: 'discordbot',
          state: 'exited',
          status: 'Exited (1) 1 hour ago',
          finishedAt: '2026-10-01T11:00:00Z',
          cpuPercent: null,
          memoryBytes: null,
          memoryLimitBytes: null,
        }),
      ]),
    ])

    render(<App />)
    expect(screen.getByText('Loading…')).toBeInTheDocument()
    await advance(0)

    expect(screen.getByText('itzg/minecraft-server')).toBeInTheDocument()
    expect(screen.getByText('running')).toHaveClass('badge-up')
    expect(screen.getByText('exited')).toHaveClass('badge-down')
    expect(screen.getByText('up 2d 4h')).toHaveAttribute('title', 'Up 2 days (healthy)')
    expect(screen.getByText('down 1h 0m')).toHaveAttribute('title', 'Exited (1) 1 hour ago')
    expect(screen.getByText('0.4%')).toBeInTheDocument()
    expect(screen.getByText('5.0 GiB')).toBeInTheDocument()
    expect(screen.getAllByText('—')).toHaveLength(2) // discordbot's CPU and memory
    expect(screen.queryByRole('status')).not.toBeInTheDocument()
    expect(fetchMock).toHaveBeenCalledWith('/api/containers', expect.anything())
  })

  it('shows a message when there are no containers', async () => {
    mockFetch([200, snapshot([])])

    render(<App />)
    await advance(0)

    expect(screen.getByText('No containers.')).toBeInTheDocument()
  })

  it('shows the API error, then recovers on a later poll', async () => {
    mockFetch([502, { error: 'could not list containers' }], [200, snapshot([container()])])

    render(<App />)
    await advance(0)
    expect(screen.getByRole('alert')).toHaveTextContent('could not list containers')

    await advance(5000)
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(screen.getByText('mc')).toBeInTheDocument()
  })

  it('refreshes every 5 seconds, and uptimes move on', async () => {
    // Started 59m57s before NOW, so the next refresh crosses the hour.
    const startedAt = '2026-10-01T11:00:03Z'
    const fetchMock = mockFetch(
      [200, snapshot([container({ startedAt })])],
      [200, () => snapshot([container({ startedAt, cpuPercent: 12.34 })], { updatedAt: new Date().toISOString() })],
    )

    render(<App />)
    await advance(0)
    expect(screen.getByText('up 59m')).toBeInTheDocument()

    await advance(4999)
    expect(fetchMock).toHaveBeenCalledTimes(1)

    await advance(1)
    expect(fetchMock).toHaveBeenCalledTimes(2)
    expect(screen.getByText('12.3%')).toBeInTheDocument()
    expect(screen.getByText('up 1h 0m')).toBeInTheDocument()
  })

  it('keeps the data and warns when a refresh fails', async () => {
    mockFetch([200, snapshot([container()])], [500, { error: 'boom' }])

    render(<App />)
    await advance(0)
    await advance(5000)

    expect(screen.getByText('mc')).toBeInTheDocument()
    expect(screen.getByRole('status')).toHaveTextContent("Can't reach the dashboard server. Showing data from <1m ago.")
  })

  it("warns when the server can't reach Docker", async () => {
    const updatedAt = new Date(NOW.getTime() - 2 * 60_000).toISOString()
    mockFetch([200, snapshot([container()], { stale: true, updatedAt })])

    render(<App />)
    await advance(0)

    expect(screen.getByRole('status')).toHaveTextContent("The dashboard can't reach Docker. Showing data from 2m ago.")
  })

  it("warns when the server's data stops updating", async () => {
    const updatedAt = new Date(NOW.getTime() - 3 * 60_000).toISOString()
    mockFetch([200, snapshot([container()], { updatedAt })])

    render(<App />)
    await advance(0)

    expect(screen.getByRole('status')).toHaveTextContent('Updates from Docker have stopped. Showing data from 3m ago.')
  })

  it('pauses while the tab is hidden and refreshes when it is shown', async () => {
    const fetchMock = mockFetch([200, snapshot([container()])])

    render(<App />)
    await advance(0)
    expect(fetchMock).toHaveBeenCalledTimes(1)

    setHidden(true)
    await advance(60_000)
    expect(fetchMock).toHaveBeenCalledTimes(1)

    setHidden(false)
    await advance(0)
    expect(fetchMock).toHaveBeenCalledTimes(2)
  })

  describe('server tiles', () => {
    it('show CPU, memory and disk with usage bars', async () => {
      mockFetch([200, snapshot([], { host: hostStats() })])

      render(<App />)
      await advance(0)

      const cpu = within(screen.getByRole('region', { name: 'CPU' }))
      expect(cpu.getByText('4.2%')).toBeInTheDocument()
      expect(cpu.getByRole('meter')).toHaveAttribute('aria-valuenow', '4')

      const memory = within(screen.getByRole('region', { name: 'Memory' }))
      expect(memory.getByText('6.0 GiB')).toBeInTheDocument()
      expect(memory.getByText('of 15.0 GiB')).toBeInTheDocument()
      expect(memory.getByRole('meter')).toHaveAttribute('aria-valuenow', '40')

      // Like df's Use%: 50 / (50 + 150) = 25%, not 50 / 210.
      const disk = within(screen.getByRole('region', { name: 'Disk' }))
      expect(disk.getByText('50.0 GiB')).toBeInTheDocument()
      expect(disk.getByText('of 210.0 GiB')).toBeInTheDocument()
      expect(disk.getByRole('meter')).toHaveAttribute('aria-valuenow', '25')
    })

    it('turn the bar amber from 80% and red from 90%', async () => {
      mockFetch([
        200,
        snapshot([], {
          host: hostStats({
            memoryBytes: 12.75 * GiB, // 85%
            diskUsedBytes: 95 * GiB,
            diskAvailableBytes: 5 * GiB, // 95%
          }),
        }),
      ])

      render(<App />)
      await advance(0)

      const fill = (name: string) => within(screen.getByRole('region', { name })).getByRole('meter').firstElementChild
      expect(fill('CPU')).toHaveClass('bar-ok')
      expect(fill('Memory')).toHaveClass('bar-warn')
      expect(fill('Disk')).toHaveClass('bar-high')
    })

    it('show a dash for CPU before the second sample', async () => {
      mockFetch([200, snapshot([], { host: hostStats({ cpuPercent: null }) })])

      render(<App />)
      await advance(0)

      const cpu = within(screen.getByRole('region', { name: 'CPU' }))
      expect(cpu.getByText('—')).toBeInTheDocument()
      expect(cpu.getByRole('meter')).not.toHaveAttribute('aria-valuenow')
    })

    it('are replaced by a note when the host could not be read', async () => {
      mockFetch([200, snapshot([container()], { host: null })])

      render(<App />)
      await advance(0)

      expect(screen.getByText('Server stats unavailable.')).toBeInTheDocument()
      expect(screen.queryByRole('meter')).not.toBeInTheDocument()
      expect(screen.getByText('mc')).toBeInTheDocument()
    })
  })
})
