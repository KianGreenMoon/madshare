# files · recordings · tagsets — the schema triangle

The entity-relationship reference for the core content model (as of migration
`025_playlist_tagsets.sql`). Design rationale lives in
[Recordings](recordings.md) (same-audio grouping & renditions) and
[Recording tagsets](recording-tagsets.md) (appearances, lifecycle, ops); this
page is the one-look map of the tables, foreign keys, and multiplicities those
designs produce.

## Diagram

```
                              ┌──────────────────────────────┐
                              │          recordings          │
                              │  "the same audio" (overlay)  │
                              ├──────────────────────────────┤
                              │ id                    PK     │
                              │ created_at                   │
                       ┌──────│ preferred_file_id     FK ────│──┐  manual "best rendition"
                       │      │ license                      │  │  override (nullable,
                       │      │ guest_playable               │  │  ON DELETE SET NULL)
                       │      │ guest_playable_manual        │  │
                       │      └──────────────────────────────┘  │
                       │          ▲                  ▲          │
        1 recording    │          │                  │          │
        ── has ≥1 ─────┼──────────┘                  └──────────┼───── 1 recording
        renditions     │  files.recording_id           tagsets.recording_id
        (blobs)        │  NOT NULL (trigger-           NOT NULL,
                       │  emulated, mig 024)           ON DELETE CASCADE
                       │  no delete action —                    │      1 recording
                       │  app cascades (repair/GC)              │      ── has ≥1 ──
                       │                                        │      appearances
                       ▼                                        ▼      (catalog entries)
┌───────────────────────────────┐                ┌───────────────────────────────────┐
│             files             │                │              tagsets              │
│  one stored blob (rendition)  │                │  one appearance: descriptive tags │
├───────────────────────────────┤                │  + review/trash lifecycle         │
│ id                         PK │                ├───────────────────────────────────┤
│ hash      (content address)   │                │ id                             PK │
│ recording_id           FK ────│─→ recordings   │ recording_id  NOT NULL   FK ──────│─→ recordings
│ recording_pinned              │                │ title/artist/album/... (raw tags) │
│ storage_backend               │                │ artist_id / album_artist_id /     │
│ deleted_at  (soft-removed     │                │   album_id     (resolved overlay) │
│              blob, Files      │                │ review_state / review_note /      │
│              lens Trash)      │                │   submitted_at / created_by       │
│ uploaded_by, mime, size, ...  │                │ deleted_at   (appearance Trash)   │
└───────────────────────────────┘                │ origin_file_id           FK ──────│─┐
              ▲                                  │ is_primary                        │ │
              │                                  │ created_at                        │ │
              │                                  └───────────────────────────────────┘ │
              │                                                                        │
              └────────────────────────────────────────────────────────────────────────┘
                origin_file_id → files.id   (nullable, ON DELETE SET NULL)
                PROVENANCE ONLY: "which blob these tags were read from" —
                re-point target, never a delete key
```

## The four edges

| FK | Cardinality | Delete action | Meaning |
|---|---|---|---|
| `files.recording_id → recordings.id` | many files : 1 recording (**≥ 1 required**) | none — app cascades | Every blob is a **rendition** of exactly one recording. `NOT NULL` is emulated by RAISE triggers (migration 024; a column rebuild is blocked by inbound FKs + the in-transaction migration runner). There is deliberately no SQL delete action: a recording is only deleted after its files are gone (`repairRecordingTx`, `deleteRecordingFilesTx`), so a dangling rendition cannot exist. |
| `tagsets.recording_id → recordings.id` | many tagsets : 1 recording (**≥ 1 required**) | `ON DELETE CASCADE` | Every **appearance** (catalog entry: descriptive tags + review/trash lifecycle) belongs to exactly one recording. This is the only real DB-level cascade in the triangle — deleting a recording takes its appearances with it, which is what makes "recording GC" a single `DELETE FROM recordings`. |
| `tagsets.origin_file_id → files.id` | many tagsets : 0..1 file | `ON DELETE SET NULL` | **Provenance only** — the blob these tags were read from, kept for audit / future federation attribution. It is *not* "the file this tagset describes": playback resolves through the recording's quality ladder, and after merge/absorb a blob can host several appearances, an appearance can point at a soft-removed blob, or (hand-authored appearances, P7d) at nothing. Rule: on any file hard-delete it is a **re-point target** (moved to a surviving rendition of the tagset's own recording), never a delete key. |
| `recordings.preferred_file_id → files.id` | 1 recording : 0..1 file | `ON DELETE SET NULL` | Nullable manual override of the auto-ranked best rendition (`RankRenditions`). Unsurfaced in v0; NULL = use the ladder; pruning the chosen file falls back to the ladder rather than dangling. |

## Invariants (the "hardlink" rules)

Checked by `assertInvariants` (`database/lifecycle_test.go`), repaired by
`ReconcileTagsets` (startup) and `SweepInvalidRecordings` (prune backstop):

1. Every recording has **≥ 1 file** (something to play). Last file gone → the
   recording is GC'd, its tagsets cascade.
2. Every recording has **≥ 1 tagset** (something to list it in the catalog).
   Last *live* appearance hard-deleted → the recording and all its files go
   with it (owner decision: "nothing left to reach it = prune everything").
3. Every recording has **exactly one `is_primary` tagset** — the default
   appearance for files-rooted surfaces (`reprTagset` precedence: live first,
   then primary, then oldest).

Invariants 1–3 are application-enforced (shared cascades:
`hardDeleteTagsetsTx` / `hardDeleteFilesTx` / `repairRecordingTx` — every
hard-delete entry point must go through them; a second code path is how
orphans happen). **Successor design (agreed 2026-07-15, not yet
implemented):** the [GC deletion model](gc-model.md) replaces the cascade
philosophy — zero-reference states become collectable garbage instead of
forbidden corruption, invariants 1–3 dissolve into the reaper's convergence
rules, and the formerly planned DB-enforced deferred FK
(`recordings.primary_tagset_id`) is superseded and will not be built.

## Lifecycle state, per table

Trash and review are **not** on the same table, and that placement is load-bearing:

- `tagsets.review_state` / `tagsets.deleted_at` — the appearance is the
  reviewable, trashable catalog unit (draft → submitted → approved; Trash
  Appearances lens). See [moderation.md](moderation.md),
  [soft-delete.md](soft-delete.md).
- `files.deleted_at` — a **soft-removed blob** (rendition pulled from the
  ladder; Trash › Files lens). Independent of appearance state: absorb keeps
  the appearance live while soft-removing its origin blob.
- `recordings` has no lifecycle column — its liveness is derived: visible
  appearances make it browsable, live files make it playable; a recording
  whose renditions are all soft-removed is *dormant*, one that violates
  invariant 1 or 2 is invalid and gets repaired/GC'd.
