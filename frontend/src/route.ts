import { useEffect, useState } from 'react'

// The page to show, from the URL's hash: #/ (containers), #/apps, #/apps/3.
//
// Hash routing, not paths: the part after # never reaches the server, so the
// Go file server needs no fallback to index.html for /apps/3, and reloading
// or bookmarking any page just works. With three pages, a router library
// would be more code than this.
export type Route = { page: 'containers' } | { page: 'apps' } | { page: 'app'; id: number } | { page: 'not-found' }

export function parseRoute(hash: string): Route {
  const path = hash.replace(/^#/, '').replace(/\/+$/, '')
  if (path === '') return { page: 'containers' }
  if (path === '/apps') return { page: 'apps' }
  const m = /^\/apps\/([1-9][0-9]*)$/.exec(path)
  if (m) return { page: 'app', id: Number(m[1]) }
  return { page: 'not-found' }
}

export function appHref(id: number): string {
  return `#/apps/${id}`
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
