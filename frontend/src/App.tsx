import { useEffect, useState } from 'react'
import { fetchContainers, type Container } from './api'
import { uptimeText } from './format'

// One union instead of separate loading/error/data flags, so impossible
// combinations (e.g. loading *and* an error) can't be represented.
type LoadState =
  | { status: 'loading' }
  | { status: 'error'; message: string }
  // loadedAt is the "now" uptimes are measured against. It's captured when
  // the data arrives rather than read during render, which keeps rendering
  // pure and every row consistent with the others.
  | { status: 'ok'; containers: Container[]; loadedAt: number }

function App() {
  const [state, setState] = useState<LoadState>({ status: 'loading' })

  useEffect(() => {
    // Aborting on cleanup cancels the request if the component unmounts. In
    // development, StrictMode mounts twice on purpose, so this also stops the
    // first request from updating state after it's been thrown away.
    const controller = new AbortController()
    fetchContainers(controller.signal)
      .then((containers) => setState({ status: 'ok', containers, loadedAt: Date.now() }))
      .catch((err: unknown) => {
        if (controller.signal.aborted) return
        setState({ status: 'error', message: err instanceof Error ? err.message : String(err) })
      })
    return () => controller.abort()
  }, [])

  return (
    <main>
      <h1>Containers</h1>
      <ContainerList state={state} />
    </main>
  )
}

function ContainerList({ state }: { state: LoadState }) {
  switch (state.status) {
    case 'loading':
      return <p>Loading…</p>
    case 'error':
      return <p role="alert">Couldn't load containers: {state.message}</p>
    case 'ok':
      if (state.containers.length === 0) {
        return <p>No containers.</p>
      }
      return (
        <table>
          <thead>
            <tr>
              <th scope="col">Name</th>
              <th scope="col">Image</th>
              <th scope="col">State</th>
              <th scope="col">Uptime</th>
            </tr>
          </thead>
          <tbody>
            {state.containers.map((c) => (
              <tr key={c.id}>
                <td>{c.name}</td>
                <td className="mono">{c.image}</td>
                <td>
                  <span className={`badge badge-${c.state === 'running' ? 'up' : 'down'}`}>{c.state}</span>
                </td>
                {/* Docker's own status text stays available on hover: it
                    includes health ("(healthy)") and exit codes. */}
                <td title={c.status}>{uptimeText(c, state.loadedAt)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )
  }
}

export default App
