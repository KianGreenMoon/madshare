# Playwright end-to-end (browser) tests

Functional, **single-user** tests that drive the Madshare web UI in a real
browser and assert correctness. This is the *top of the test pyramid* — few,
high-value user journeys.

> Not load testing. For throughput / latency under N users see `../k6/`. The two
> suites are deliberately separate: k6 answers "is it fast under load?", this one
> answers "does the feature actually work for one user?".

## Prerequisites

- Node.js ≥ 20 (uses the native `.env` loader; developed on Node 22).
- A **running, disposable Madshare server** with seeded `admin` / `uploader` /
  `user` accounts — same convention as the k6 suite. The upload journey *mutates*
  the library (creates and approves a track), so never point it at production.
- `ffmpeg` on PATH — only for the upload spec, which generates a tiny tagged
  fixture on the fly. Without it, `upload.spec.ts` skips itself.

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
  helpers/
    auth.ts              # ROLES, login(), storageStateFor()
    audio.ts             # ffmpeg-generated upload fixtures
    library.ts           # drill helper + sticky-header-safe menu/click helpers
  e2e/
    auth.setup.ts        # setup project: log in each role once → .auth/<role>.json
    auth.spec.ts         # login: happy path + wrong-password
    access.spec.ts       # role-based header gating matrix
    library.spec.ts      # artist → album → track drill-down
    playback.spec.ts     # clicking a track actually plays audio
    favorites.spec.ts    # heart a track → appears in Favorites → persists
    playlists.spec.ts    # create a playlist from a track → verify → delete
    upload.spec.ts       # uploader → moderation → appears in library (3 sessions)
  .env.example           # copy to .env to override BASE_URL / credentials
  .auth/                 # saved sessions (gitignored)
```

The `setup` project runs first and saves one session per role; specs adopt a
role with `test.use({ storageState: storageStateFor('user') })`, so they start
already signed in. The upload spec instead opens three explicit contexts
(uploader/admin/user) because the journey spans multiple identities.

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
