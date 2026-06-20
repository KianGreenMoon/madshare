# Playwright end-to-end (browser) tests

Functional, **single-user** tests that drive the Madshare web UI in a real
browser and assert correctness. This is the *top of the test pyramid* — few,
high-value user journeys.

> Not load testing. For throughput / latency under N users see `../k6/`. The two
> suites are deliberately separate: k6 answers "is it fast under load?", this one
> answers "does the feature actually work for one user?".

## Prerequisites

- Node.js ≥ 20 (uses the native `.env` loader; developed on Node 22).
- A **running Madshare server** with seeded test users — same disposable
  environment convention as the k6 suite.

## Install

```bash
cd tests/playwright
npm install                 # installs @playwright/test locally
npx playwright install      # downloads the browser binaries (one-time, ~hundreds of MB)
```

## Test environment

The specs point at a server you run yourself (`BASE_URL`, default
`http://localhost:3000`) and sign in as a seeded user (`TEST_USER` / `TEST_PASS`,
default `user` / `password` — the k6 convention).

> ⚠️ **The test user must not be a first-run admin.** On first login a freshly
> bootstrapped admin is forced through a change-password modal, so the clean
> "logged-in" state never appears. Use a user that has already completed setup.

Override anything via env or a local `.env` (copy `.env.example`; it's gitignored
because it may hold credentials):

| Variable    | Default                 | Meaning                                  |
|-------------|-------------------------|------------------------------------------|
| `BASE_URL`  | `http://localhost:3000` | Server under test                        |
| `TEST_USER` | `user`                  | Seeded login username                    |
| `TEST_PASS` | `password`              | Seeded login password                    |
| `CI`        | _(unset)_               | When set: enables retries, forbids `.only` |

## Running

```bash
npm test                    # headless, all projects
npm run test:headed         # watch it drive a real browser
npm run test:ui             # interactive UI mode (run / re-run / time-travel)
npm run test:debug          # step through with the inspector
npm run report              # open the HTML report after a run
npm run codegen             # record clicks → generated test code

npx playwright test auth.spec.ts            # one file
npx playwright test -g "wrong password"     # filter by title
npx playwright test --project=chromium      # one project
```

## Layout

```
tests/playwright/
  playwright.config.ts   # the control center: baseURL, projects, retries, trace
  e2e/
    auth.spec.ts         # login: happy path + wrong-password
  .env.example           # copy to .env to override BASE_URL / credentials
```

## Debugging a failure

`trace: 'on-first-retry'` captures a full time-travel trace (DOM snapshots,
network, console per step) when a test retries. Open the last one with:

```bash
npx playwright show-trace
```

## Troubleshooting — Fedora / non-Debian Linux

**Do not run `npx playwright install-deps` or `playwright install --with-deps` on
Fedora.** That helper only speaks `apt` and will hand you Debian package names
(e.g. `libjpeg-turbo8`) that don't exist on Fedora. It's a dead end, not a real
missing dependency.

Just download the browser and rely on system libraries:

```bash
npx playwright install chromium     # downloads only Chromium
ldd ~/.cache/ms-playwright/chromium-*/chrome-linux/chrome | grep "not found"   # should be empty
```

On a desktop Fedora the distro `chromium` package already pulls in every library
the bundled Chromium needs, so this "just works" — no Docker/podman required.
`libjpeg-turbo8` is a WebKit/Firefox dependency; this suite uses Chromium only.
If you ever enable Firefox/WebKit you'll need a few `dnf` libs (incl.
`libjpeg-turbo`, the Fedora name) — but that's optional.

## Next steps (future lessons)

- A shared **login fixture** + saved **storageState** so most tests start already
  authenticated (no repeated login clicks).
- More journeys: upload → draft → submit → moderate → appears in library; play a
  track; playlists; permission gating.
- A `webServer` block (commented in the config) to auto-launch a disposable
  Madshare seeded for tests, so `npm test` is self-contained.
