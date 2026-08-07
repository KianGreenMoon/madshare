# The GC deletion model — unlink, derive, reap, purge

Status: **implemented** (owner decision 2026-07-15; all phases P1–P5 done).
This document is the durable reference for the content lifecycle — soft-delete
marks, Trash, restore, and permanent deletion. It absorbed the former
`soft-delete.md` (deleted; its surviving content is the
[Trash section](#trash--the-quarantine-at-three-grains) below), supersedes the
old invariants section of
[files-recording-tagsets.md](files-recording-tagsets.md), and **replaces**
(archived, see [Rejected alternative](#rejected-alternative-the-cascade-model--deferred-fk)
below) the migration-026 plan for a DB-enforced "every recording has ≥ 1
tagset" invariant.

## Why

The previous model deleted by **synchronous cascade**: every hard-delete entry
point had to, inside its own transaction, decide "was that the last
appearance/rendition?" and then cascade the recording, its files, its tagsets,
and its blobs in the right order. That discipline was the source of the
model's complexity:

- every new code path (dedup, absorb, merge, move, prune) had to re-implement
  or correctly reuse the shared cascade, and *one forgotten guard stranded rows
  silently* (the 2026-07-14 review reproduced exactly this class);
- the backstops (`ReconcileTagsets`, `SweepInvalidRecordings`) existed only
  because the invalid states could occur at all;
- the test surface grew with every path × every lifecycle state, because
  correctness lived in each path instead of in one place.

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

## Derived visibility — the read side

The system never destroys content on an admin delete. It sets one of **two
soft-delete marks**, and whether something is *in the library* or *in the
Trash* is **derived** from them — there is no third "trashed" flag or state
to keep in sync:

| Mark | Grain | Set/cleared by | Meaning |
|---|---|---|---|
| `tagsets.deleted_at` | appearance (catalog unit) | Trash/Restore of an appearance; trashing a recording trashes all of its appearances | this **appearance** is trashed |
| `files.deleted_at` | file / rendition (blob) | `RemoveRendition` / `RestoreRendition` (bytes kept on disk) | this **blob** is soft-removed |

Everything else — "is this track in the library?", "is this recording
trashed?", "is this blob dormant?" — is a **query** over those two marks.
Neither mark ever cascades on set. Every user-facing (tagset-rooted) listing
gates an appearance on the shared `visibleTagset` predicate
(`database/tagsets.go`):

```
m.deleted_at IS NULL
  AND m.review_state = 'approved'
  AND EXISTS (SELECT 1 FROM files sf
              WHERE sf.recording_id = m.recording_id AND sf.deleted_at IS NULL)
```

An appearance is in the library **iff** it is approved, not trashed, **and its
recording still has at least one surviving rendition to play**. The moment the
**last** surviving file of a recording is soft-removed, that `EXISTS` goes
false and **every appearance of the recording drops out of the library** — no
separate action needed. Such a recording is called **dormant**; "dormant" is a
*label for a derived condition* (`recording has zero surviving files`), not a
stored state. Restoring any rendition makes `EXISTS` true again and the
appearances return.

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

### Don't hand P2 a husk that still has appearances

P2 is a demotion, and the invariant above promises demotions are reversible.
For appearances it is **not**, and this is the one place the guarantee has a
hole: restoring a P2-trashed appearance puts a live row back on a recording
with no file rows, which is P2's own trigger, so the next reap trashes it
again. It bounces. Escaping needs a `Move…` onto a recording that still has a
rendition *before* the restore — two steps, in that order, and nothing in the
UI says so.

So the rule is upstream: **an operation that takes a recording's last rendition
away must not leave appearances behind.** `origin_file_id` is provenance, not
structure (recording-tagsets P7), so "the appearances that move" is decided by
recording membership, never by which blob a row was read from — a hand-authored
appearance (`CreateAppearance`, origin NULL) and one `MoveTagset` re-homed here
both describe this recording's audio.

The two operations that can empty a recording answer it differently, on purpose
(owner decision, 2026-08-07):

- **`ResolveRecording` moves them along.** The fingerprint proves the target is
  the same audio, so its appearances belong there; the caller is the background
  analysis worker, so there is nobody to ask — and the startup backfill can
  regroup a whole library the first time `fpcalc` is installed. Identity
  collisions on the target are **allowed**, not deduped: destroying a curated
  row to keep an identity set tidy is the wrong trade on an unattended path.
  `/admin/duplicates` is the cleanup surface.
- **`SplitRendition` refuses** (`SplitRenditionOutcome.StrandedAppearances` →
  409). A split *asserts* the rendition is a different composition, so carrying
  a curator's hand-added appearance across would file it under the very thing
  the moderator just declared separate. A human is present, so ask instead of
  guessing.

Both check file **rows**, not live renditions: a recording keeping a
soft-removed sibling is *dormant*, not a husk, so P2 never fires on it.

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

## Trash — the quarantine at three grains

**Trash is the complement of the library.** The same underlying removal is
visible through three lenses on the Trash scope of `/admin/library`
(sub-mode switch **Appearances · Recordings · Files**); an admin restores from
whichever one they happen to be looking at, and it always lands the item back
in a playable library state. The three perspectives are **never merged into
one list** — each is its own view over the same facts, and they overlap by
design.

### 1. Appearances (default)

**Membership** — individually trashed appearances
(`tagsets.deleted_at IS NOT NULL`). The listing is rooted `FROM tagsets`: one
row per appearance, keyed by **tagset id**; a blobless appearance (its
recording quarantined or its bytes purged) simply has an empty `hash`/`url` —
no preview, no size — and is restored / purged by its id like any other.

- **Restore** — `RestoreTagset`: the appearance re-enters its prior review
  state ([moderation.md](moderation.md)).
- **Delete forever** — the purge composition (`purgeTagsetsTx`,
  purge → reap → purge): the trashed appearance rows go; every recording that
  lost its last appearance is reclaimed in the same transaction — files,
  blobs, and husk — so delete-forever frees the bytes immediately. Refuses a
  live appearance (409). A blob hosting several appearances is only reclaimed
  once the last of them is purged.
- **Edit** — tags only (`metadata.edit`). Access (license / guest) is a
  recording property and is not offered here.

> **Dormant appearances are handled by Recordings + Files, not here.** A
> dormant recording's appearances have `tagsets.deleted_at` still `NULL`, so
> this lens (whose base predicate *is* that mark) does not list them. The
> dormant recording surfaces under **Recordings** and its removed blob under
> **Files** — the three lenses cover both soft-delete marks; Appearances stays
> scoped to its own. That is deliberate: dormancy is a recording-level
> state, better shown where the recording is.

### 2. Recordings

**Membership** — recordings entirely out of the library: at least one
appearance was once approved, but **none is visible now** (all trashed and/or
dormant). Recordings that only ever had drafts are excluded — moderation, not
Trash. This is the whole-recording bin; its trashed appearances also show
under Appearances, its removed files under Files. A **dormant** recording —
live tagsets, no surviving file — is reachable *only* here and under Files,
which is the point: this lens is where the whole thing comes back or goes.

- **Restore** — `RestoreRecording`: un-trash every trashed appearance **and**
  ensure ≥ 1 rendition survives (restore the best removed one if none) →
  fully back in the library.
- **Delete forever** — `HardDeleteRecording`: the count-aware confirm, then
  the same purge composition over every appearance of the recording (an
  already appearance-less husk is reclaimed directly).

### 3. Files

**Membership** — soft-removed blobs (`files.deleted_at IS NOT NULL`): removed
renditions and absorbed/dormant blobs. The file grain — **the quarantine
window** of the reaper diagram above.

- **Restore** — `RestoreRendition`: clear the mark; any dormant appearances of
  its recording re-enter the library automatically (`visibleTagset`).
- **Delete forever** — `HardDeleteRemovedFile`: purge the row and its bytes;
  drafts whose origin this blob was are discarded (a draft dies with its
  origin blob), approved appearances just lose their inert provenance pointer
  (`origin_file_id` → NULL — never re-pointed). The scoped reap then converges
  the recording: if this was its **last** file row, its appearances are
  **trashed** (Trash › Appearances), never destroyed — the catalog entry
  survives blobless and restorable, only the bytes are gone.

### Permanent delete lives *only* in Trash

Every other surface does **soft** operations only — the library lenses,
`/admin/duplicates`, and the Recordings curation lens trash appearances or
remove renditions; nothing outside the Trash scope destroys a row. One rule,
easy to reason about: **you remove things anywhere; you destroy them in one
place.** Which lens an admin used to remove or restore is irrelevant — the
derived model keeps every surface consistent.

### Endpoints

`file.delete`-gated under `/api/admin/` (Appearances bulk also accepts
`metadata.edit` for its edit action). Select-all-N-matching uses `all:true`
([docs/api/bulk.md](../api/bulk.md)).

| Perspective | List | Restore | Delete forever | Bulk |
|---|---|---|---|---|
| Appearances | `GET /trash` | `POST /tagsets/{id}/restore` | `DELETE /tagsets/{id}` | `POST /trash/bulk` (`tagset_ids`) |
| Recordings | `GET /trash/recordings` | `POST /recordings/{id}/restore` | `DELETE /recordings/{id}` | `POST /trash/recordings/bulk` |
| Files | `GET /trash/files` | `POST /renditions/{id}/restore` | `DELETE /renditions/{id}` | `POST /trash/files/bulk` |

### Audit

| Action | Trigger |
|---|---|
| `file.trash` / `appearance.bulk_trash` | soft delete appearances (move to Trash) |
| `rendition.remove` / `rendition.restore` | soft-remove / restore a blob |
| `recording.trash` / `recording.restore` (+ `bulk_`) | whole-recording soft trash / restore |
| `appearance.restore` / `appearance.bulk_restore` | restore appearances from Trash |
| `appearance.delete` / `appearance.bulk_delete` | purge appearances |
| `recording.delete` / `recording.bulk_delete` | purge recordings |
| `file.delete` / `file.bulk_delete` / `file.bulk_restore` | purge / bulk-restore removed blobs |

### Related upload behaviour

A re-upload of a trashed file restores it instead of duplicating (subject to
the trash-restore policy, `docs/api/upload.md`). With moderation configured,
an upload-initiated restore of a previously *approved* file is demoted to the
restorer's draft (`StageRestoredFile`) rather than republishing — restores
must not bypass review ([moderation.md](moderation.md)).

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
- **P4 — UI (DONE 2026-07-16).** The Files lens is retired from Full Library
  curation (`library-files.js` deleted; the scope is three lenses: By entity ·
  All Appearances · Recordings). Its exclusive backend went with it:
  `POST /api/admin/renditions/bulk` + `LiveFileIDs` + `BulkRemoveRenditions`
  (the per-id `renditions/{id}/{remove,restore}` pair stays — the Recordings
  lens's renditions arm and `/admin/duplicates` use it). Blobs now surface
  only where the model says they belong: the maintenance surfaces (prune,
  duplicates, the renditions arm) and **Trash › Files** — the quarantine
  window, unchanged.
- **P5 — docs (DONE 2026-07-16).** `soft-delete.md` folded into this document
  (the Trash section above) and deleted; `files-recording-tagsets.md`'s
  invariants section rewritten to reaper convergence; the cascade references
  in `recordings.md` / `recording-tagsets.md` / `moderation.md` /
  `prune-job.md` / `file-list-scaling.md` updated to GC terms.

Open (small) decisions, defaulted here, overridable at implementation time:
trash **age-based** auto-purge is *off* by default (purge stays explicit;
if ever wanted: a `[storage] trash_ttl_days = 0` sweep purging trashed rows
older than the TTL — design when implementing); the reaper nudge is
asynchronous with the synchronous `purge→reap→purge` composition reserved
for the delete-forever endpoints.
