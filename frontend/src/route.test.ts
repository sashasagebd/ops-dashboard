import { describe, expect, it } from 'vitest'
import { appHref, parseRoute } from './route'

describe('parseRoute', () => {
  it.each([
    ['', { page: 'overview' }],
    ['#', { page: 'overview' }],
    ['#/', { page: 'overview' }],
    ['#/containers', { page: 'containers' }],
    ['#/apps', { page: 'apps' }],
    ['#/apps/', { page: 'apps' }],
    ['#/apps/12', { page: 'app', id: 12, tab: 'overview' }],
    ['#/apps/12/', { page: 'app', id: 12, tab: 'overview' }],
    ['#/apps/12/overview', { page: 'app', id: 12, tab: 'overview' }],
    ['#/apps/12/deployments', { page: 'app', id: 12, tab: 'deployments' }],
    ['#/apps/12/settings', { page: 'app', id: 12, tab: 'settings' }],
    ['#/apps/12/nope', { page: 'not-found' }],
    ['#/apps/0', { page: 'not-found' }],
    ['#/apps/3x', { page: 'not-found' }],
    ['#/apps/3/logs/extra', { page: 'not-found' }],
    ['#/nope', { page: 'not-found' }],
  ])('%j', (hash, want) => {
    expect(parseRoute(hash)).toEqual(want)
  })
})

describe('appHref', () => {
  it('leaves the overview tab out', () => {
    expect(appHref(3)).toBe('#/apps/3')
    expect(appHref(3, 'overview')).toBe('#/apps/3')
    expect(appHref(3, 'logs')).toBe('#/apps/3/logs')
  })
})
