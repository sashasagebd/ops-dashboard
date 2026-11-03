import { act } from '@testing-library/react'
import type { LogLine } from '../api'

// FakeEventSource stands in for the browser's EventSource (jsdom has none).
// Install it with vi.stubGlobal('EventSource', FakeEventSource); tests then
// drive each stream through the helpers below.
export class FakeEventSource {
  static readonly CONNECTING = 0
  static readonly OPEN = 1
  static readonly CLOSED = 2
  // Every stream opened, newest last; reset in each test's setup.
  static instances: FakeEventSource[] = []

  readonly url: string
  readyState = FakeEventSource.CONNECTING
  onopen: ((e: Event) => void) | null = null
  onmessage: ((e: MessageEvent<string>) => void) | null = null
  onerror: ((e: Event) => void) | null = null
  private listeners: Record<string, ((e: MessageEvent<string>) => void)[]> = {}

  constructor(url: string) {
    this.url = url
    FakeEventSource.instances.push(this)
  }

  static latest(): FakeEventSource {
    const s = FakeEventSource.instances.at(-1)
    if (s === undefined) throw new Error('no EventSource was opened')
    return s
  }

  addEventListener(type: string, fn: (e: MessageEvent<string>) => void) {
    ;(this.listeners[type] ??= []).push(fn)
  }

  close() {
    this.readyState = FakeEventSource.CLOSED
  }

  // Test helpers: what the server (or the network) does.

  open() {
    act(() => {
      this.readyState = FakeEventSource.OPEN
      this.onopen?.(new Event('open'))
    })
  }

  send(...lines: Partial<LogLine>[]) {
    act(() => {
      for (const l of lines) {
        const line: LogLine = { at: '2026-11-03T10:00:00Z', stream: 'platform', line: '', ...l }
        this.onmessage?.(new MessageEvent('message', { data: JSON.stringify(line) }))
      }
    })
  }

  end() {
    act(() => {
      for (const fn of this.listeners.end ?? []) fn(new MessageEvent('end', { data: '{}' }))
    })
  }

  // error: the connection dropped (the browser retries), or with gaveUp the
  // answer wasn't a stream at all and the browser won't.
  error(gaveUp = false) {
    act(() => {
      this.readyState = gaveUp ? FakeEventSource.CLOSED : FakeEventSource.CONNECTING
      this.onerror?.(new Event('error'))
    })
  }
}
