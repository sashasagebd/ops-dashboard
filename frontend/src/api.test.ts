import { afterEach, describe, expect, it, vi } from 'vitest'
import { fetchHistory, type HistoryResponse } from './api'

// stubFetch answers every call with status and body, using just the parts of
// a Response the fetchers read.
function stubFetch(status: number, body: unknown) {
  const fetchMock = vi.fn((_url: string, _init?: RequestInit) =>
    Promise.resolve({
      ok: status < 400,
      status,
      statusText: 'Bad Request',
      json: () => Promise.resolve(body),
    } as Response),
  )
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('fetchHistory', () => {
  it('asks for the window and returns the body', async () => {
    const body: HistoryResponse = {
      start: '2026-10-06T12:00:00Z',
      stepSeconds: 60,
      host: { cpuPercent: [null, 12.35], cpuPercentMax: [null, 80], memoryBytes: [1, null], diskUsedBytes: [5, 5] },
      containers: { mc: { cpuPercent: [1.5, null], cpuPercentMax: [3, null], memoryBytes: [100, 200] } },
    }
    const fetchMock = stubFetch(200, body)
    const controller = new AbortController()

    await expect(fetchHistory('24h', controller.signal)).resolves.toEqual(body)

    expect(fetchMock).toHaveBeenCalledWith('/api/history?window=24h', { signal: controller.signal })
  })

  it("throws the API's error message", async () => {
    stubFetch(400, { error: 'window must be 1h or 24h' })

    await expect(fetchHistory('1h')).rejects.toThrow('window must be 1h or 24h')
  })
})
