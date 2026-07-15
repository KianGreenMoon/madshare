# The GC deletion model — unlink, derive, reap, purge

Status: **agreed design** (owner decision 2026-07-15); phases P1 (reaper),
P2 (unlink-only write paths) and P3 (demotions) implemented, P4–P5 pending.
Once implemented, this document is the durable reference for the content
lifecycle; it supersedes the *cascade* deletion philosophy described in
[soft-delete.md](soft-delete.md) and the invariants section of
[files-recording-tagsets.md](files-recording-tagsets.md), and it **replaces**
(archived, see [Rejected alternative](#rejected-alternative-the-cascade-model--deferred-fk)
below) the migration-026 plan for a DB-enforced "every recording has ≥ 1
tagset" invariant.

## Why

The current model deletes by **synchronous cascade**: every hard-delete entry
point must, inside its own transaction, decide "was that the last
appearance/rendition?" and then cascade the recording, its files, its tagsets,
and its blobs in the right order. That discipline is the source of the model's
complexity:

- every new code path (dedup, absorb, merge, move, prune) must re-implement or
  correctly reuse the shared cascade, and *one forgotten guard strands rows
  silently* (the 2026-07-14 review reproduced exactly this class);
- the backstops (`ReconcileTagsets`, `SweepInvalidRecordings`) exist only
  because the invalid states can occur at all;
- the test surface grows with every path × every lifecycle state, because
  correctness lives in each path instead of in one place.

The fix is the model filesystems use: **deleting only removes references;
a single collector reclaims what nothing references — and it reclaims into
quarantine, not into oblivion.**

## The model

Three layers, unchanged in schema, renamed in the mental model:

```
 what a person sees in the library      the recording itself           the bytes on disk
 (appearances — "directory entries")    ("inode")                      (renditions — "data blocks")

  „Du riechst so gut“ ───────────┐
    Herzeleid · track 5          │     ┌───────────────────────┐     ┌── …/ab12…/song.flac   (best)
                                 ├───► │   Du riechst so gut   │ ◄───┤
  „Du riechst so gut“ ───────────┘     │  the audio identity;  │     └── …/cd34…/song.mp3    (320k)
    Best-of compilation · track 2      │  license · access     │
                                       └───────────────────────┘
       many appearances : 1 recording        1 recording : many files
       DELETING = UNLINKING A NAME           NEVER DELETED DIRECTLY — REAPED
```

- **`recordings`** — the identity ("this audio"). Owns license and guest
  access. Has **no lifecycle column and no direct delete operation**; its
  liveness is derived, its removal is the reaper's job alone.
- **`files`** — physical renditions. Needed for dedup (content hash),
  verification, and the quality ladder. **Not a curation object**: the admin
  library manages appearances and recordings; files appear only on
  maintenance surfaces (verify/prune/dedup) and in Trash.
- **`tagsets`** — appearances, the only thing users see, name, and delete.

### The two structural edges (and only two)

| Edge | Meaning | Delete action |
|---|---|---|
| `files.recording_id` (NOT NULL) | every blob is a rendition of exactly one recording | none — rows die only via purge |
| `tagsets.recording_id` (NOT NULL) | every appearance names exactly one recording | `ON DELETE CASCADE` (belt-and-braces for reaper husk removal) |

Everything else is a **preference or provenance pointer**, never structure:

- `recordings.preferred_file_id` (nullable, SET NULL) — optional manual
  override of the auto-ranked best rendition (`RankRenditions`).
- `tagsets.is_primary` — **demoted from invariant to preference.** The
  representative appearance of a recording (for file rows, recording cards,
  duplicates) is *derived*: **oldest live appearance**; the flag is an
  optional manual override, exactly mirroring `preferred_file_id`. There is
  no "exactly one primary" rule, no repair, no enforcement: if the flagged
  appearance dies, the derived default silently takes over. (The existing
  `reprTagset` ordering — live first, then flag, then oldest — already
  implements this; only the enforcement/repair code is deleted.)
- `tagsets.origin_file_id` (nullable, SET NULL) — **the submission pairing**:
  which uploaded blob these tags were read from. Meaningful only while
  `review_state ≠ approved` (the moderation queue, duplicate classification,
  absorb decisions act on it). After approval it is inert audit data that no
  logic may read. Consequences: plain `SET NULL` is correct and *more honest*
  than re-pointing (a re-pointed origin claims the tags were read from a blob
  they were not); the entire repoint-on-hard-delete machinery is deleted.
  One guarded rule replaces it: **a draft dies with its origin blob** —
  purging a file that is some draft's origin discards that draft (a
  submission without its bytes is meaningless).

