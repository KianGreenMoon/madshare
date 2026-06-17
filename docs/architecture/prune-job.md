# Single-process prune (background prune job)

Status: **agreed** — all decisions resolved; not yet implemented.

## Problem

Verify & Prune (`/admin/prune`) scans every `files` row to find rows whose
backing blob is missing (cheap presence scan) or — with **Deep verify** —
re-hashes every blob to also catch bit-rot corruption (slow: reads all content).

Today the whole scan runs **synchronously inside the HTTP request**:

```
POST /api/admin/prune {confirm,deep}  →  database.PruneDangling(...)  →  blocks until done  →  JSON
```

Consequences:

- **No singleton.** Two admins — or one admin in two tabs — can each fire a deep
  scan. They run concurrently and independently, oblivious to each other. Nothing
  guards against overlap.
- **No cross-page / cross-user visibility.** The work is bound to one request on
  one page. Navigate away and you lose all sight of it; another admin opening the
  page sees a clean "Preview prune" button with no hint a scan is already running.
- **No cancel.** Once a deep scan starts there is no way to stop it.
- **Wasteful double scan.** Preview runs the (slow) scan once; clicking *Prune*
  runs the identical scan a second time before deleting. The deep read happens
  twice.

## Goals

1. **Strictly one prune operation server-wide at a time.** A second start is
   rejected, not queued or run in parallel.
2. **Runs in the background.** The initiator can navigate away (or close the tab)
   without killing it. The work outlives the HTTP request that started it.
3. **Shared, live status.** Any admin opening `/admin/prune` sees the current
   state: *in progress* (who started it, when, deep or not, progress so far) or
   *idle* with the last result.
4. **Cancellable.** A button stops the running operation cleanly.
5. **Durable last-run record.** The page shows the outcome of the most recent
   scan and of the most recent prune — counts **and the date/time each ran** — so
   an admin opening the page (even after a restart) sees "last scanned N dangling
   on *date*" / "last pruned M records on *date*" without re-running anything.
6. No duplicate / parallel runs to reconcile — there is only ever one.

## Non-goals

