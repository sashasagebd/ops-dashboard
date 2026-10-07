import type { Series } from './api'
import { HEIGHT, seriesStats, sparklinePath, WIDTH } from './series'

type SparklineProps = {
  values: Series // step averages: the main line
  peaks?: Series // step maxima: drawn faintly behind, so short spikes still show
  max: number // top of the y-scale, e.g. 100 for CPU %, total bytes for memory
  label: string // what it shows, e.g. "mc CPU, last hour"
  format: (v: number) => string // formats the average and peak for the label
  className?: string
}

// Sparkline is a small trend line with no axes. Its accessible name carries
// the numbers a screen reader can't get from the drawing.
export function Sparkline({ values, peaks, max, label, format, className }: SparklineProps) {
  const stats = seriesStats(values, peaks)
  const name = stats === null ? `${label}: no data yet` : `${label}: average ${format(stats.avg)}, peak ${format(stats.max)}`

  return (
    <svg
      className={`sparkline ${className ?? ''}`}
      viewBox={`0 0 ${WIDTH} ${HEIGHT}`}
      preserveAspectRatio="none"
      role="img"
      aria-label={name}
    >
      <title>{name}</title>
      {peaks && <path className="sparkline-peaks" d={sparklinePath(peaks, max)} vectorEffect="non-scaling-stroke" />}
      <path className="sparkline-line" d={sparklinePath(values, max)} vectorEffect="non-scaling-stroke" />
    </svg>
  )
}