### Zero schema migration

The GC model requires **no schema change**. All columns and FKs above already
exist with the required actions; what changes is code semantics (and what code
gets deleted). Migration 026 (`recordings.primary_tagset_id`) is never
created.

### The DB diagram (target state)

Structurally identical to today's triangle — the differences are all in the
annotations: no `primary_tagset_id`, no "≥ 1" invariants (replaced by reaper
convergence), and the two non-structural pointers demoted to
preference/provenance.

```
                          ┌─────────────────────────────────┐
                          │           recordings            │
                          │  the identity ("inode"): owns   │
                          │  license/access; NO lifecycle   │
                          │  column, NO direct delete op —  │
                          │  empty husk removed by reaper P3│
                          ├─────────────────────────────────┤
                          │ id                     PK       │
                          │ created_at                      │
                   ┌──────│ preferred_file_id      FK ──────│──┐  PREFERENCE only: manual
                   │      │ license                         │  │  best-rendition override
                   │      │ guest_playable                  │  │  (nullable, ON DELETE SET
                   │      │ guest_playable_manual           │  │  NULL; NULL = quality ladder)
                   │      └─────────────────────────────────┘  │
                   │          ▲                    ▲           │
  files.recording_id          │                    │        tagsets.recording_id
  NOT NULL — structural       │                    │        NOT NULL, ON DELETE CASCADE
  edge 1: every blob is a     │                    │        — structural edge 2: every
  rendition of exactly one    │                    │        appearance names exactly one
  recording; rows die only    │                    │        recording; no "≥ 1" rule —
  via PURGE                   │                    │        zero rows = garbage, reaped
                   ▼          │                    │           ▼
┌───────────────────────────────┐       ┌──────────────────────────────────────┐
│            files              │       │               tagsets                │
│  rendition ("data block") —   │       │  appearance ("directory entry") —    │
│  a storage object, NOT a      │       │  the only thing users see, name,     │
│  curation object              │       │  and delete                          │
├───────────────────────────────┤       ├──────────────────────────────────────┤
│ id                         PK │       │ id                               PK  │
│ hash     (content address)    │       │ recording_id  NOT NULL   FK ─────────│─→ recordings
│ recording_id  NOT NULL FK ────│─→ rec │ title/artist/album/…     (raw tags)  │
│ recording_pinned              │       │ artist_id / album_artist_id /        │
│ storage_backend               │       │   album_id      (resolved overlay)   │
│ deleted_at  = TRASH mark      │       │ review_state / review_note /         │
│   (Trash › Files — the        │       │   submitted_at / created_by          │
│    quarantine window)         │       │ deleted_at  = TRASH mark             │
│ uploaded_by, mime, size, …    │       │   (Trash › Appearances)              │
└───────────────────────────────┘       │ is_primary  — PREFERENCE only:       │
              ▲                         │   manual override of the derived     │
              │                         │   default (oldest live appearance);  │
              │                         │   no exactly-one rule, no repair     │
              │                         │ origin_file_id            FK ────────│─┐
              │                         │ created_at                           │ │
              │                         └──────────────────────────────────────┘ │
              │                                                                  │
              └──────────────────────────────────────────────────────────────────┘
                origin_file_id → files.id   (nullable, ON DELETE SET NULL)
                SUBMISSION PAIRING only: meaningful while review_state ≠
                'approved' (moderation queue, duplicate classify, absorb);
                after approval it is inert audit data — never re-pointed,
                never a delete key; a draft dies with its origin blob
```


## Operations: unlink and mark only

Every user- or admin-facing delete touches **exactly one kind of row** and
never cascades:

| Operation | Effect | Reversible? |
|---|---|---|
| user/uploader/admin deletes an appearance | `tagsets.deleted_at` set (Trash › Appearances) | restore re-enters prior review state |
| admin removes a rendition | `files.deleted_at` set (Trash › Files) | restore |
| admin trashes a recording | trash **all its appearances** (bulk of the row above — not a recording op) | restore |
| "Delete forever" / empty trash | **purge**: delete trashed rows (+ blob with the last files row of a hash) | no — this is the only destructive op |

There is no "delete recording" and no "delete file row" primitive in any
request path. Whatever a delete leaves unreferenced is the reaper's problem —
by design, so the correctness of every path is trivial to see.

## Derived visibility (already implemented — unchanged)

