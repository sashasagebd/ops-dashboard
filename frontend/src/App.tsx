import { useEffect, useState } from 'react'
import { fetchContainers, type Container } from './api'

// One union instead of separate loading/error/data flags, so impossible
// combinations (e.g. loading *and* an error) can't be represented.
type LoadState =
  | { status: 'loading' }
  | { status: 'error'; message: string }
  | { status: 'ok'; containers: Container[] }

function App() {
  const [state, setState] = useState<LoadState>({ status: 'loading' })

  useEffect(() => {
    // Aborting on cleanup cancels the request if the component unmounts. In
    // development, StrictMode mounts twice on purpose, so this also stops the
    // first request from updating state after it's been thrown away.
    const controller = new AbortController()
    fetchContainers(controller.signal)
      .then((containers) => setState({ status: 'ok', containers }))
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
        return <p>No running containers.</p>
      }
      return (
        <table>
          <thead>
            <tr>
              <th scope="col">Name</th>
              <th scope="col">Image</th>
              <th scope="col">State</th>
              <th scope="col">Status</th>
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
                <td>{c.status}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )
  }
}

export default App
