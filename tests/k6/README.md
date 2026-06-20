# k6 load & API test suite

Load / performance / regression / stability tests for the Madshare HTTP API.
Design and rationale: [PLAN.md](./PLAN.md). **Security testing is out of scope.**

> ⚠️ The suite mutates data (upload + delete). Run it **only against a disposable
> test environment** — there are no in-script guards.

## Prerequisites

- [k6](https://grafana.com/docs/k6/latest/set-up/install-k6/) on your `PATH`.
- A running Madshare server with the three seeded users (`admin` / `uploader` /
  `user`, password `password` by default — override via env, see below).

## The model (PLAN.md in one paragraph)

**User cases** (`cases/`) are atomic workflows (browse, search, listen, …).
A **profile** (`config/profiles/*.json`) is a named map of *case → requests/hour*.
**Scenarios** (`scenarios/`) run a profile, scaled by `PROFILE_PROC`, via a shared
engine. `setup()` mints one bearer token per role and reuses it for every VU.

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
```

## Configuration

Every knob is an env var with a default (see [`config/env.js`](./config/env.js)).
Copy [`.env.example`](./.env.example) and either `source` it or pass `-e KEY=val`.
k6 reads OS env vars via `__ENV`, so exporting them works:

```bash
set -a; source tests/k6/.env; set +a
k6 run tests/k6/scenarios/load.js
```

Key ones: `BASE_URL`, the `*_USER`/`*_PASS` credentials, `PROFILE`,
`PROFILE_PROC`, `DURATION`, `PER_HOUR_<case>`, `HARD_DELETE`, the `RAMP_*` ramp
shape, and `TEST_AUDIO_DIR`.

## Test data

- **Reads** discover real content live in `setup()` — no seeding, no fixtures in
  the repo.
- **Uploads** read audio from `TEST_AUDIO_DIR` (default: the repo's gitignored
  `test_data/`). Because k6 cannot list a directory, the files actually opened are
  those named in [`config/audio-manifest.json`](./config/audio-manifest.json)
  (paths relative to `TEST_AUDIO_DIR`). **Edit the manifest when you swap audio.**
  No audio is ever committed. If `TEST_AUDIO_DIR` is empty/absent the `upload`
  case simply no-ops, so read-only runs still work.
- **delete** only reaps the suite's own uploads (files whose name is in the
  manifest), so it never destroys the rest of the library. With auth configured,
  uploads land as drafts; on a long run those drafts accumulate — prune the test
  server between big runs if needed.

## Layout

```
config/   env.js, options.js (thresholds), profiles/*.json, audio-manifest.json
lib/      auth, http, discover, data, lifecycle (setup/teardown), engine, runner
cases/    one file per user case
scenarios/ smoke, load, upload, capacity
```