An appearance is library-visible iff it is live and approved **and its
recording has at least one live blob** (the shared `visibleTagset` predicate,
`database/tagsets.go`). Soft-removing the last rendition hides every
appearance of the recording instantly and mutation-free ("dormant");
restoring the rendition re-surfaces them. Nothing in the GC model changes
this — it *is* the model's read side.

## The reaper

One implementation, idempotent, short transactions. Runs at startup, is
nudged after every delete/purge/move operation, and before reporting on
maintenance surfaces. It consolidates and replaces `ReconcileTagsets`'s
healing and `SweepInvalidRecordings`.

```
                    unlink / remove / purge (any single-row op)
                                     │ nudge
                                     ▼
            ┌───────────────────────────────────────────────────────┐
            │ REAPER — demotes garbage to Trash; NEVER destroys     │
            │                                                       │
            │  P1  recording has NO tagset rows,                    │
            │      but non-trashed files      → soft-remove them    │
            │                                   (Trash › Files)     │
            │  P2  recording has NO file rows,                      │
            │      but non-trashed tagsets    → trash them          │
            │                                   (Trash › Appearances)│
            │  P3  recording has NO tagset rows AND NO file rows    │
            │                                 → delete the husk row │
            └───────────────────────────────────────────────────────┘

            ┌───────────────────────────────────────────────────────┐
            │ PURGE — the only destroyer; touches ONLY trashed rows │
            │ ("Delete forever", "Empty trash", optional age policy)│
            │  deletes trashed tagset/file rows + reclaims blobs;   │
            │  discards drafts whose origin blob it purges; nudges  │
            │  the reaper afterwards                                │
            └───────────────────────────────────────────────────────┘
```

Object lifecycle, from any row's point of view:

```
             unlink / remove (one row)                explicit purge / empty trash / age
  ┌────────┐ ─────────────────────────► ┌─────────┐ ─────────────────────────► ┌────────┐
  │  LIVE  │                            │  TRASH  │                            │  GONE  │
  └────────┘ ◄───────────────────────── └─────────┘                            └────────┘
                      restore
```

