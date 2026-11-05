// Shared building blocks for every page: the page header, cards, loading and
// error states, tabs. Styles in index.css under the same class names.
import { useRef, type KeyboardEvent, type ReactNode } from 'react'
import type { ActionState } from './useAction'
import type { PollState } from './usePolling'

// PageHeader is a page's title, an optional line under it, and its buttons.
export function PageHeader({
  title,
  subtitle,
  badges,
  actions,
  crumbs,
}: {
  title: string
  subtitle?: ReactNode
  badges?: ReactNode
  actions?: ReactNode
  crumbs?: ReactNode
}) {
  return (
    <header className="page-header">
      {crumbs && <div className="crumbs">{crumbs}</div>}
      <div className="page-header-row">
        <div className="page-header-text">
          <div className="page-title">
            <h1>{title}</h1>
            {badges}
          </div>
          {subtitle && <div className="page-subtitle">{subtitle}</div>}
        </div>
        {actions && <div className="page-actions">{actions}</div>}
      </div>
    </header>
  )
}

// Card is a titled panel. The title is an h2, so each card is a section a
// screen reader can jump to.
export function Card({
  title,
  action,
  children,
  className,
}: {
  title?: string
  action?: ReactNode
  children: ReactNode
  className?: string
}) {
  return (
    <section className={`card ${className ?? ''}`} aria-label={title}>
      {(title || action) && (
        <div className="card-head">
          {title && <h2>{title}</h2>}
          {action}
        </div>
      )}
      {children}
    </section>
  )
}

// Loaded shows a polled resource's loading and error states, and children
// once it has loaded. If a refresh fails later, the last data stays up
// under a banner.
export function Loaded<T>({
  state,
  what,
  children,
}: {
  state: PollState<T>
  what: string
  children: (data: T, checkedAt: number) => ReactNode
}) {
  switch (state.status) {
    case 'loading':
      return <p className="muted">Loading…</p>
    case 'error':
      return (
        <p role="alert" className="error-text">
          Couldn't load {what}: {state.message}
        </p>
      )
    case 'ok':
      return (
        <>
          {state.refreshError !== null && (
            <p role="status" className="banner">
              Couldn't refresh: {state.refreshError}. Showing the last data loaded.
            </p>
          )}
          {children(state.data, state.checkedAt)}
        </>
      )
  }
}

export function Field({ label, hint, children }: { label: string; hint: string; children: ReactNode }) {
  return (
    <label className="field">
      <span>{label}</span>
      {children}
      <span className="muted field-hint">{hint}</span>
    </label>
  )
}

export function ActionError({ state }: { state: ActionState }) {
  if (state.status !== 'error') return null
  return (
    <p role="alert" className="action-error">
      {state.message}
    </p>
  )
}

// Tabs is a tab bar whose selected tab comes from the URL: choosing one
// navigates to its href, so a reload or a shared link keeps it. It follows
// the ARIA tabs pattern: one tab stop, arrow keys (and Home/End) move
// between tabs and select them. The panel is TabPanel.
export function Tabs<T extends string>({
  label,
  tabs,
  current,
  hrefFor,
}: {
  label: string
  tabs: readonly { id: T; label: string }[]
  current: T
  hrefFor: (id: T) => string
}) {
  const refs = useRef(new Map<T, HTMLButtonElement>())
  const select = (id: T, focus: boolean) => {
    if (focus) refs.current.get(id)?.focus()
    window.location.assign(hrefFor(id)) // a hash: changes the route, no reload
  }
  const onKeyDown = (e: KeyboardEvent) => {
    const i = tabs.findIndex((t) => t.id === current)
    const next = { ArrowRight: i + 1, ArrowLeft: i - 1, Home: 0, End: tabs.length - 1 }[e.key]
    if (next === undefined) return
    e.preventDefault()
    select(tabs[(next + tabs.length) % tabs.length].id, true)
  }
  return (
    <div className="tabs" role="tablist" aria-label={label} onKeyDown={onKeyDown}>
      {tabs.map((t) => (
        <button
          key={t.id}
          ref={(el) => {
            if (el) refs.current.set(t.id, el)
            else refs.current.delete(t.id)
          }}
          type="button"
          role="tab"
          id={`tab-${t.id}`}
          aria-selected={t.id === current}
          aria-controls="tabpanel"
          tabIndex={t.id === current ? 0 : -1}
          className="tab"
          onClick={() => select(t.id, false)}
        >
          {t.label}
        </button>
      ))}
    </div>
  )
}

export function TabPanel({ current, children }: { current: string; children: ReactNode }) {
  return (
    <div role="tabpanel" id="tabpanel" aria-labelledby={`tab-${current}`} className="tabpanel">
      {children}
    </div>
  )
}
