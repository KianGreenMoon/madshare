# k6 load & API test suite

Load / performance / regression / stability tests for the Madshare HTTP API.
**Security testing is out of scope** — this suite does not test auth failures,
throttle behavior, or negative authorization; it exercises the *working* API.

> ⚠️ The suite mutates data (upload + delete). It runs **only against a disposable
> test environment** — there are no in-script guards; the tester ensures the target.

## Prerequisites

- [k6](https://grafana.com/docs/k6/latest/set-up/install-k6/) on your `PATH`.
- A running Madshare server with the three seeded users (`admin` / `uploader` /
  `user`, password `password` by default — override via env, below).

## The model: user cases → profiles → scenarios

Three layers, each a separate entity, so the *traffic shape* lives apart from
*how you run it*:

1. **User cases** (`cases/`) — atomic, reusable workflows that mirror what a real
   user does (browse, search, listen, upload, …). The building blocks.
2. **Profiles** (`config/profiles/*.json`) — a *named* map of *case → requests per
   hour*. Pure data describing "normal traffic." A `PROFILE_PROC` multiplier scales
   a whole profile (1.0 = as written, 0.75 = lighter, 1.25 = heavier). The engine
   loads profiles dynamically from `config/profiles/index.json` (k6 can't list a
   directory), so adding a profile means dropping `<name>.json` and adding its name
   to that index — no code change.
3. **Scenarios** (`scenarios/`) — thin runners that execute a profile via one shared
   **engine** (`lib/engine.js`). Change the mix without touching scenarios; change
   the run style without touching the mix.

`setup()` mints one bearer token per role once and reuses it for every VU
(`lib/lifecycle.js` → `lib/auth.js`). Per-iteration login is avoided on purpose:
the login path runs argon2id behind a global 8-concurrent-verify cap, so it would
measure the throttle, not the endpoint. Tokens are the intended non-browser path.

### User cases

Each is one module under `cases/`, declaring the role that drives it and the
requests it makes. Profiles and scenarios reference cases by name.

| Case | Role | Workflow / endpoints |
|---|---|---|
| `browse` | user | drill down: `GET /api/artists` → `/api/albums?artist_id=` → `/api/tracks?album_id=` |
| `search` | user | `GET /api/search?q=` with a term derived from real artist names |
| `listen` | user | `GET /files/{hash}/{name}` — streams the blob (`responseType:'none'`) |
| `playlists` | user | `GET /api/playlists`, `GET /api/favorites` |
| `admin_read` | admin | `GET /api/admin/{storage,trash,moderation,duplicates}` |
| `upload` | uploader | `POST /files/upload` of a fixture file |
| `delete` | admin | `DELETE /api/admin/files/{hash}`, then (when `HARD_DELETE`) `DELETE /api/admin/trash/{hash}` |

`upload` and `delete` are **separate cases with different roles** — the uploader
cannot delete; deletion is an admin action. In a self-balancing profile they run
at equal rates, so the library nets to zero.

### Profiles & `PROFILE_PROC`

A profile maps each case to a `role` and a `per_hour` rate. Pick one with
`-e PROFILE=<name>` (default `standard`). Two ship:

- **`standard.json`** — balanced everyday traffic; `upload == delete` so it
  self-balances. (The numbers are placeholders — set real ones from capacity.)
- **`uploading.json`** — an active-ingest period: upload-heavy, still self-balanced.

The engine runs each case at `round(per_hour × PROFILE_PROC)` requests/hour
(k6 `constant-arrival-rate`, `timeUnit: '1h'`). Override a single case with
`-e PER_HOUR_<case>=…`.

### Scenarios

| Scenario | What it does |
|---|---|
| **`smoke`** | one pass of every case with all checks — the contract gate; run first |
| **`load`** | run `PROFILE` at `PROFILE_PROC` for `DURATION`. **regression = short `DURATION`, soak = long `DURATION`** — one scenario, one knob. Includes `upload`+`delete`. |
| **`upload`** | the `uploading` profile through the same engine — a separate entry point for an active-ingest period |
| **`capacity`** | reuses the engine and ramps `PROFILE_PROC` upward (ramp shape from `RAMP_*`) until the abort thresholds break — reports the max multiple of the profile the system sustains |

**Deriving the profile:** there are no analytics, so derive the standard rates from
a capacity run — `capacity` tells you the max `× profile` sustained; run
regression/soak at ~0.75 of that (via `PROFILE_PROC`, or fold it into the
`per_hour` numbers). Capacity is heavy → run it against the **dedicated** test
server, never your dev box / localhost.

## Quick start

```bash
# contract gate — one pass over every case, all checks (run this first)
k6 run tests/k6/scenarios/smoke.js

# regression: standard profile at 1.0 for a short DURATION
k6 run -e DURATION=5m tests/k6/scenarios/load.js

# soak: same scenario, long DURATION, lighter mix
k6 run -e DURATION=2h -e PROFILE_PROC=0.75 tests/k6/scenarios/load.js

# active-ingest run (upload-heavy profile)
k6 run tests/k6/scenarios/upload.js

# capacity: ramp PROFILE_PROC to the knee — DEDICATED test server, not localhost
k6 run -e BASE_URL=https://test.example.ygg tests/k6/scenarios/capacity.js

# machine-readable summary for trend tracking
k6 run --summary-export=summary.json -e DURATION=5m tests/k6/scenarios/load.js
```

## Configuration

Every knob is an env var with a default (see [`config/env.js`](./config/env.js)).
Copy [`.env.example`](./.env.example) and `source` it, or pass `-e KEY=val`. k6
reads OS env vars via `__ENV`, so exporting them works:

```bash
set -a; source tests/k6/.env; set +a
k6 run tests/k6/scenarios/load.js
```

| Env var | Default | Meaning |
|---|---|---|
| `BASE_URL` | `http://localhost:3000` | target root |
| `ADMIN_USER`/`ADMIN_PASS`, `UPLOADER_*`, `USER_*` | seeded creds | per-role credentials |
| `PROFILE` | `standard` | which `config/profiles/*.json` to run |
| `PROFILE_PROC` | `1.0` | scale the whole profile (0.75 / 1.0 / 1.25 …) |
| `PER_HOUR_<case>` | from profile | override one case's requests/hour |
| `DURATION` | `5m` | `load` run length (short = regression, long = soak) |
| `HARD_DELETE` | `true` | `delete` also purges from trash; `false` = soft-delete only |
| `RAMP_START`/`RAMP_STEP`/`RAMP_STEP_SECS`/`RAMP_MAX` | `0.5`/`0.5`/`60`/`5.0` | `capacity` ramp over `PROFILE_PROC` |
| `TEST_AUDIO_DIR` | `test_data` (repo dir, gitignored) | audio for `upload`; files listed in `config/audio-manifest.json` |

Thresholds live in [`config/options.js`](./config/options.js) (global + per-case;
capacity uses looser `abortOnFail` SLOs). Every request is **tagged by user case**,
so the summary and per-case thresholds break metrics down per case. Re-baseline the
starting SLOs after your first capacity run.

## Test data

- **Reads** discover real content live in `setup()` (`lib/discover.js`) — no
  seeding, no fixtures in the repo.
- **Uploads** read audio from `TEST_AUDIO_DIR` (default: the repo's gitignored
  `test_data/`). Because k6 cannot list a directory, the files actually opened are
  those named in [`config/audio-manifest.json`](./config/audio-manifest.json)
  (paths relative to `TEST_AUDIO_DIR`). **Edit the manifest when you swap audio.**
  No audio is ever committed. If a fixture is absent the `upload` case no-ops, so
  read-only runs work without any audio.
- **delete** addresses files by content hash (computed locally — the server keys
  blobs by `sha256` of the bytes), so it only ever reaps the manifest files the
  `upload` case creates, never the rest of the library. A 404 is accepted (the
  upload/delete churn means the file is often already gone).

## Safety

The suite runs **only against a disposable test environment** — so `delete` (incl.
hard delete) on test data is fine, and there are no "are you sure" guards. `prune`
(the destructive single-job admin op) is **not** a case and never runs. `capacity`
is heavy → dedicated test server only.

## Layout

```
config/    env.js, options.js (thresholds), profiles/{index.json,standard.json,uploading.json}, audio-manifest.json
lib/       auth, http, discover, data, lifecycle (setup/teardown), engine, runner (case dispatch)
cases/     browse, search, listen, playlists, admin_read, upload, delete
scenarios/ smoke, load, upload, capacity
```

## Separation from Playwright

k6 is a standalone Go binary with its own JS runtime — no Node, npm, or
`node_modules`. So `tests/k6/` has zero package-manager footprint and stays fully
independent of any future Playwright suite (`tests/e2e/`).
