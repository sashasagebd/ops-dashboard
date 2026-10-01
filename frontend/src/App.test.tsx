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
})

describe('App', () => {
  it('lists containers from the API', async () => {
    const fetchMock = mockFetch(200, [
      { id: 'a1', name: 'discord-bot', image: 'discord-bot:latest', state: 'running', status: 'Up 2 days' },
      { id: 'b2', name: 'minecraft', image: 'itzg/minecraft-server', state: 'exited', status: 'Exited (0) 1 hour ago' },
    ])

    render(<App />)

    expect(screen.getByText('Loading…')).toBeInTheDocument()
    expect(await screen.findByText('discord-bot')).toBeInTheDocument()
    expect(screen.getByText('itzg/minecraft-server')).toBeInTheDocument()
    expect(screen.getByText('running')).toHaveClass('badge-up')
    expect(screen.getByText('exited')).toHaveClass('badge-down')
    expect(fetchMock).toHaveBeenCalledWith('/api/containers', expect.anything())
  })

  it('shows a message when no containers are running', async () => {
    mockFetch(200, [])

    render(<App />)

    expect(await screen.findByText('No running containers.')).toBeInTheDocument()
  })

  it('shows the API error message', async () => {
    mockFetch(502, { error: 'could not list containers' })

    render(<App />)

    expect(await screen.findByRole('alert')).toHaveTextContent('could not list containers')
  })
})
