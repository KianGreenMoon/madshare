import { defineConfig, devices } from '@playwright/test';

// ── Environment ──────────────────────────────────────────────────────────────
// Node 22 reads a .env file natively, so we need no `dotenv` dependency.
// The file is optional: if it's absent we fall back to real env vars / defaults.
try {
  process.loadEnvFile('.env');
} catch {
  /* no .env present — that's fine */
}

// One place that decides "which server do we point at?" — overridable per env,
// exactly like your k6 suite. Tests never hardcode a URL.
const BASE_URL = process.env.BASE_URL ?? 'http://localhost:3000';

export default defineConfig({
  // Where the *.spec.ts files live.
  testDir: './e2e',

  // Run test files in parallel (each in its own isolated browser context).
  fullyParallel: true,

  // A stray `test.only` left in the code fails the CI run instead of silently
  // skipping everything else.
  forbidOnly: !!process.env.CI,

  // Flaky-guard: retry a failed test in CI (and capture a trace on that retry,
  // see `use.trace`). Locally we keep 0 so failures surface immediately.
  retries: process.env.CI ? 2 : 0,

  // Generates an HTML report you open with `npm run report`.
  reporter: 'html',

  // Defaults inherited by every test (and every project below).
  use: {
    baseURL: BASE_URL,            // makes `page.goto('/')` resolve against it
    trace: 'on-first-retry',      // full time-travel trace, captured only on retry
    screenshot: 'only-on-failure',
  },

  // ── Projects = your "profiles": one suite, many configurations ───────────────
  // Start with Chromium only; widen the matrix once the suite is green.
  projects: [
    { name: 'chromium', use: { ...devices['Desktop Chrome'] } },
    // { name: 'firefox', use: { ...devices['Desktop Firefox'] } },
    // { name: 'webkit',  use: { ...devices['Desktop Safari'] } },
    // { name: 'mobile',  use: { ...devices['Pixel 7'] } },
  ],

  // ── Optional: let Playwright start the server for you (Lesson 3 territory) ────
  // Uncomment to auto-launch a disposable Madshare before the run. Needs a built
  // binary + a throwaway DB seeded with the test users. For now we point at a
  // server you start yourself (the k6 model).
  //
  // webServer: {
  //   command: 'go run ../../madshare.go -config ./madshare.test.toml',
  //   url: BASE_URL,
  //   reuseExistingServer: !process.env.CI,
  //   cwd: '../..',
  // },
});
