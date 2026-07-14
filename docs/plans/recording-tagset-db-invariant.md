# Plan: DB-enforced "every recording has ≥ 1 tagset"

Status: **proposed** — awaiting the owner's pick between Option 1 and Option 2.
All mechanics below were verified empirically on the real driver
(modernc.org/sqlite) with the project's pragmas (`foreign_keys=ON`,
`_txlock=immediate`), inside transactions shaped like the migration runner's
and like the app's write paths.

## Why

A recording's tagsets are its catalog entries — a recording with zero tagsets
is unreachable: no library row lists it, no admin lens reaches it, its blobs
sit dead on disk. The model has always *declared* the invariant ("every
recording has ≥ 1 tagset, exactly one primary" — `assertInvariants` in
`lifecycle_test.go`), but only application discipline enforces it, backed by
two after-the-fact repairs:

- `ReconcileTagsets` (startup) heals a zero-tagset recording by
  **manufacturing** a filename-derived appearance — the exact shape the
  "don't manufacture nameless appearances" rule forbids; it exists only
  because the invalid state can occur at all.
- `SweepInvalidRecordings` (prune backstop) GCs recordings with no files.

The 2026-07-14 feature review reproduced a real path that stranded a
zero-tagset recording (prune blob-loss, `hardDeleteFilesTx` — fixed
2026-07-14). The class remains: every new cascade/dedup/move path is one
forgotten guard away from re-creating the state, and the failure today is
*silent* (row vanishes from every surface until a restart "heals" it lossily).

## For what

Make the invalid state **uncommittable**. A buggy code path then fails loudly
at `COMMIT` (transaction rolled back, HTTP 500 on the offending op, nothing
corrupted or stranded) instead of silently damaging the library and getting
papered over at the next restart. The invariant moves from "tested and swept"
to "structurally impossible", the same step 016/024 took for
`artists.name`/`files.recording_id` NOT NULL.

## Mechanism (spike-verified)

SQLite has no deferred CHECK constraints and no ON-COMMIT triggers, and a
plain FK cannot express "parent must have a child". What it *does* have:
**`DEFERRABLE INITIALLY DEFERRED` foreign keys, checked at COMMIT time**. So
the invariant is encoded by making the recording itself reference one of its
tagsets:

```sql
recordings.primary_tagset_id INTEGER REFERENCES tagsets(id)
    DEFERRABLE INITIALLY DEFERRED   -- + NOT NULL (trigger-emulated, see below)
```

A recording that exists at COMMIT must reference an *existing* tagset —
i.e. it has ≥ 1 tagset. Spike results (throwaway test, session-verified):

