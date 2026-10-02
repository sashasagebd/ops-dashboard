import { describe, expect, it } from 'vitest'
import type { Container } from './api'
import { formatDuration, uptimeText } from './format'

const SECOND = 1000
const MINUTE = 60 * SECOND
const HOUR = 60 * MINUTE
const DAY = 24 * HOUR

describe('formatDuration', () => {
  it.each([
    [-5 * SECOND, '<1m'],
    [0, '<1m'],
    [59 * SECOND, '<1m'],
    [MINUTE, '1m'],
    [59 * MINUTE + 59 * SECOND, '59m'],
    [HOUR, '1h 0m'],
    [3 * HOUR + 12 * MINUTE, '3h 12m'],
    [DAY - 1, '23h 59m'],
    [DAY, '1d 0h'],
    [2 * DAY + 4 * HOUR + 30 * MINUTE, '2d 4h'],
  ])('%i ms is %s', (ms, want) => {
    expect(formatDuration(ms)).toBe(want)
  })
})

describe('uptimeText', () => {
  const now = Date.parse('2026-10-01T12:00:00Z')
  const base: Container = {
    id: 'a1',
    name: 'mc',
    image: 'itzg/minecraft-server',
    state: 'running',
    status: 'Up 3 hours',
    startedAt: null,
    finishedAt: null,
  }

  it('counts a running container from when it started', () => {
    const c = { ...base, startedAt: '2026-10-01T08:48:00Z', finishedAt: '2026-09-30T00:00:00Z' }
    expect(uptimeText(c, now)).toBe('up 3h 12m')
  })

  it('counts a stopped container from when it stopped', () => {
    const c = { ...base, state: 'exited', startedAt: '2026-09-29T00:00:00Z', finishedAt: '2026-10-01T11:40:00Z' }
    expect(uptimeText(c, now)).toBe('down 20m')
  })

  it('shows a dash when Docker has no time', () => {
    expect(uptimeText({ ...base, state: 'created' }, now)).toBe('—')
    expect(uptimeText({ ...base, startedAt: 'not a date' }, now)).toBe('—')
  })
})
