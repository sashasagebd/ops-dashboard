// The colour theme: the system's (the default), or light or dark whatever
// the system says. The choice is per browser, in localStorage; index.html
// applies a stored one before the page is drawn, so it doesn't flash light
// first. CSS reads it from <html data-theme>.
export type ThemeChoice = 'system' | 'light' | 'dark'

const KEY = 'ops-theme' // also in index.html's inline script

export function storedTheme(): ThemeChoice {
  try {
    const v = localStorage.getItem(KEY)
    if (v === 'light' || v === 'dark') return v
  } catch {
    // Storage can be blocked (private windows, site settings): the system's.
  }
  return 'system'
}

export function applyTheme(choice: ThemeChoice) {
  const root = document.documentElement
  if (choice === 'system') delete root.dataset.theme
  else root.dataset.theme = choice
  try {
    if (choice === 'system') localStorage.removeItem(KEY)
    else localStorage.setItem(KEY, choice)
  } catch {
    // Not remembered, but still applied for this page.
  }
}
