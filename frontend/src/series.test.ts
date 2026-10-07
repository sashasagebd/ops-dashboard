import { describe, expect, it } from 'vitest'
import { seriesMax, seriesStats, sparklinePath } from './series'

// The drawing space is 100 × 24 with 2 units of padding: a value of 0 sits
// at y = 22 and the top of the scale at y = 2.
describe('sparklinePath', () => {
  it.each([
    { name: 'flat line', values: [50, 50, 50], max: 100, want: 'M0 12L50 12L100 12' },
    { name: 'rising line', values: [0, 100], max: 100, want: 'M0 22L100 2' },
    { name: 'gap splits the line', values: [0, 0, null, 100, 100], max: 100, want: 'M0 22L25 22M75 2L100 2' },
    { name: 'lone value is a dot', values: [null, 50, null], max: 100, want: 'M50 12h0' },
    { name: 'single value sits at the right edge', values: [50], max: 100, want: 'M100 12h0' },
    { name: 'values outside the scale are clamped', values: [-5, 150], max: 100, want: 'M0 22L100 2' },
    { name: 'zero scale draws at the bottom', values: [3, 3], max: 0, want: 'M0 22L100 22' },
    { name: 'all gaps draw nothing', values: [null, null], max: 100, want: '' },
    { name: 'empty series draws nothing', values: [], max: 100, want: '' },
  ])('$name', ({ values, max, want }) => {
    expect(sparklinePath(values, max)).toBe(want)
  })
})

describe('seriesStats', () => {
  it('averages the values and takes the peak from peaks', () => {
    expect(seriesStats([10, null, 20], [15, null, 90])).toEqual({ avg: 15, max: 90 })
  })

  it('uses the values for the peak without peaks', () => {
    expect(seriesStats([10, 30])).toEqual({ avg: 20, max: 30 })
  })

  it('is null with no values', () => {
    expect(seriesStats([null, null], [null, null])).toBeNull()
  })
})

describe('seriesMax', () => {
  it('is the largest value, ignoring gaps', () => {
    expect(seriesMax([3, null, 7, 5])).toBe(7)
  })

  it('is 0 with no values', () => {
    expect(seriesMax([null])).toBe(0)
  })
})
