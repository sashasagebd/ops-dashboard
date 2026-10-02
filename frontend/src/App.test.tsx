import { render, screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import App from './App'

function mockFetch(status: number, body: unknown) {
  const fetchMock = vi.fn(() => Promise.resolve(new Response(JSON.stringify(body), { status })))
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

afterEach(() => {
  vi.unstubAllGlobals()
  vi.useRealTimers()
})

describe('App', () => {
  it('lists containers from the API', async () => {
    // Fake only Date, so uptimes are predictable but Testing Library's
    // timers (used by findBy*) still run.
    vi.useFakeTimers({ toFake: ['Date'] })
    vi.setSystemTime(new Date('2026-10-01T12:00:00Z'))
    const fetchMock = mockFetch(200, [
      {
        id: 'a1',
        name: 'discord-bot',
        image: 'discord-bot:latest',
        state: 'running',
        status: 'Up 2 days',
        startedAt: '2026-09-29T08:00:00Z',
        finishedAt: null,
      },
      {
        id: 'b2',
        name: 'minecraft',
        image: 'itzg/minecraft-server',
        state: 'exited',
        status: 'Exited (0) 1 hour ago',
        startedAt: '2026-09-30T00:00:00Z',
        finishedAt: '2026-10-01T11:00:00Z',
      },
    ])

    render(<App />)

    expect(screen.getByText('Loading…')).toBeInTheDocument()
    expect(await screen.findByText('discord-bot')).toBeInTheDocument()
    expect(screen.getByText('itzg/minecraft-server')).toBeInTheDocument()
    expect(screen.getByText('running')).toHaveClass('badge-up')
    expect(screen.getByText('exited')).toHaveClass('badge-down')
    expect(screen.getByText('up 2d 4h')).toHaveAttribute('title', 'Up 2 days')
    expect(screen.getByText('down 1h 0m')).toHaveAttribute('title', 'Exited (0) 1 hour ago')
    expect(fetchMock).toHaveBeenCalledWith('/api/containers', expect.anything())
  })

  it('shows a message when there are no containers', async () => {
    mockFetch(200, [])

    render(<App />)

    expect(await screen.findByText('No containers.')).toBeInTheDocument()
  })

  it('shows the API error message', async () => {
    mockFetch(502, { error: 'could not list containers' })

    render(<App />)

    expect(await screen.findByRole('alert')).toHaveTextContent('could not list containers')
  })
})