| Scenario | Result |
|---|---|
| Commit a recording without any tagset (placeholder never repointed) | **refused at COMMIT** (`FOREIGN KEY constraint failed`) |
| Delete the last tagset of a still-existing recording, commit | **refused at COMMIT** |
| Legit GC: delete the recording itself (tagsets cascade via their FK) | commits fine |
| Legit creation: INSERT recording (placeholder `0`) → INSERT first tagset → UPDATE repoint → COMMIT | commits fine |
| Any intermediate zero-tagset state *inside* an open transaction | legal (deferred), so **no existing flow needs reordering** |
| `ALTER TABLE ADD COLUMN` with the deferred REFERENCES clause, inside one transaction with `foreign_keys=ON` (the migration runner's exact conditions) | works |
| NULL in the column | escapes FK checking → NOT NULL must be emulated with RAISE triggers (the 024 `files.recording_id` precedent) — verified to refuse NULL writes |

Also verified, relevant background: an FK `ON DELETE CASCADE` fires child
`BEFORE DELETE` triggers even with `recursive_triggers=0`, but with the parent
row already gone — which is why the (rejected) trigger-only variant can't
distinguish "last tagset deleted by recording GC" from "last tagset deleted by
a bug" without restructuring `hardDeleteTagsetsTx`, and why it covers neither
creation nor the move paths. The deferred FK covers all of them uniformly.

**Known limitation.** `ALTER TABLE ADD COLUMN` only supports a single-column
FK, so the DB proves the referenced tagset *exists*, not that it *belongs to
this recording*. The full ownership encoding — a composite
`FOREIGN KEY (id, primary_tagset_id) REFERENCES tagsets (recording_id, id)`,
also spike-verified to work and to refuse a foreign-recording primary —
requires rebuilding `recordings`, which the current migration runner cannot do
(each migration runs inside one transaction; `PRAGMA foreign_keys` is a no-op
in-transaction, and dropping an FK-referenced parent under `foreign_keys=ON`
would cascade away every tagset). Upgrading to the composite FK is possible
later by teaching the runner out-of-transaction migrations; not worth it now —
ownership stays covered by app logic + `assertInvariants` + the sweep.

**Failure semantics.** A violation surfaces as a generic
`FOREIGN KEY constraint failed` error from `tx.Commit()`; the transaction is
rolled back (every write path already does `defer tx.Rollback()`). Less
pinpointed than an app-level error — acceptable, because it only fires on
bugs, and the alternative today is silent data damage. Keep
`assertInvariants`, `ReconcileTagsets`, and `SweepInvalidRecordings` as
diagnostics/backstops (they should now never find anything; their log lines
become a bug signal).

## Common groundwork (both options)

### Migration `026_recording_primary_tagset.sql`

Runs in the existing runner (one transaction, FKs ON) — no runner changes.

1. **Heal pre-existing violations** (SQL ports of `ReconcileTagsets` steps
   2–4, `database/recordings.go:157ff` — the migration runs *before* startup
   reconciliation, so it must self-heal):
   - fileless recordings → `DELETE` (tagsets cascade);
   - zero-tagset recordings → insert one filename-derived approved primary
     appearance from the oldest file (same shape as reconcile step 2);
   - no primary → promote `MIN(id)` (reconcile step 4);
   - more than one primary → demote all but `MIN(id)` (reconcile doesn't have
     this step; the migration needs it so the backfill is deterministic).
2. `ALTER TABLE recordings ADD COLUMN primary_tagset_id INTEGER REFERENCES
   tagsets(id) DEFERRABLE INITIALLY DEFERRED;`
3. **Backfill**: `UPDATE recordings SET primary_tagset_id = (SELECT id FROM
   tagsets WHERE recording_id = recordings.id AND is_primary = 1);` (unique
   after step 1).
4. **NOT NULL emulation** (024 precedent — `files_recording_required_*`):
   `BEFORE INSERT` / `BEFORE UPDATE OF primary_tagset_id` RAISE-ABORT triggers
   on NULL.

### New creation pattern

Every `INSERT INTO recordings` gains the placeholder-then-point shape inside
its existing transaction:

```
INSERT INTO recordings (created_at, primary_tagset_id) VALUES (?, 0);
-- … create / move the first tagset …
UPDATE recordings SET primary_tagset_id = ? WHERE id = ?;
```

`0` references no tagset, so a path that forgets the repoint **fails at
COMMIT** — creation is enforced, not just deletion.

### Central helpers

Two small tx-scoped helpers in `database/`, so no call site hand-rolls the
dual write and the mirror cannot drift by construction:

- `setPrimaryTx(ctx, tx, recordingID, tagsetID)` — flips `is_primary`
  (demote-then-promote, as `SetPrimaryTagset` does today) *and* points
  `recordings.primary_tagset_id`.
- `promotePrimaryTx(ctx, tx, recordingID)` — the `repairRecordingTx`
  promotion ("oldest remaining tagset"), extended to update the mirror; also
  re-syncs the mirror when the flagged primary survived but the mirror
  dangles.

### Write-site inventory (audited via grep, 2026-07-14)

Recording creation (4 sites → placeholder-then-point):

| Site | Flow |
|---|---|
| `database/files.go:173` | `InsertFile` — upload creates singleton recording + first tagset |
| `database/approve.go:70` | `ApproveSubmission` force-new — duplicate split into a fresh recording |
| `database/recordings.go:136` | `ReconcileTagsets` step 1 — singleton for a recording-less file |
| `database/recordings.go:461` | `SplitRendition` — split-off file gets its own recording |

Primary-affecting writes (route through the helpers):

| Site | Flow |
|---|---|
| `database/approve.go:78,83` | force-new: moved tagset becomes the new recording's primary; source re-asserted |
| `database/recordings.go:84` | `ResolveRecording` — arriving tagsets demoted when target has a primary |
| `database/recordings.go:181,204` | `ReconcileTagsets` — healing insert; promote-missing-primary |
| `database/recordings.go:472–501` | `SplitRendition` — moved/cloned tagset is the new recording's primary; source demote |
| `database/files.go:890` | `repairRecordingTx` — promotion after members died/moved (all hard-delete cascades funnel here) |
| `database/curate.go:426,450` | `MergeRecordings` — arrivals demoted; target primary re-asserted |
| `database/curate.go:532` | `MoveTagset` — arrival demoted; source re-promoted |
| `database/curate.go:564,569` | `SetPrimaryTagset` — the explicit admin op |
| `database/curate.go:944` | `CreateAppearance` — born non-primary, no mirror change (audit only) |

Tagset hard-deletes (`hardDeleteTagsetsTx`, absorb/merge `deleteTagsetIDsTx`)
already end in `repairRecordingTx`/promotion — they inherit the mirror update
through `promotePrimaryTx`; the audit confirms each path reaches it.

## Option 1 — mirror column (`is_primary` stays the source of truth)

`primary_tagset_id` is an *enforced mirror* of "the `is_primary = 1` tagset".
Every reader — `reprTagset` orderings, curate/metadata queries, the API's
`is_primary` JSON, admin JS — is untouched.

- **Scope**: migration 026 + the two helpers + threading them through the
  ~10 write sites above + tests. No read-side or API/UI changes.
- **What the DB then guarantees**: no recording exists at COMMIT without at
  least one live tagset row (creation, deletion, and dedup paths alike).
- **What it does not guarantee**: that mirror and flag agree, or that the
  mirror's tagset belongs to this recording. Both remain covered by
  `assertInvariants` (extended: `primary_tagset_id` = the recording's
  `is_primary=1` tagset) and the startup/prune repairs. By construction the
  helpers keep them in sync; drift would be a bug the extended invariant test
  catches.
- **Risk**: low. The diff is confined to write paths that already exist and
  already promote primaries; behavior of every read query is unchanged.

## Option 2 — replace `is_primary`

`recordings.primary_tagset_id` becomes the *only* primary marker;
`tagsets.is_primary` is dropped.

- **Everything from Option 1**, plus:
  - migration additionally does `ALTER TABLE tagsets DROP COLUMN is_primary`
    (legal without a rebuild: no index or trigger references it — checked);
  - rewrite the ~29 `is_primary` usages across `database/{files,metadata,
    curate,recordings,approve}.go`: every `ORDER BY t.is_primary DESC` becomes
    `(t.id = r.primary_tagset_id) DESC` with a `recordings` join in the
    `reprTagset` subqueries and list queries (hot browse paths — joins on the
    PK, cheap, but every query changes);
  - the API keeps *emitting* `is_primary` (computed from the join), so
    `admin/recordings.js` and the rest of the UI stay untouched.
- **What it buys beyond Option 1**: "exactly one primary" becomes structural —
  a recording *cannot* have zero or two primary marks, the flag/mirror drift
  class disappears entirely, and the promote-if-lost repair in
  `repairRecordingTx` simplifies to a single mirror repoint.
- **Cost/risk**: a large diff through the hottest read queries for no
  additional zero-tagset enforcement (the actual ask). The exactly-one-primary
  invariant it hardens has never been the one violated in the wild.

## Recommendation

**Option 1 now; Option 2 as an optional later cleanup.** Option 1 is a strict
subset of Option 2 (same column, same FK, same helpers, same migration
skeleton), so nothing is thrown away by starting with the mirror — Option 2
later is "drop the flag, rewrite the readers", with the enforcement already
live in production the whole time.

## Test plan

- **Migration test** (the `migration024_test.go` pattern): seed a pre-026 DB
  with a healthy recording, a zero-tagset recording with files, a
  multi-primary recording, and a fileless recording; migrate; assert healed
  data, backfilled column, and working NULL triggers.
- **Enforcement tests** (raw-SQL transactions, the spike shapes): commit of a
  tagset-less recording refused; delete-last-tagset-and-commit refused;
  placeholder-then-point creation and recording GC commit fine.
- `database_test.go` version/table assertions bump (known migration gotcha);
  `assertInvariants` extended with the mirror check.
- Full `go test ./...` + `-race`, then live-verify the flows that create or
  retire primaries: upload → approve (incl. force-new), merge, split, move,
  set-primary, absorb, trash → delete-forever, prune.
