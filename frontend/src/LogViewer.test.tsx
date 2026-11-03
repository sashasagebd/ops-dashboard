import { fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { LogViewer } from './LogViewer'
import { FakeEventSource } from './test/fakeEventSource'

beforeEach(() => {
  FakeEventSource.instances = []
  vi.stubGlobal('EventSource', FakeEventSource)
})

afterEach(() => {
  vi.unstubAllGlobals()
})

function renderViewer(restartable = false) {
  render(
    <LogViewer
      url="/logs"
      label="Deploy #7 log"
      emptyText="No log."
      endedText="End of log."
      restartable={restartable}
    />,
  )
  return FakeEventSource.latest()
}

describe('LogViewer', () => {
  it('shows lines as they arrive, then the end', async () => {
    const source = renderViewer()
    expect(source.url).toBe('/logs')
    expect(screen.getByRole('status')).toHaveTextContent('Connecting…')

    source.open()
    expect(screen.getByRole('status')).toHaveTextContent('Waiting for output…')
    source.send({ line: 'Building image' }, { stream: 'build', line: 'Step 1/2 : FROM busybox' })
    const log = await screen.findByLabelText('Deploy #7 log')
    expect(log).toHaveTextContent('Building image')
    expect(screen.getByText('Building image')).toHaveClass('log-platform')
    expect(screen.getByText('Step 1/2 : FROM busybox')).toHaveClass('log-build')
    expect(screen.getByRole('status')).toHaveTextContent('Live')

    source.send({ line: 'Live at http://hello.apps.test/' })
    source.end()
    expect(screen.getByRole('status')).toHaveTextContent('End of log.')
    // Lines sent just before the end aren't lost in the batching.
    expect(log).toHaveTextContent('Live at http://hello.apps.test/')
    expect(source.readyState).toBe(FakeEventSource.CLOSED) // or the browser would replay it
    // A deploy's log is over for good: nothing to reconnect to.
    expect(screen.queryByRole('button')).not.toBeInTheDocument()
  })

  it('says when a stream ended with nothing in it', () => {
    const source = renderViewer()
    source.open()
    source.end()
    expect(screen.getByText('No log.')).toBeInTheDocument()
  })

  it('shows a dropped connection while the browser reconnects', async () => {
    const source = renderViewer()
    source.open()
    source.send({ line: 'one' })
    source.error()
    expect(screen.getByRole('status')).toHaveTextContent('Connection lost; reconnecting…')
    // The lines so far stay.
    expect(await screen.findByText('one')).toBeInTheDocument()
    source.open()
    expect(screen.getByRole('status')).toHaveTextContent('Live')
  })

  it('offers to try again once the browser gives up', () => {
    const source = renderViewer()
    source.error(true)
    expect(screen.getByRole('status')).toHaveTextContent("Couldn't load the log.")
    fireEvent.click(screen.getByRole('button', { name: 'Try again' }))
    expect(FakeEventSource.instances).toHaveLength(2)
    expect(FakeEventSource.latest()).not.toBe(source)
  })

  it("reconnects to an app's output when asked, after it stopped", async () => {
    const source = renderViewer(true)
    source.open()
    source.send({ stream: 'stderr', line: 'panic: oops' })
    expect(await screen.findByText('panic: oops')).toHaveClass('log-stderr')
    source.end()

    fireEvent.click(screen.getByRole('button', { name: 'Reconnect' }))
    expect(FakeEventSource.instances).toHaveLength(2)
    expect(screen.getByRole('status')).toHaveTextContent('Connecting…')
  })

  it('closes the stream when it goes away', () => {
    const { unmount } = render(<LogViewer url="/logs" label="Log" emptyText="" endedText="" />)
    const source = FakeEventSource.latest()
    source.open()
    unmount()
    expect(source.readyState).toBe(FakeEventSource.CLOSED)
  })
})
