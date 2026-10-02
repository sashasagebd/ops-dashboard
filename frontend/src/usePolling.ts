import { useEffect, useState } from 'react'

// One union instead of separate loading/error/data flags, so impossible
// combinations (e.g. loading *and* an error) can't be represented.
export type PollState<T> =
  | { status: 'loading' }
  // No data has ever loaded; polling keeps retrying.
  | { status: 'error'; message: string }
  | {
      status: 'ok'
      data: T // from the last successful fetch
      checkedAt: number // ms since the epoch of the latest attempt, successful or not
      refreshError: string | null // set if the latest attempt failed; data is then older
    }

// usePolling calls fetcher now and then again intervalMs after each response.
//
// - The next fetch is scheduled after the previous one finishes, not on a
//   fixed clock, so a slow response never overlaps the next request.
// - While the tab is hidden it stops polling and fetches again as soon as the
//   tab is visible, so a forgotten tab doesn't poll all day.
// - If a refresh fails after data has loaded, the old data stays on screen
//   with refreshError set, rather than being replaced by an error.
//
// fetcher must be stable (e.g. a module-level function); a new function on
// every render would restart polling each time.
export function usePolling<T>(fetcher: (signal: AbortSignal) => Promise<T>, intervalMs: number): PollState<T> {
  const [state, setState] = useState<PollState<T>>({ status: 'loading' })

  useEffect(() => {
    let controller: AbortController | undefined
    let timer: ReturnType<typeof setTimeout> | undefined

    // Aborting cancels the request on unmount or when the tab is hidden. In
    // development, StrictMode mounts twice on purpose, so this also stops the
    // first effect's request from updating state after it's been thrown away.
    const stop = () => {
      clearTimeout(timer)
      controller?.abort()
    }

    const poll = async () => {
      stop()
      const c = new AbortController()
      controller = c
      try {
        const data = await fetcher(c.signal)
        if (c.signal.aborted) return
        setState({ status: 'ok', data, checkedAt: Date.now(), refreshError: null })
      } catch (err: unknown) {
        if (c.signal.aborted) return
        const message = err instanceof Error ? err.message : String(err)
        setState((prev) =>
          prev.status === 'ok'
            ? { ...prev, checkedAt: Date.now(), refreshError: message }
            : { status: 'error', message },
        )
      }
      if (!document.hidden) timer = setTimeout(() => void poll(), intervalMs)
    }

    const onVisibilityChange = () => {
      if (document.hidden) stop()
      else void poll()
    }

    document.addEventListener('visibilitychange', onVisibilityChange)
    void poll()
    return () => {
      document.removeEventListener('visibilitychange', onVisibilityChange)
      stop()
    }
  }, [fetcher, intervalMs])

  return state
}
