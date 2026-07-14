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
        ── has many ───┼──────────┘                  └──────────┼───── 1 recording
        renditions     │  files.recording_id           tagsets.recording_id
        (blobs)        │  NOT NULL (trigger-           NOT NULL,
                       │  emulated, mig 024)           ON DELETE CASCADE
                       │  no delete action —                    │      1 recording
                       │  rows die only via purge;              │      ── has many ──
                       │  the reaper converges                  │      appearances
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
                SUBMISSION PAIRING: "which blob these tags were read from" —
                meaningful while review_state ≠ 'approved', inert audit data
                after; never re-pointed, never a delete key
```

## The four edges

| FK | Cardinality | Delete action | Meaning |
|---|---|---|---|
| `files.recording_id → recordings.id` | many files : 1 recording | none — rows die only via purge | Every blob is a **rendition** of exactly one recording. `NOT NULL` is emulated by RAISE triggers (migration 024; a column rebuild is blocked by inbound FKs + the in-transaction migration runner). There is deliberately no SQL delete action: file rows are destroyed only by the purge primitives (`database/purge.go`), and the reaper deletes a recording row only once it has no rows of either kind — a dangling rendition cannot exist. |
| `tagsets.recording_id → recordings.id` | many tagsets : 1 recording | `ON DELETE CASCADE` | Every **appearance** (catalog entry: descriptive tags + review/trash lifecycle) belongs to exactly one recording. The only DB-level cascade in the triangle — belt-and-braces under the GC model, since the reaper only removes recording rows that have no tagsets left anyway. |
| `tagsets.origin_file_id → files.id` | many tagsets : 0..1 file | `ON DELETE SET NULL` | **The submission pairing** — the blob these tags were read from. Meaningful only while `review_state ≠ 'approved'` (moderation queue, duplicate classification, absorb decisions); after approval it is inert audit data kept for future federation attribution. It is *not* "the file this tagset describes": playback resolves through the recording's quality ladder, and after merge/absorb a blob can host several appearances, an appearance can point at a soft-removed blob, or (hand-authored appearances, P7d) at nothing. On file purge it simply goes NULL — never re-pointed (that would fake provenance), never a delete key; the one exception is that a **draft dies with its origin blob** (purge trashes it). |
| `recordings.preferred_file_id → files.id` | 1 recording : 0..1 file | `ON DELETE SET NULL` | Nullable manual override of the auto-ranked best rendition (`RankRenditions`). Unsurfaced in v0; NULL = use the ladder; pruning the chosen file falls back to the ladder rather than dangling. |

## Convergence rules (the GC model)

The old "≥ 1 file / ≥ 1 tagset / exactly one primary" invariants are replaced
by the [GC deletion model](gc-model.md)'s convergence rules, checked by
`assertInvariants` (`database/lifecycle_test.go`) and converged by the reaper
(startup + prune backstop `db.Reap`, in-tx `reapRecordingsTx` on every op
that may leave a recording unreferenced):

1. A recording with **no tagset rows** is unreachable garbage → its files are
   quarantined into Trash › Files.
2. A recording with **no file rows** has nothing to play → its appearances
   are trashed into Trash › Appearances.
3. A recording with **no rows of either kind** is a husk → the row is
   removed.

Safety invariant: the reaper only demotes; only the purge primitives
(`database/purge.go`) destroy rows, and destruction is confined to
delete-forever / prune paths. `is_primary` is a preference, not an invariant:
the representative appearance derives as oldest-live (`reprTagset`
precedence: live first, then the optional flag, then oldest). The formerly
planned DB-enforced deferred FK (`recordings.primary_tagset_id`) is
superseded and will not be built.

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
  whose renditions are all soft-removed is *dormant*, and a zero-reference
  one is garbage the reaper demotes into Trash (see
  [gc-model.md](gc-model.md)).
