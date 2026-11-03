import { describe, expect, it } from 'vitest'
import { parseRoute } from './route'

describe('parseRoute', () => {
  it.each([
    ['', { page: 'containers' }],
    ['#', { page: 'containers' }],
    ['#/', { page: 'containers' }],
    ['#/apps', { page: 'apps' }],
    ['#/apps/', { page: 'apps' }],
    ['#/apps/12', { page: 'app', id: 12 }],
    ['#/apps/0', { page: 'not-found' }],
    ['#/apps/3x', { page: 'not-found' }],
    ['#/apps/3/extra', { page: 'not-found' }],
    ['#/nope', { page: 'not-found' }],
  ])('%j', (hash, want) => {
    expect(parseRoute(hash)).toEqual(want)
  })
})
