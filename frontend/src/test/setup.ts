// Runs before every test file (see test.setupFiles in vite.config.ts).
import '@testing-library/jest-dom/vitest'
import { cleanup } from '@testing-library/react'
import { afterEach } from 'vitest'

// Testing Library only cleans up automatically when Vitest globals are on;
// they're off here so tests import what they use explicitly.
afterEach(() => {
  cleanup()
})
