# k6 Load & API Test Plan

Status: **draft for review (rev 3)** · Scope: k6 only (Playwright is a separate suite,
see [§2](#2-separation-from-playwright)) · Target: Madshare HTTP API.

This is the agreed design before any k6 script is written. It is grounded in the live
API surface (`api/api.go`, `api/auth_handlers.go`) and probes of the running local
server. Nothing here is implemented yet.

**Explicitly out of scope:** security testing. This suite does **not** test auth
failures, throttle behavior, or negative authorization (401/403). k6 here is about
**load, performance, regression and stability** of the working API.

**The whole suite runs only against a disposable test environment.** There are no
in-script "are you sure" guards — the tester always points it at a test server. So
write user cases (upload/delete) are first-class, not opt-in.

---

## 1. The model: user cases → profiles → scenarios

Three layers, each a separate entity:

1. **User cases** ([§5](#5-user-cases)) — atomic, reusable workflows that mirror what a
   real user does (browse, search, listen, upload, delete, …). The building blocks.
2. **Profiles** ([§6](#6-profiles--profile_proc)) — a *named* set of user cases, each
   with a **standard rate in requests per hour**. Pure data; describes "normal traffic."
   Several profiles can exist (`standard`, `uploading`, …). A `profile_proc` multiplier
   scales a whole profile up or down (1.0 = as written, 0.75 = lighter, 1.25 = heavier).
3. **Scenarios** ([§7](#7-scenarios)) — thin runners that take a profile + `profile_proc`
   and execute it. They share one **engine** so they stay consistent.

This separation is the point: the *traffic shape* (profile) lives apart from *how we
run it* (scenario). Change the mix without touching scenarios; change the run style
without touching the mix.

---

## 2. Separation from Playwright

k6 ships as a **standalone Go binary** with its **own JS runtime (goja)** — no Node,
npm, or `node_modules`. So `tests/k6/` carries **zero** package-manager footprint and
never shares a `package.json` with the future Playwright suite (`tests/e2e/`). Shared
code is plain ES-module `import` *within* `tests/k6/` only.

---

## 3. Target environment & credentials

All configurable via env vars — pointing at a different server is a one-line change.

| | Value (default) | Env var |
|---|---|---|
| Base URL | `http://localhost:3000` (now; later a dedicated server) | `BASE_URL` |
| Admin | `admin` / `password` | `ADMIN_USER` / `ADMIN_PASS` |
| Uploader | `uploader` / `password` | `UPLOADER_USER` / `UPLOADER_PASS` |
| Plain user | `user` / `password` | `USER_USER` / `USER_PASS` |

Verified live: all three log in `200`, no forced password change. Permissions:
`user` → content-access; `uploader` → `file.upload` + content-access (**cannot
delete**); `admin` → full incl. `file.delete` / `content.moderate`.

**Library state:** *not* empty — the earlier "empty" reading was an **unauthenticated**
call hitting default-deny on the listing endpoints. Authenticated, the live server has
**125 files / 6 artists**, so read cases have real content today; no seeding to start.

API quirks captured for the cases:
- `/api/tracks` requires `?album_id=N`; the **flat** list is `/api/files`.
- Listing + `/files/*` are **content-access gated** → every case carries an identity
  (we always do — see §4). An anonymous empty/denied result is by design, not tested.

---

## 4. Authentication — token once in `setup()`

`setup()` runs once before VUs start: log in each needed role once, `POST
/api/auth/tokens` to mint a bearer token, return the raw tokens; every VU sends
`Authorization: Bearer <token>`. `teardown()` revokes them.

Per-iteration login is avoided deliberately: the login path runs argon2id (~64 MiB)
behind a global 8-concurrent-verify cap, so it would measure argon2, not the endpoint.
Tokens are the intended non-browser path. `lib/auth.js` encapsulates this; one token
per role (`user` / `uploader` / `admin`).

---

## 5. User cases

Atomic workflows, one module each under `cases/`. Each declares the **role** that drives
it and the request(s) it makes. Scenarios and profiles reference cases by name.

| Case | Role | Workflow / endpoints |
|---|---|---|
| `browse` | user | drill down: `GET /api/artists` → `/api/albums?artist_id=` → `/api/tracks?album_id=` |
| `search` | user | `GET /api/search?q=` with a term from discovered data |
| `listen` | user | `GET /files/{hash}/{name}` (audio stream) + `GET /api/albums/{id}/image` (cover) |
| `playlists` | user | `GET /api/playlists`, `GET /api/favorites` |
| `admin_read` | admin | `GET /api/admin/{storage,trash,moderation,duplicates}` (dashboards) |
| `upload` | uploader | `POST /files/upload` of a fixture file (see §8) |
| `delete` | admin | remove a test-uploaded file: `DELETE /api/admin/files/{hash}` (→ trash), and — when `hardDelete` (default) — also `DELETE /api/admin/trash/{hash}` to purge it |

Notes:
- **`upload` and `delete` are separate cases, different roles** — the uploader cannot
  delete; deletion is admin. In a self-balancing profile they run at equal rates so the
  library nets to zero.
- **Hard delete** is the `hardDelete` flag (env `HARD_DELETE`, **default `true`**):
  `true` purges the blob from trash too; `false` leaves it soft-deleted in trash. Since
  the suite only runs on a test environment, full purge is the sensible default — we
  don't need to be cautious about it.
  *(JS naming: env var is `HARD_DELETE` (UPPER_SNAKE); the in-code boolean is `hardDelete`
  (camelCase) — both idiomatic.)*
- Upload→delete is just a **plain workflow** used in almost every run, not an automated
  coordination subsystem — `delete` simply removes what the suite uploaded.
- A case is an exported function `(ctx) => { ... }` taking the per-role token +
  discovered data; the engine maps each case name to its function via k6 `exec`.

---

## 6. Profiles & `profile_proc`

A **profile** is a named JSON file under `config/profiles/`: a map of *user case →
{ role, per_hour }*. `per_hour` is the standard request rate **per hour**. Multiple
profiles coexist; pick one with `-e PROFILE=<name>` (default `standard`).

**`config/profiles/standard.json`** — balanced everyday traffic; `upload == delete`
so it self-balances. (Numbers are placeholders the tester sets.)

```jsonc
{
  "name": "standard",
  "note": "requests/hour per user case — placeholders; tester sets real values",
  "cases": {
    "browse":     { "role": "user",     "per_hour": 3000 },
    "listen":     { "role": "user",     "per_hour": 1500 },
    "search":     { "role": "user",     "per_hour": 600  },
    "playlists":  { "role": "user",     "per_hour": 300  },
    "admin_read": { "role": "admin",    "per_hour": 120  },
    "upload":     { "role": "uploader", "per_hour": 60   },
    "delete":     { "role": "admin",    "per_hour": 60   }
  }
}
```

**`config/profiles/uploading.json`** — an active-uploading period; upload-weighted, still
self-balanced (`upload == delete`). The example of "a profile with different values."

```jsonc
{
  "name": "uploading",
  "note": "heavy ingest period",
  "cases": {
    "upload":  { "role": "uploader", "per_hour": 1200 },
    "delete":  { "role": "admin",    "per_hour": 1200 }
  }
}
```

**`profile_proc`** (`-e PROFILE_PROC=1.0`) scales the whole profile: the engine runs each
case at `round(per_hour × profile_proc)` requests/hour (k6 `constant-arrival-rate`,
`timeUnit: '1h'`). `1.0` = the profile as written, `0.75` lighter, `1.25` heavier. A
single case can also be overridden directly with `-e PER_HOUR_upload=2000`.

The **engine** (`lib/engine.js`) is the shared core: given a profile object and a
`profile_proc`, it emits k6 `options.scenarios` — one arrival-rate scenario per case,
each at its scaled rate, `exec`-bound to that case's function. Every scenario in §7
builds on it.

---

## 7. Scenarios

Thin runners over the engine. Pick one by running its file.

| Scenario | What it does | Notes |
|---|---|---|
| **`smoke`** | one pass of every user case, all `check()`s | contract gate; run first; tiny |
| **`load`** | run `PROFILE` at `PROFILE_PROC` for `DURATION` | **regression = short `DURATION`, soak = long `DURATION`** — same scenario, one knob. Includes `upload`+`delete`. |
| **`upload`** | run the `uploading` profile | a *separate* scenario for the active-uploading period — same engine, fixed to the upload-heavy profile, kept distinct for obviousness |
| **`capacity`** | **imports `load`/engine** and ramps `PROFILE_PROC` upward until thresholds break | reports the max multiple of the profile the system sustains; the heavy ceiling-finder |

Key points:
- **`load` is the merged soak+regression** (your call): identical scenario, the only
  difference is `DURATION`. A short run asserts the regression thresholds; a long run
  is the stability/soak check.
- **`capacity` reuses `load`'s engine** — it wraps the same per-case scenarios in a
  ramp. The ramp drives `profile_proc` (not raw RPS), so capacity literally answers
  "how many × the standard profile can we take": `ramp = { start, step, step_secs, max }`
  over `profile_proc`, all in `config` and env-overridable (`RAMP_*`).
- Capacity is heavy — run it against the **dedicated test server**, not your dev box.
  (No code guard; the tester knows the suite only ever targets a test environment.)

---

## 8. Test data

**Reads — discover, don't seed.** `lib/discover.js` runs in `setup()`: lists artists →
albums (`?artist_id=`) → tracks (`?album_id=`) + `/api/files`, collecting real ids and
track hashes into a `SharedArray` the read cases pick from. Works against the live
server (125 files) and any populated server later; no fixtures in the repo.

**Uploads — a configurable, never-committed audio dir.** `TEST_AUDIO_DIR` points at a
local folder of audio the `upload` case reads via k6 `open()`. Default is the repo's
**`test_data/`** dir (provided locally, **gitignored** — never committed); the tester
can point it at any other royalty-free set. Because k6 cannot list a directory at
runtime, the files actually opened come from **`config/audio-manifest.json`** (relative
paths under `TEST_AUDIO_DIR`); edit it when you swap the audio. README documents this.

**Upload + delete is a plain workflow.** Uploading then deleting is something almost
every run does — not an automated coordination subsystem, so we don't build machinery
around it. The `upload` case adds test files; the `delete` case (admin) removes test
uploads, purging from trash too when `hardDelete` is on (default). On a test environment
we don't over-engineer which file gets reaped — in a balanced profile delete simply
keeps pace with upload. (Re-uploading an identical blob hits content-hash dedupe; point
`TEST_AUDIO_DIR` at fresh files for genuinely new inserts.)

---

## 9. Safety

The premise removes the need for in-script guards: **the suite is only ever pointed at a
disposable test environment**, and the tester knows that. Concretely:

1. The suite runs on a **disposable test environment**, so `delete` (incl. hard delete /
   trash purge) operating on test data is fine — no guards built around which file it reaps.
2. `prune` (the destructive single-job admin op) is **not** a user case and is never
   placed in any scenario.
3. `capacity` is heavy → use the dedicated test server, not localhost. Guidance, not an
   enforced flag.

---

## 10. Configuration (env vars)

Defaults in `config/env.js`; override with `-e KEY=val`.

| Env var | Default | Meaning |
|---|---|---|
| `BASE_URL` | `http://localhost:3000` | target root |
| `ADMIN_USER`/`ADMIN_PASS` etc. | seeded creds | per-role credentials |
| `PROFILE` | `standard` | which `config/profiles/*.json` to run |
| `PROFILE_PROC` | `1.0` | scale the whole profile (0.75 / 1.0 / 1.25 …) |
| `PER_HOUR_<case>` | from profile | override a single case's requests/hour |
| `HARD_DELETE` | `true` | `delete` case also purges from trash (`DELETE /trash/{hash}`); `false` = soft-delete only |
| `DURATION` | per-scenario | run length (`load`: short = regression, long = soak) |
| `RAMP_START`/`RAMP_STEP`/`RAMP_STEP_SECS`/`RAMP_MAX` | from config | capacity ramp over `profile_proc`: start / step / step duration / cap |
| `TEST_AUDIO_DIR` | `test_data` (repo dir, gitignored) | audio folder for the `upload` case; files listed in `config/audio-manifest.json` |

Starting thresholds (in `config/options.js`, re-baselined after the first capacity run):

```js
thresholds: {
  http_req_failed:   ['rate<0.01'],
  http_req_duration: ['p(95)<500', 'p(99)<1500'],
  checks:            ['rate>0.99'],
  // per-case via tags, e.g. listen (file streaming) gets its own looser p95
}
```

Every request is **tagged by user case** so the summary breaks latency/error down per
case, and `load` (regression mode) can assert per-case baselines.

---

## 11. Running (target shape, once implemented)

```bash
# smoke — contract gate
k6 run tests/k6/scenarios/smoke.js

# load as a regression check (short) — standard profile at 1.0
k6 run -e DURATION=5m tests/k6/scenarios/load.js

# same scenario as a soak (long) at a lighter mix
k6 run -e DURATION=2h -e PROFILE_PROC=0.75 tests/k6/scenarios/load.js

# active-uploading period
k6 run -e TEST_AUDIO_DIR=/path/to/audio tests/k6/scenarios/upload.js

# capacity — ramp profile_proc to the knee (dedicated test server)
k6 run -e BASE_URL=https://test.example.ygg tests/k6/scenarios/capacity.js

# machine-readable output for trend tracking
k6 run --summary-export=summary.json -e DURATION=5m tests/k6/scenarios/load.js
```

`handleSummary()` emits a readable summary + a JSON artifact. k6 Cloud / Grafana is out
of scope for now.

---

## 12. Directory layout

```
tests/k6/
  PLAN.md
  README.md                    # how to run + setup (TEST_AUDIO_DIR, profiles)
  config/
    env.js                     # BASE_URL, creds, PROFILE, PROFILE_PROC, RAMP_*, TEST_AUDIO_DIR
    options.js                 # thresholds, per-case tagging, handleSummary
    profiles/
      standard.json            # balanced, self-balancing (upload == delete)
      uploading.json           # upload-heavy active-ingest profile
    audio-manifest.json        # relative paths under TEST_AUDIO_DIR the upload case opens
  cases/                       # atomic user cases (one workflow each)
    browse.js  search.js  listen.js  playlists.js
    upload.js  delete.js  admin_read.js
  lib/
    auth.js                    # mint tokens in setup(); per-role bearer headers
    http.js                    # checked request wrapper; per-case tags
    discover.js                # setup(): pull live ids + track hashes
    data.js                    # SharedArray of discovered data; multipart builder
    engine.js                  # profile × profile_proc → k6 scenarios (shared core)
  scenarios/
    smoke.js                   # one pass of every case
    load.js                    # engine(PROFILE, PROFILE_PROC) for DURATION (regression/soak)
    upload.js                  # engine(uploading profile) — active-uploading run
    capacity.js                # imports engine; ramps PROFILE_PROC to the knee
```

---

## 13. Decisions log (resolved)

- Three-layer model: **user cases → profiles (req/hour) → scenarios**, with `profile_proc`
  scaling. ✓
- `upload` and `delete` are **separate cases, different roles** (uploader can't delete;
  delete is admin). `delete` supports **hard delete** via `hardDelete` (env `HARD_DELETE`,
  **default `true`** = also purge from trash). Upload+delete is a **plain workflow, not
  automated coordination** — fine because it runs only on a test environment. ✓
- **Multiple profiles** supported; `standard` (balanced, self-balancing) + `uploading`
  (upload-heavy) provided. ✓
- **soak + regression collapsed into one `load` scenario**, differing only by `DURATION`. ✓
- **`upload` kept as a separate scenario** (active-uploading period) for obviousness. ✓
- `capacity` stays a separate scenario but **imports the load engine** and ramps
  `profile_proc` to the ceiling. ✓
- **No in-script guards** (`ALLOW_WRITES` / loopback checks removed) — suite runs only on
  a test environment; the tester ensures it. ✓
- Auth: token once in `setup()`. Test data: discover for reads, `TEST_AUDIO_DIR` for
  uploads (never committed). Security testing out of scope. ✓

---

## 14. Implementation phases (after approval)

1. **Phase 1 — scaffolding + smoke.** `config/` (env, options, both profiles), `lib/`
   (auth, http, discover, data, engine), `cases/` (the read cases), `scenarios/smoke.js`,
   `README.md`. Runs immediately against localhost.
2. **Phase 2 — `load` scenario.** Engine wired to profiles + `profile_proc`; run as a
   short regression. Read cases first, then add `upload`/`delete` cases (`TEST_AUDIO_DIR`,
   marker prefix, balanced delete).
3. **Phase 3 — `upload` scenario + `uploading` profile** tuned.
4. **Phase 4 — `capacity`.** Ramp `profile_proc`; produce first ceiling; tester sets real
   `per_hour` numbers; lock per-case thresholds.
5. **Phase 5 — long-run soak** = `load` at long `DURATION`; drift/leak watch.
6. **Later (out of scope now):** CI wiring, trend storage, k6 Cloud/Grafana.

---

*Nothing here is implemented. Awaiting review before writing Phase 1.*
