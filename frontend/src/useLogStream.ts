import { useEffect, useState } from 'react'
import type { LogLine } from './api'

// LogStreamState is a log stream's lines so far, and where the stream is:
// connecting, open (following new lines), reconnecting after a dropped
// connection (the browser does this by itself, resuming after the last
// line), ended (the server said there's nothing more), or failed (the
// browser gave up, e.g. because the answer was an error, not a stream).
export type LogStreamState = {
  status: 'connecting' | 'open' | 'reconnecting' | 'ended' | 'failed'
  lines: LogLine[]
}

// The most lines kept: the newest, once an app's output passes it.
const MAX_LINES = 5000
// Lines arriving within this long are added in one render, since a replayed
// deploy log is thousands of messages at once.
const BATCH_MS = 100

// useLogStream follows the log stream at url. Its state starts empty only
// when the component mounts, so give the component a key per url.
export function useLogStream(url: string): LogStreamState {
  const [state, setState] = useState<LogStreamState>({ status: 'connecting', lines: [] })

  useEffect(() => {
    const source = new EventSource(url)
    let pending: LogLine[] = []
    let timer: ReturnType<typeof setTimeout> | undefined
    const flush = () => {
      timer = undefined
      if (pending.length === 0) return
      const batch = pending
      pending = []
      setState((s) => ({ ...s, lines: [...s.lines, ...batch].slice(-MAX_LINES) }))
    }

    source.onopen = () => setState((s) => ({ ...s, status: 'open' }))
    source.onmessage = (e: MessageEvent<string>) => {
      pending.push(JSON.parse(e.data) as LogLine)
      timer ??= setTimeout(flush, BATCH_MS)
    }
    // The server's "nothing more": closed here, or the browser would
    // reconnect and replay the log again.
    source.addEventListener('end', () => {
      source.close()
      clearTimeout(timer)
      flush()
      setState((s) => ({ ...s, status: 'ended' }))
    })
    source.onerror = () => {
      const status = source.readyState === EventSource.CLOSED ? 'failed' : 'reconnecting'
      setState((s) => ({ ...s, status }))
    }

    return () => {
      source.close()
      clearTimeout(timer)
    }
  }, [url])

  return state
}
