import { useEffect, useState } from 'react'

// The tabs of an app's page, in the order they're shown.
export const APP_TABS = ['overview', 'deployments', 'logs', 'database', 'settings'] as const
export type AppTab = (typeof APP_TABS)[number]

// The page to show, from the URL's hash: #/ (overview), #/containers,
// #/apps, #/apps/3 and #/apps/3/<tab>.
//
// Hash routing, not paths: the part after # never reaches the server, so the
// Go file server needs no fallback to index.html for /apps/3, and reloading
// or bookmarking any page (and an app's tab) just works. With this few pages,
// a router library would be more code than this.
export type Route =
  | { page: 'overview' }
  | { page: 'containers' }
  | { page: 'apps' }
  | { page: 'app'; id: number; tab: AppTab }
  | { page: 'not-found' }

export function parseRoute(hash: string): Route {
  const path = hash.replace(/^#/, '').replace(/\/+$/, '')
  if (path === '') return { page: 'overview' }
  if (path === '/containers') return { page: 'containers' }
  if (path === '/apps') return { page: 'apps' }
  const m = /^\/apps\/([1-9][0-9]*)(?:\/([a-z]+))?$/.exec(path)
  if (m) {
    const tab = m[2] ?? 'overview'
    if (isAppTab(tab)) return { page: 'app', id: Number(m[1]), tab }
  }
  return { page: 'not-found' }
}

function isAppTab(s: string): s is AppTab {
  return (APP_TABS as readonly string[]).includes(s)
}

// appHref links to an app's page, on one of its tabs (the overview by
// default, which has the shorter URL).
export function appHref(id: number, tab: AppTab = 'overview'): string {
  return tab === 'overview' ? `#/apps/${id}` : `#/apps/${id}/${tab}`
}

// useRoute returns the current route and re-renders when the hash changes
// (links, the back button, or code setting location.hash).
export function useRoute(): Route {
  const [route, setRoute] = useState(() => parseRoute(window.location.hash))
  useEffect(() => {
    const onChange = () => setRoute(parseRoute(window.location.hash))
    window.addEventListener('hashchange', onChange)
    return () => window.removeEventListener('hashchange', onChange)
  }, [])
  return route
}
