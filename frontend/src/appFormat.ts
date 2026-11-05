// How the platform's states are shown: badge colours and labels, shared by
// the Overview, Apps and app pages. Badge classes: badge-up (good),
// badge-warn (in progress, or fixed), badge-down (failed), badge-neutral.
import type { Backup, DeploymentStatus, EventKind, PlatformApp } from './api'

export const STATUS_BADGE: Record<DeploymentStatus, string> = {
  queued: 'warn',
  building: 'warn',
  starting: 'warn',
  healthy: 'up',
  failed: 'down',
  superseded: 'neutral',
  rolled_back: 'neutral',
}

// How each kind of reconciler event looks: what was fixed is a warning (the
// app was down, briefly); housekeeping is neutral; what keeps going wrong is
// a fault.
export const EVENT_BADGE: Record<EventKind, string> = {
  restarted: 'warn',
  recreated: 'warn',
  crash_looping: 'down',
  removed_orphan: 'neutral',
  routes_reapplied: 'neutral',
  fix_failed: 'down',
  database_created: 'up',
}

export const EVENT_LABEL: Record<EventKind, string> = {
  restarted: 'restarted',
  recreated: 'recreated',
  crash_looping: 'crash-looping',
  removed_orphan: 'cleaned up',
  routes_reapplied: 'routes fixed',
  fix_failed: 'fix failed',
  database_created: 'database created',
}

export const BACKUP_BADGE: Record<Backup['status'], string> = {
  queued: 'warn',
  running: 'warn',
  done: 'up',
  failed: 'down',
}

// How a database's state looks; anything else (stopped, missing) is a
// warning, since the reconciler is bringing it back.
export const DATABASE_BADGE: Record<string, string> = {
  running: 'up',
  unknown: 'neutral',
}

export const IN_PROGRESS: ReadonlySet<DeploymentStatus> = new Set(['queued', 'building', 'starting'])

export function shortSHA(sha: string): string {
  return sha.slice(0, 7)
}

// appState is an app's headline status, for its badge and sidebar dot: live
// if something is, else whether it has never been deployed.
export function appState(app: PlatformApp): { label: string; badge: string } {
  return app.currentDeployment === null ? { label: 'Not deployed', badge: 'neutral' } : { label: 'Live', badge: 'up' }
}
