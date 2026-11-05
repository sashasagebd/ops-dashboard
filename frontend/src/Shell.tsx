import { useState, type ReactNode } from 'react'
import { fetchApps } from './api'
import { appState } from './appFormat'
import { appHref, type Route } from './route'
import { applyTheme, storedTheme, type ThemeChoice } from './theme'
import { usePolling } from './usePolling'

// The sidebar's app list and platform status don't need to be fresher than
// the Apps page's.
const APPS_REFRESH_MS = 10_000

// Shell is the frame around every page: a sidebar with the pages, the apps
// and the theme switch on a wide screen; on a phone, a top bar whose menu
// button shows the same sidebar over the page.
export function Shell({ route, children }: { route: Route; children: ReactNode }) {
  const [menuOpen, setMenuOpen] = useState(false)
  const apps = usePolling(fetchApps, APPS_REFRESH_MS)
  const close = () => setMenuOpen(false)
  const current = (page: Route['page']) => (route.page === page ? 'page' : undefined)

  return (
    <div className="shell">
      <header className="topbar">
        <Brand />
        <button
          type="button"
          className="icon-button"
          aria-label="Menu"
          aria-expanded={menuOpen}
          aria-controls="sidebar"
          onClick={() => setMenuOpen((o) => !o)}
        >
          <svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" aria-hidden="true">
            <path d="M4 7h16M4 12h16M4 17h16" />
          </svg>
        </button>
      </header>

      <nav id="sidebar" className={menuOpen ? 'sidebar open' : 'sidebar'} aria-label="Main">
        <div className="sidebar-brand">
          <Brand />
        </div>
        <div className="nav-group">
          <a className="nav-link" href="#/" aria-current={current('overview')} onClick={close}>
            <Icon d="M3 3h7v7H3zM14 3h7v7h-7zM3 14h7v7H3zM14 14h7v7h-7z" />
            Overview
          </a>
          <a className="nav-link" href="#/containers" aria-current={current('containers')} onClick={close}>
            <Icon d="M21 8 12 3 3 8v8l9 5 9-5zM3 8l9 5 9-5M12 13v8" />
            Containers
          </a>
          <a
            className="nav-link"
            href="#/apps"
            aria-current={route.page === 'apps' || route.page === 'app' ? 'page' : undefined}
            onClick={close}
          >
            <Icon d="M4 7h16M4 12h16M4 17h10" />
            Apps
          </a>
        </div>
        {apps.status === 'ok' && apps.data.length > 0 && (
          <div className="nav-group">
            <div className="nav-label" id="nav-apps">
              Your apps
            </div>
            <ul aria-labelledby="nav-apps">
              {apps.data.map((a) => (
                <li key={a.id}>
                  <a
                    className="nav-link"
                    href={appHref(a.id)}
                    aria-current={route.page === 'app' && route.id === a.id ? 'page' : undefined}
                    onClick={close}
                  >
                    <span className={`dot dot-${appState(a).badge}`} aria-hidden="true" />
                    {a.name}
                  </a>
                </li>
              ))}
            </ul>
          </div>
        )}
        <div className="sidebar-foot">
          <ThemeSwitch />
          {/* Not a live region: it would be announced on every poll, and
              pages have role="status" banners of their own. */}
          <p className="platform-status">
            {apps.status === 'loading' ? (
              'Checking the platform…'
            ) : apps.status === 'error' || (apps.status === 'ok' && apps.refreshError !== null) ? (
              <>
                <span className="dot dot-down" aria-hidden="true" />
                Platform unreachable
              </>
            ) : (
              <>
                <span className="dot dot-up" aria-hidden="true" />
                Platform reachable
              </>
            )}
          </p>
        </div>
      </nav>
      {menuOpen && <div className="scrim" onClick={close} aria-hidden="true" />}

      <main className="content">{children}</main>
    </div>
  )
}

function Brand() {
  return (
    <a className="brand" href="#/">
      <span className="brand-mark" aria-hidden="true">
        ops
      </span>
      <span>
        <span className="brand-name">Ops Console</span>
        <span className="brand-sub">homelab</span>
      </span>
    </a>
  )
}

function Icon({ d }: { d: string }) {
  return (
    <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" aria-hidden="true">
      <path d={d} />
    </svg>
  )
}

const THEMES: { id: ThemeChoice; label: string }[] = [
  { id: 'system', label: 'System' },
  { id: 'light', label: 'Light' },
  { id: 'dark', label: 'Dark' },
]

// ThemeSwitch picks the colour theme: the system's, or light or dark
// regardless of it; remembered per browser (theme.ts).
function ThemeSwitch() {
  const [theme, setTheme] = useState(storedTheme)
  const choose = (t: ThemeChoice) => {
    applyTheme(t)
    setTheme(t)
  }
  return (
    <div className="segmented" role="group" aria-label="Theme">
      {THEMES.map((t) => (
        <button key={t.id} type="button" aria-pressed={theme === t.id} onClick={() => choose(t.id)}>
          {t.label}
        </button>
      ))}
    </div>
  )
}
