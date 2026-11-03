import { useLayoutEffect, useRef, useState } from 'react'
import { useLogStream, type LogStreamState } from './useLogStream'

type Props = {
  url: string
  label: string // names the log for screen readers
  emptyText: string // shown when the stream ended with no lines at all
  endedText: string // what the end of the stream means for this log
  restartable?: boolean // offer to reconnect once it has ended (app output)
}

// LogViewer shows a log stream, following new lines while it's scrolled to
// the bottom (scroll up to read, and it stays put).
export function LogViewer(props: Props) {
  // Bumped to start the stream again: a new key mounts a fresh stream.
  const [attempt, setAttempt] = useState(0)
  return <LogOutput key={attempt} {...props} onRestart={() => setAttempt((a) => a + 1)} />
}

function LogOutput({ url, label, emptyText, endedText, restartable, onRestart }: Props & { onRestart: () => void }) {
  const state = useLogStream(url)
  const ref = useRef<HTMLPreElement>(null)
  const atBottom = useRef(true)

  useLayoutEffect(() => {
    const el = ref.current
    if (el !== null && atBottom.current) el.scrollTop = el.scrollHeight
  }, [state.lines])

  const onScroll = () => {
    const el = ref.current
    if (el !== null) atBottom.current = el.scrollHeight - el.scrollTop - el.clientHeight < 24
  }

  const canRestart = state.status === 'failed' || (state.status === 'ended' && restartable === true)
  return (
    <div className="log-viewer">
      <p className="muted log-status" role="status">
        {statusText(state, endedText)}
        {canRestart && (
          <button type="button" onClick={onRestart}>
            {state.status === 'failed' ? 'Try again' : 'Reconnect'}
          </button>
        )}
      </p>
      {state.lines.length === 0 ? (
        state.status === 'ended' && <p className="muted">{emptyText}</p>
      ) : (
        // Focusable so it can be scrolled from the keyboard.
        <pre className="log" ref={ref} onScroll={onScroll} tabIndex={0} aria-label={label}>
          {state.lines.map((l, i) => (
            <span key={i} className={`log-${l.stream}`} title={l.at}>
              {l.line}
              {'\n'}
            </span>
          ))}
        </pre>
      )}
    </div>
  )
}

function statusText(state: LogStreamState, endedText: string): string {
  switch (state.status) {
    case 'connecting':
      return 'Connecting…'
    case 'open':
      return state.lines.length === 0 ? 'Waiting for output…' : 'Live'
    case 'reconnecting':
      return 'Connection lost; reconnecting…'
    case 'ended':
      return endedText
    case 'failed':
      return "Couldn't load the log."
  }
}
