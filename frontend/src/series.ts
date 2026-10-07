import type { Series } from './api'

// Pure helpers for Sparkline, kept out of the component file so React's fast
// refresh keeps working (a component file should only export components).

// The SVG is drawn in this coordinate space and stretched to whatever size
// CSS gives it (preserveAspectRatio="none"); the stroke stays the same width
// thanks to vector-effect, so the same paths work in a table cell or a tile.
export const WIDTH = 100
export const HEIGHT = 24
// Keeps the line's stroke inside the box at 0 and at the top of the scale.
const PAD = 2

// sparklinePath turns a series into an SVG path on a fixed 0–max scale,
// oldest value on the left, newest at the right edge. Each run of non-null
// values is its own subpath, so a gap (null) breaks the line instead of
// dropping to 0. A run of one value is drawn as a dot. Returns '' if there's
// nothing to draw.
//
// The scale is fixed rather than fitted to the data, so a container idling
// between 1% and 2% CPU draws a flat line near the bottom, not a dramatic
// zigzag from top to bottom.
export function sparklinePath(values: Series, max: number): string {
  const n = values.length
  const x = (i: number) => (n === 1 ? WIDTH : (i / (n - 1)) * WIDTH)
  const y = (v: number) => {
    const ratio = max > 0 ? Math.min(Math.max(v / max, 0), 1) : 0
    return PAD + (1 - ratio) * (HEIGHT - 2 * PAD)
  }

  let d = ''
  let runStart = true
  values.forEach((v, i) => {
    if (v === null) {
      runStart = true
      return
    }
    const point = `${round(x(i))} ${round(y(v))}`
    // A lone point is a zero-length line, which the round line cap draws as
    // a dot; "h0" is overwritten if the run continues.
    const lone = i === n - 1 || values[i + 1] === null
    if (runStart) d += `M${point}${lone ? 'h0' : ''}`
    else d += `L${point}`
    runStart = false
  })
  return d
}

// round keeps the generated paths short (24h is 1,440 points); a hundredth
// of a unit is far below a pixel at these sizes.
function round(n: number): number {
  return Math.round(n * 100) / 100
}

// seriesStats is the average and peak of a series' non-null values, or null
// if it has none. peaks (each step's maximum) gives a truer peak than the
// averages when it's available.
export function seriesStats(values: Series, peaks?: Series): { avg: number; max: number } | null {
  const present = values.filter((v): v is number => v !== null)
  if (present.length === 0) return null
  const peakValues = (peaks ?? values).filter((v): v is number => v !== null)
  return {
    avg: present.reduce((a, b) => a + b, 0) / present.length,
    max: Math.max(...peakValues, ...present),
  }
}

// seriesMax is the largest non-null value, or 0 if there are none: a scale
// for series with no natural maximum.
export function seriesMax(values: Series): number {
  return values.reduce<number>((m, v) => (v !== null && v > m ? v : m), 0)
}
