// A notice that outlives the page that raised it: deleting an app moves on to
// the Apps page, which shows what was left behind. Kept in memory until it's
// dismissed (not on a reload: it's about something just done).
export type Flash = { title: string; lines: string[]; commands: string[] }

let current: Flash | null = null

export function setFlash(f: Flash) {
  current = f
}

export function readFlash(): Flash | null {
  return current
}

export function clearFlash() {
  current = null
}
