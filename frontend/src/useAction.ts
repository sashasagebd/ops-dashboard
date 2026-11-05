import { useState } from 'react'

// ActionState is a button's request: idle, in flight, or failed with the
// platform's message.
export type ActionState = { status: 'idle' } | { status: 'busy' } | { status: 'error'; message: string }

// useAction runs one change at a time (a click, a form), tracking whether
// it's in flight and why it failed.
export function useAction() {
  const [state, setState] = useState<ActionState>({ status: 'idle' })
  const run = async (fn: () => Promise<void>) => {
    setState({ status: 'busy' })
    try {
      await fn()
      setState({ status: 'idle' })
    } catch (err: unknown) {
      setState({ status: 'error', message: err instanceof Error ? err.message : String(err) })
    }
  }
  return [state, run] as const
}
