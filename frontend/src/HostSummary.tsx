import type { HostStats } from './api'
import { formatBytes, formatCPU, usagePercent } from './format'

// Usage bars turn amber, then red, so a filling disk or memory stands out at
// a glance. Same thresholds for all three; tune here if one gets noisy.
const WARN_PERCENT = 80
const HIGH_PERCENT = 90

// HostSummary shows the whole server's CPU, memory and disk as three tiles.
export function HostSummary({ host }: { host: HostStats | null }) {
  if (host === null) {
    return <p className="muted">Server stats unavailable.</p>
  }

  // Disk % is used / (used + available), like df's "Use%". Blocks reserved
  // for root are in neither, so used / total would read a few percent low
  // and wouldn't reach 100% when the disk is actually full.
  const diskPercent = usagePercent(host.diskUsedBytes, host.diskUsedBytes + host.diskAvailableBytes)

  return (
    <div className="tiles">
      <Tile label="CPU" value={formatCPU(host.cpuPercent)} detail="all cores" percent={host.cpuPercent} />
      <Tile
        label="Memory"
        value={formatBytes(host.memoryBytes)}
        detail={`of ${formatBytes(host.memoryTotalBytes)}`}
        percent={usagePercent(host.memoryBytes, host.memoryTotalBytes)}
      />
      <Tile
        label="Disk"
        value={formatBytes(host.diskUsedBytes)}
        detail={`of ${formatBytes(host.diskTotalBytes)}`}
        percent={diskPercent}
      />
    </div>
  )
}

// Tile is one stat with a usage bar. percent is null when there's no value
// yet (CPU on the first poll); the bar is then empty.
function Tile({ label, value, detail, percent }: { label: string; value: string; detail: string; percent: number | null }) {
  const pct = percent === null ? 0 : Math.min(Math.max(percent, 0), 100)
  const level = pct >= HIGH_PERCENT ? 'high' : pct >= WARN_PERCENT ? 'warn' : 'ok'

  return (
    <section className="tile" aria-label={label}>
      <h3 className="tile-label">{label}</h3>
      <p className="tile-value">{value}</p>
      <p className="tile-detail">{detail}</p>
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