**The safety invariant** (replacing the old "≥ 1 file / ≥ 1 tagset / exactly
one primary" trio): *the reaper only demotes; only purge destroys; purge only
touches rows already in Trash.* A hypothetical bug that wrongly drops
references can therefore at worst move things into Trash — visible,
restorable — never silently destroy bytes. This is the GC model's answer to
the cascade model's loud-at-COMMIT guarantee: instead of making bad states
uncommittable, it makes their blast radius reversible.

Legal steady states after a reap: **reachable** (≥ 1 tagset row), **dormant**
(live appearances, all blobs soft-removed — restorable), **quarantined**
(no tagset rows, all files trashed — restorable from Trash › Files as a
re-staged draft, or purgeable), **gone**.

Worked example — the old "delete the last appearance forever" cascade,
staged: purge the trashed tagset row → reaper P1 soft-removes the
recording's files into Trash › Files → their purge (explicit or aged)
reclaims the blobs → reaper P3 deletes the recording husk. Same end state as
today's owner rule ("nothing left to reach it = prune everything"), but every
intermediate step is a single obvious write and the destructive one is
explicit. Where the immediate-disk-reclaim UX matters, an endpoint may run
`purge → reap → purge` synchronously — a composition of the same primitives,
not a new cascade path.

**Prune keeps its separate job** — reconciling disk against DB (dangling
blobs, orphan directories, link health). The reaper is purely DB-internal
reference collection. Prune's blob-loss handling simplifies: a lost blob's
files row is purged, referencing drafts are discarded, `origin_file_id` of
approved appearances goes NULL — no re-pointing.

## Rejected alternative: the cascade model + deferred FK

The previous plan (`docs/plans/recording-tagset-db-invariant.md`, committed
0353149, now archived by deletion) hardened the cascade philosophy instead:
`recordings.primary_tagset_id` as a `DEFERRABLE INITIALLY DEFERRED` FK made a
zero-tagset recording uncommittable. Superseded because the GC model removes
the invariant rather than enforcing it — a zero-reference recording is not
corruption but garbage awaiting collection, and the deferred FK would forbid
exactly the states the reaper exists to collect. The two philosophies cannot
be mixed.

Durable spike knowledge from that plan, kept for the record (verified 2026-07
on modernc.org/sqlite, `foreign_keys=ON`, `_txlock=immediate`, inside the
migration runner's transaction shape): single-column deferred FKs added via
`ALTER TABLE ADD COLUMN` are checked at COMMIT and do refuse
parent-without-child commits; NULL escapes FK checks (NOT NULL needs RAISE
triggers, the 024 precedent); composite deferred FKs
(`(id, primary_tagset_id) → tagsets(recording_id, id)`) also work but require
a table rebuild the one-transaction migration runner cannot do; FK cascades
fire child `BEFORE DELETE` triggers with the parent row already gone.

## Implementation outline

Each phase leaves the system running; tests accompany each phase.

- **P1 — the reaper (DONE 2026-07-15).** `database/reap.go`: `db.Reap`
  (the three passes as guarded bulk statements in one transaction) + the
  `Reaper` single-flight nudge runner; startup runs reap instead of the
  removed `ReconcileTagsets`; `SweepInvalidRecordings` folded into the
  post-prune backstop (`Repository.Reap`). `assertInvariants` rewritten to
  the safety invariant + convergence ("after reap: no zero-reference
  recording retains non-trashed children"); the exactly-one-primary check
  and the startup primary promotion dropped (preference, not invariant).
- **P2 — unlink-only write paths (DONE 2026-07-15).** `database/purge.go`
  holds the only row-destroyers: `deleteTagsetRowsTx` / `deleteFileRowsTx`
  (with the draft-dies-with-its-blob rule) / `reclaimAbandonedRecordingsTx`,
  composed by `purgeTagsetsTx` (the sanctioned purge → reap → purge, so
  appearance delete-forever still frees bytes immediately), plus
  `reapRecordingsTx` — the in-tx scoped reap every op that may leave a
  recording unreferenced calls on the recordings it touched (move, merge,
  split, dedup, file purge). Being transactional, this is *stronger* than the
  async nudge, which remains for paths that cannot reap inline. The cascade
  web (`repairRecordingTx`, `hardDeleteFilesTx`, `hardDeleteTagsetsTx`,
  `deleteRecordingFilesTx`) and the origin re-point machinery are deleted.
  Two deliberate behavior changes: purging a recording's **last file** now
  trashes its appearances instead of destroying the recording (catalog
  entries survive blobless in Trash › Appearances — restorable, re-stageable
  when the bytes return), and a move/dedup that empties a recording trashes
  stranded appearances instead of cascade-eating them.
- **P3 — demotions (DONE 2026-07-15).** `is_primary` is never auto-set or
  auto-demoted anymore: `InsertFile` seeds 0, the split/force-new/regroup
  promote+demote sweeps are gone, and the reported "primary" of a recording
  (`RecordingDetail`, recording cards) is *derived* — live first, then the
  manual flag, then oldest — with `SetPrimaryTagset` as the only writer.
  The hash-addressed soft-delete/restore machinery was deleted outright
  (`SoftDeleteFileByHash`, `Bulk{SoftDelete,Restore,HardDeleteTrashed}ByHashes`,
  `HardDeleteTrashedFileByHash`, the files-rooted Trash listing family,
  `BulkUpdateFileMetadata`, hash-addressed bulk access setters, plus
  `DELETE /api/admin/files/{hash}` and `POST /api/admin/files/bulk`):
  every surface now speaks tagset/rendition ids — the entity view's deletes
  go through `POST /api/admin/appearances/bulk` (whose filter gained
  `artist_id`/`album_id` pins), the duplicates page's per-rendition delete
  through `POST /api/admin/renditions/{id}/remove`. The two genuinely
  byte-rooted entry points (re-upload restore, uploader restore) stay
  hash-keyed at the boundary but resolve their rows via the recording edge:
  `RestoreFileByHash` restores every trashed appearance of the blob's
  recording *and* revives the rendition, `StageRestoredFile` re-stages by
  recording, `GetFileByHash` derives lifecycle from the representative
  appearance (either trash mark counts), and prune's `ListFileRefs` scans
  live renditions by `files.deleted_at` — no delete/restore logic reads
  `origin_file_id` any longer (it remains submission pairing + inert audit
  data, per the rule above).
- **P4 — UI.** Retire the Files lens from Full Library curation; keep
  Trash › Files (the quarantine window) and a maintenance surface
  (verification, prune, dedup stats, rare per-rendition removal).
- **P5 — docs.** Fold the surviving content of `soft-delete.md` into this
  document's terms; update `files-recording-tagsets.md`'s invariants section;
  update `recordings.md` / `recording-tagsets.md` / `moderation.md`
  references to cascades.

Open (small) decisions, defaulted here, overridable at implementation time:
trash **age-based** auto-purge is *off* by default (purge stays explicit);
the reaper nudge is asynchronous with the synchronous `purge→reap→purge`
composition reserved for the delete-forever endpoints.
