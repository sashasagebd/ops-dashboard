import type { ReactNode } from 'react'
import type { HostHistory, HostStats } from './api'
import { formatBytes, formatCPU, usagePercent } from './format'
import { Sparkline } from './Sparkline'

// Usage bars turn amber, then red, so a filling disk or memory stands out at
// a glance. Same thresholds for all three; tune here if one gets noisy.
const WARN_PERCENT = 80
const HIGH_PERCENT = 90

type HostSummaryProps = {
  host: HostStats | null
  history: HostHistory | null // null until it has loaded, or if it can't
  windowLabel: string // e.g. "last hour", for the trend lines' names
}

// HostSummary shows the whole server's CPU, memory and disk as three tiles,
// each with a trend line on the same scale as its usage bar.
export function HostSummary({ host, history, windowLabel }: HostSummaryProps) {
  if (host === null) {
    return <p className="muted">Server stats unavailable.</p>
  }

  // Disk % is used / (used + available), like df's "Use%". Blocks reserved
  // for root are in neither, so used / total would read a few percent low
  // and wouldn't reach 100% when the disk is actually full.
  const diskUsable = host.diskUsedBytes + host.diskAvailableBytes
  const diskPercent = usagePercent(host.diskUsedBytes, diskUsable)

  return (
    <div className="tiles">
      <Tile
        label="CPU"
        value={formatCPU(host.cpuPercent)}
        detail="all cores"
        percent={host.cpuPercent}
        trend={
          history && (
            <Sparkline
              className="tile-trend"
              values={history.cpuPercent}
              peaks={history.cpuPercentMax}
              max={100}
              label={`Server CPU, ${windowLabel}`}
              format={formatCPU}
            />
          )
        }
      />
      <Tile
        label="Memory"
        value={formatBytes(host.memoryBytes)}
        detail={`of ${formatBytes(host.memoryTotalBytes)}`}
        percent={usagePercent(host.memoryBytes, host.memoryTotalBytes)}
        trend={
          history && (
            <Sparkline
              className="tile-trend"
              values={history.memoryBytes}
              max={host.memoryTotalBytes}
              label={`Server memory, ${windowLabel}`}
              format={formatBytes}
            />
          )
        }
      />
      <Tile
        label="Disk"
        value={formatBytes(host.diskUsedBytes)}
        detail={`of ${formatBytes(host.diskTotalBytes)}`}
        percent={diskPercent}
        trend={
          history && (
            <Sparkline
              className="tile-trend"
              values={history.diskUsedBytes}
              max={diskUsable}
              label={`Server disk, ${windowLabel}`}
              format={formatBytes}
            />
          )
        }
      />
    </div>
  )
}

type TileProps = {
  label: string
  value: string
  detail: string
  percent: number | null
  trend: ReactNode // a Sparkline, or nothing while there's no history
}

// Tile is one stat with a usage bar. percent is null when there's no value
// yet (CPU on the first poll); the bar is then empty.
function Tile({ label, value, detail, percent, trend }: TileProps) {
  const pct = percent === null ? 0 : Math.min(Math.max(percent, 0), 100)
  const level = pct >= HIGH_PERCENT ? 'high' : pct >= WARN_PERCENT ? 'warn' : 'ok'

  return (
    <section className="tile" aria-label={label}>
      <h3 className="tile-label">{label}</h3>
      <p className="tile-value">{value}</p>
      <p className="tile-detail">{detail}</p>
      {trend}
      {/* role="meter" (not progressbar): it's a level, not task progress. */}
      <div
        className="bar"
        role="meter"
        aria-label={`${label} usage`}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={percent === null ? undefined : Math.round(pct)}
      >
        <div className={`bar-fill bar-${level}`} style={{ width: `${pct}%` }} />
      </div>
    </section>
  )
}