- Surviving a server restart **for the in-flight run**. A prune actively running
  when the process exits is simply gone; the next start begins fresh. Prune is
  idempotent and re-runnable, so this is acceptable (see
  [Restart behaviour](#restart-behaviour)). The *summary of the last completed
  run* **is** persisted (small DB record) so the date + counts survive restart —
  see [Last-run record](#last-run-record-persisted).
- Queuing. There is no "run after the current one" — a start while busy is
  refused, full stop.
- Changing *what* gets pruned. The scan/flag/delete logic in
  `database.PruneDangling` is unchanged except for progress reporting and
  cancellation support.

## Design

### An in-memory singleton manager

Introduce a process-global **prune manager** (one instance, created in `main`,
lives for the process lifetime) that owns the one-and-only prune operation. It is
plain in-memory state guarded by a mutex — **not** a DB-backed queue like
`imageproc`/`mediaproc`, because prune is a single global operation, not a stream
of per-item jobs, and it does not need to survive restarts.

The manager is injected into the API layer via `api.Deps` (alongside `ImagePool`
/ `MediaPool`) and held by the admin `handler`.

Suggested home: a small new package `prune/` (e.g. `prune.Manager`), keeping the
HTTP handlers thin. It depends only on `database.PruneDangling` + the storage
probe it already takes, so the dependency arrow stays `api → prune → database`.

### The critical fix: detach from the request

The job runs on a context **derived from `context.Background()`**, not the
incoming `*http.Request` context. The start handler kicks the goroutine off and
returns immediately (`202 Accepted`). When the HTTP response completes — or the
admin navigates away — the request context is cancelled, but the job's own
context is not, so the work continues. This single change is what makes the
operation outlive its initiator.

The job's context is cancellable and its `cancel` func is held by the manager so
the cancel endpoint can stop it.

### State machine

```
            start (202)                 finishes / cancelled / fails
  idle  ───────────────────►  running  ─────────────────────────────►  idle
   ▲                            │  ▲                                     │
   │      start while running   │  │   start while running               │
   └────────────── 409 Conflict ┘  └──────────────── 409 Conflict ───────┘
```

- **idle** — no operation in flight. Exposes the persisted
  [last-run record](#last-run-record-persisted): the most recent scan and the most
  recent prune, each with its counts, deep flag, who ran it, **finished-at
  timestamp**, and how it ended (`completed`, `cancelled`, `failed`). A run that
  just finished in this process also keeps its full dangling/pruned list in memory
  so the live page can render the detail; after a restart only the persisted
  summary (counts + date) remains.
- **running** — exactly one operation in flight. A snapshot exposes: `phase`
  (`scanning` for a dry run, `pruning` for a confirmed run), `deep`, `started_by`
  (username), `started_at`, and live `progress` (`scanned` / `total`).

The manager guards every transition with its mutex. `Start` is a compare-and-swap
on the state: if already running it returns `ErrBusy` and the handler responds
`409 Conflict` with the current snapshot, so the UI can immediately render
"in progress" instead of starting a duplicate.

### Progress + cancellation in `PruneDangling`

`PruneDangling` gains two small capabilities (both no-ops for existing callers):

- A progress callback invoked per row (e.g. `onProgress(scanned, total int)`), so
  the manager can publish `scanned X of N`. The slow deep path benefits most.
- An explicit `ctx.Err()` check at the top of the loop so a cancelled context
  ends the pass promptly. (It already receives a `ctx`; today it never inspects
  it mid-loop.) On cancel it returns whatever it has flagged/pruned so far plus
  `context.Canceled`; the manager records the run as `cancelled`.

### Preview vs. prune (the two phases)

The existing two-step UX is preserved, both phases routed through the one manager
so neither can overlap the other:

- **Scan (preview)** = a job with `confirm=false`. The full sweep — walks **every**
  file row and finds **every** missing/corrupt blob (deep or quick). This is the
  damage-finding pass and is always a complete scan; its dangling list is held as
  the run result.
- **Prune (delete)** = acts on the held scan result. It deletes **exactly the set
  the scan found and the admin reviewed**, re-checking only *those* flagged hashes
  immediately before deleting (cheap, even in deep mode) to guard the rare case of
  a blob reappearing in the gap. It does **not** re-sweep the whole library.

**Why Prune does not re-scan everything.** The full damage scan still always
happens — it is the Scan pass above. Re-sweeping the entire library *again* at
delete time is both wasteful (a deep prune would read all content twice) and
unsafe for the confirmation contract: a file corrupted/removed between preview and
the Prune click would be found by the second sweep and deleted even though the
admin never saw it ("Prune 4 records" silently removes 6). Deleting exactly the
reviewed set keeps the action equal to what was confirmed. Finding *newly* damaged
files is done by running the Scan again, not by the delete step.

### Last-run record (persisted)

So the "previous result + date" survives a restart, the manager persists a small
summary of the most recent completed run to the DB. Two slots are kept, because a
preview scan can run many times without ever committing a prune:

- **`prune.last_scan`** — last dry-run preview:
  `{deep, scanned, dangling_count, outcome, by, finished_at}`
- **`prune.last_prune`** — last confirmed prune:
  `{deep, scanned, pruned_count, failed_count, outcome, by, finished_at}`

`outcome` is `completed` / `cancelled` / `failed`; `finished_at` is the timestamp
shown on the page ("last pruned 3 records on 2026-06-18 14:02").

Storage: a JSON value per slot in the existing **`settings`** table (no new
migration; it is already the home for small server-managed key/values). The
manager writes the matching slot when a run completes and reads both at startup to
seed its idle snapshot. The detailed dangling/pruned *lists* are not persisted —
only the summary counts + date — since the lists can be huge and a returning admin
just needs the headline + the ability to re-scan. (The existing
`audit_log` `file.prune` entry remains the durable per-event audit trail; the
`settings` slots are the cheap at-a-glance display the page reads.)

### HTTP API

| Method & path | Purpose |
|---|---|
| `POST /api/admin/prune` | **Start** a run. Body `{confirm,deep}` unchanged. Returns `202 Accepted` + current snapshot, or `409 Conflict` + snapshot if one is already running. No longer blocks. |
| `GET /api/admin/prune/status` | Current snapshot: `state` (`idle`/`running`), `phase`, `deep`, `started_by`, `started_at`, `progress {scanned,total}`; the persisted `last_scan` / `last_prune` summaries (counts + `finished_at` + `outcome` + `by`); and, when a run finished in-process, its full dangling/pruned `last_result` detail. The page polls this. |
| `POST /api/admin/prune/cancel` | Cancel the running operation. `200` if a run was cancelled, `409`/no-op if idle. |

All three stay gated by `file.delete` (the prune page's existing requirement),
mounted in `RegisterAdmin`.

`GET .../status` is intentionally cheap and read-only so the page can poll it
(every ~1–2 s while running) and any admin gets the same shared view.

### Web UI (`/admin/prune`, `prune.js`)

- **On load**, fetch `/status` first. If `running`, render the live in-progress
  panel (started by *user* at *time*, deep or quick, `scanned X of N`, a **Cancel**
  button) and begin polling. If `idle`, render a **last-run summary line** for each
  slot present — e.g. "Last scan: 4 dangling of 1,203 checked · deep · 2026-06-18
  14:02 by admin" and "Last prune: 4 records removed · 2026-06-18 14:05 by admin" —
  plus the full `last_result` detail if this process still holds it.
- **Preview / Prune buttons** call `POST /api/admin/prune` and switch to the
  polling/in-progress view. A `409` response means someone beat us to it — render
  their in-progress panel from the returned snapshot (no error, just reconcile).
- **Cancel button** calls `POST .../cancel`, then resumes polling until the state
  flips to `idle`.
- Polling stops when `state` becomes `idle`; the final `last_result` is rendered
  using the existing `renderDanglingPanel` / `renderPruneCommitResult` paths.
- The confirmation modal is unchanged; *Prune* still goes through it.

The admin **dashboard / overview** also shows a small "prune in progress"
indicator on its Verify & Prune card (reuses the same `/status` endpoint, cheap),
mirroring the moderation pending-count pattern, so the running state is visible
without opening the prune page itself.

### Wiring

- `main` constructs the single `prune.Manager` and passes it into `api.Deps`
  (e.g. `Deps.PruneManager`), like `ImagePool`/`MediaPool`.
- The job goroutine uses a `context.Background()`-derived context, **not** the
  request `ctx` (so it outlives the request) and **not** cancelled by the shutdown
  `ctx` (so graceful shutdown does not abort it — see below).
- **Graceful shutdown lets a running prune finish.** The manager exposes a `Wait`
  (a `sync.WaitGroup` over the in-flight run); the shutdown path waits on it after
  the HTTP servers stop, rather than cancelling the job. So a prune in progress
  runs to completion instead of being killed mid-pass. Caveat: if the supervisor
  (systemd) enforces a stop timeout it may `SIGKILL` first; that is safe because
  prune is idempotent and re-runnable, and the persisted summary simply won't be
  updated for that aborted run. An admin who needs a fast shutdown can hit Cancel.
- `handler.adminPrune` becomes a thin "ask the manager to start" call; the actual
  `PruneDangling` invocation moves into the manager's goroutine.

## Restart behaviour

Graceful shutdown waits for a running prune to finish (see Wiring), so an orderly
restart does not interrupt one. If the process is killed hard while a prune is
running, the in-memory *running* state is lost and the next `/status` reports
`idle` (the persisted `last_scan`/`last_prune` summaries from earlier completed
runs remain). No half-finished record to reconcile: a dry run changes nothing, and
a confirmed run deletes row-by-row idempotently, so simply starting again
completes the job — which is why only the *running* state, not the operation, is
in-memory.

## Permissions

Unchanged: all prune endpoints require **`file.delete`** (held by admins; the
moderator role holds `content.moderate`, not `file.delete`, so moderators do not
prune today — calling out explicitly since the request was unsure). If we want
moderators to prune, that is a separate role-permission change, out of scope here.

## Resolved decisions

1. **Prune (delete) re-scan** — the Scan pass is the full damage sweep; Prune
   deletes exactly the reviewed set (re-checking only those flagged hashes), so it
   never deletes more than was confirmed and never deep-reads the whole library
   twice. See [Preview vs. prune](#preview-vs-prune-the-two-phases).
2. **Shutdown behaviour** — *let the running prune finish.* Graceful shutdown
   waits for it (does not cancel); a hard kill is safe (idempotent re-run).
3. **Dashboard indicator** — *yes,* show an "in progress" indicator on the admin
   overview/dashboard card too (same `/status` endpoint).
4. **Last-run summary store** — *use the `settings` slots* (latest of each, no
   full-history table).
