import type { Container } from './api'

const MINUTE = 60_000
const HOUR = 60 * MINUTE
const DAY = 24 * HOUR

// formatDuration shows at most two units ("3h 12m", "2d 4h"): enough to
// tell at a glance, without seconds ticking past. Negative durations (the
// browser's clock slightly behind the server's) show as "<1m".
export function formatDuration(ms: number): string {
  if (ms < MINUTE) return '<1m'
  if (ms < HOUR) return `${Math.floor(ms / MINUTE)}m`
  if (ms < DAY) return `${Math.floor(ms / HOUR)}h ${Math.floor((ms % HOUR) / MINUTE)}m`
  return `${Math.floor(ms / DAY)}d ${Math.floor((ms % DAY) / HOUR)}h`
}

// uptimeText describes how long a container has been up or down, as of now
// (milliseconds since the epoch). "—" means Docker has no time for it, e.g. a
// container that was created but never started.
export function uptimeText(c: Container, now: number): string {
  const up = c.state === 'running'
  const since = up ? c.startedAt : c.finishedAt
  const ms = since === null ? NaN : now - Date.parse(since)
  if (Number.isNaN(ms)) return '—'
  return `${up ? 'up' : 'down'} ${formatDuration(ms)}`
}

// formatCPU shows one decimal place. Tiny non-zero values show as "<0.1%"
// rather than "0.0%", so a container that's barely busy doesn't look idle.
// null (stopped, or no second sample yet) shows as "—".
export function formatCPU(pct: number | null): string {
  if (pct === null) return '—'
  if (pct > 0 && pct < 0.1) return '<0.1%'
  return `${pct.toFixed(1)}%`
}

const MiB = 1024 * 1024
const GiB = 1024 * MiB

// formatBytes uses binary units (MiB, GiB) like `docker stats`: whole MiB
// below 1 GiB, one decimal place above. null shows as "—".
export function formatBytes(bytes: number | null): string {
  if (bytes === null) return '—'
  if (bytes < MiB) return '<1 MiB'
  if (bytes < GiB) return `${Math.round(bytes / MiB)} MiB`
  return `${(bytes / GiB).toFixed(1)} GiB`
}
