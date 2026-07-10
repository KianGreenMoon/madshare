# Recording tagsets — multiple metadata appearances per audio

**Status:** ✅ **Design decided (owner review 2026-07-04). P0 (migration 024),
P1 (library & addressing, migration 025), P2 (lifecycle & GC), P3 (operations
layer + absorb), and P4 (review & upload rework) are built.**
Tagsets exist, `media_metadata` is tech-only, review/trash live on the tagset,
license/guest on the recording, every file has a recording; the library lists
tagsets and a track is addressed by `tagset_id` end-to-end (browse/search rows,
player queue, hearts, playlists, renditions), with the play URL resolved
server-side to the ladder-best surviving rendition. The hardlink lifecycle is
live: the Trash permanent-delete cascades from the tagset (non-last drops just
the appearance and keeps the blob; last takes the recording + all its files),
renditions can be soft-removed (last one → dormant recording), and prune repairs
blob-loss recordings and sweeps invalid ones. Review & upload are tagset-rooted:
uploads offer a draft appearance (byte-dups included), the queue classifies each
submission (new recording / new appearance / no new bytes) and the moderator
approves per-piece — with case-B drop-bytes (absorb-at-the-gate) and force-new
(pinned split) overrides. P5 (recordings/files admin views) is **built**: the
`/admin/recordings` curation page (both arms, selection-based merge, appearance
move/set-primary, whole-recording trash [hard delete moved to the Trash page —
`soft-delete.md`], editable license/guest) and the All-files physical columns with the "Show removed"
toggle — UX per the owner-signed mock of 2026-07-07, which also resolved open
point 1 (merge/move mechanics). Federation (P6) not started.
This document is the reference design and the implementation plan.
Extends
[Recordings](recordings.md) (same-audio grouping & renditions) and the
[artist/album overlay](artist-album-model.md); the metadata payload defined here
is what will travel in [Federation](federation.md) (deferred). A short list of
*remaining* open points is at the end — everything else is settled.

## Problem

The recordings overlay groups same-audio `files` into a **recording**; each
member file is a **rendition** (a specific encoding). Two gaps follow from tags
living 1:1 on the file (`media_metadata`, PK `file_id`):

1. **Dedup destroys metadata.** On `/admin/duplicates` a moderator can only
   *delete* a redundant rendition (which destroys its tags with the blob) or
   *split* it into its own recording. There is no third outcome — the one that
   actually matters: **the same recording legitimately appears on several
   releases** (single, studio album, "best of", VA compilation), each with a
   different album / album-artist / track number, and usually in different
   encodings. The moderator wants to **keep the one best blob and preserve every
   distinct appearance** — not choose between storage and metadata.
2. **Moderation can't express the common decision.** The review queue can only
   approve/return/discard a *file*. It cannot say "this is a duplicate of what
   we already hold — take its metadata as a new appearance, and keep or drop its
   bytes on their own merits", which is the most frequent real decision once a
   library matures.

For federation this is the same need, sharpened: a node wants to hold **one
best-quality master** per recording and carry the **union of metadata
appearances** the network knows about, rather than N redundant blobs.

## The model — tagsets are hardlinks

The design's core mental model (owner's framing, adopted as the invariant):

- A **recording** is like an *inode* (or an archive file): one audio identity
  that owns the stored data. It has no name of its own.
- Its **files** are the stored data — the renditions (encodings). Primarily
  one, the best; possibly more for streaming quality tiers
  ([recordings.md](recordings.md) quality ladder, future derived transcodes in
  [variants.md](variants.md)).
- A **tagset** is like a *hardlink*: a named catalog entry pointing at the
  recording. Title + artist + album-artist + album + track/disc/year/genre,
  resolving to `artists`/`albums` entities via the existing resolver. **The
  tagset is what users and moderators handle** — the library, search,
  playlists, the review queue, and Trash all operate on tagsets, the same way a
  filesystem user only ever touches links, never inodes.

```
                        ┌──────────────────────────────┐
             renditions │  recording  (audio identity) │ tagsets
       (physical: which │     id, preferred_file_id    │ (logical: where it
        blob to serve)  └──────┬───────────────┬───────┘  appears in library)
                               │               │
        ┌──────────┬───────────┘               └───────────┬──────────────┐
     files (blob) files (blob)                   tagset               tagset
     FLAC master  MP3 320                 "Studio Album, trk5"   "VA Comp, trk3"
     (tech specs) (tech specs)             → artist/album ids    → artist/album ids
```

**The invariant, both directions:** a live recording has **≥ 1 tagset and
≥ 1 file** — always at least one link and at least one blob.

- Hard-deleting a **non-last** tagset removes just that appearance; the
  recording and its files stay (other links exist).
- Hard-deleting the **last** tagset removes the recording **and all its files**
  (blobs reclaimed) — like the filesystem freeing an inode when its last link
  goes.
- A recording with **zero files** is *invalid* — nothing to play. Prune detects
  it and removes the recording, any corrupt file rows, and all its tagsets.

Tech specs (duration, bitrate, sample-rate, codec, bit-depth) belong to the
**blob**; descriptive tags belong to the **appearance**. Today `media_metadata`
conflates them in one file-keyed row because every file had exactly one
appearance. Splitting that conflation is the migration's core.

## Decision record (owner review, 2026-07-04)

1. **Approach A** — decompose `media_metadata` (tech stays file-keyed;
   descriptive columns move to a new `tagsets` table). The alternative additive
   `recording_tagsets` union was rejected: pre-release, clean model wins.
2. **Every file belongs to a recording** — `files.recording_id` becomes
   NOT NULL; the resolver creates a singleton recording for any unmatched file,
   including fpcalc-absent installs (supersedes recordings.md's "NULL =
   implicit recording" rule). Every tagset belongs to a recording (NOT NULL FK).
3. **Appearance identity includes `album_artist_id`** — the dedup key is
   `(recording_id, album_id, album_artist_id, disc_number, track_number)`.
4. **Playlists / favorites / queue reference a tagset** (the specific
   appearance the user picked), not the recording.
5. **The lifecycle is tagset-centric** (the hardlink model above). Review,
   Trash, and the file-management surfaces list tagsets; restore restores a
   tagset; the last-tagset hard delete cascades to the recording and its files;
   prune garbage-collects invalid recordings. This **supersedes** the draft's
   "blocked hard-delete + moderator override" guard entirely.
6. **Upload & review are reworked around tagsets** — an upload *offers a
   tagset*; the moderator's queue classifies each submission (new recording /
   new appearance on an existing recording / better rendition) and acts
   accordingly. A large work item, designed below.
7. **Federation / external-source tagsets stay open** — peer- or
   data-source-offered tagsets must be locally reviewable; design deferred to
   the [federation.md](federation.md) session. The P0 schema only reserves
   provenance.
8. **The DB guarantees the invariants where it can**; every mutation is a
   single transaction; prune is the enforcement backstop. Two new admin
   perspectives — a **recordings view** and a **files view** — complement the
   tagset surfaces; recording **merge and split** are wanted (mechanics still
   open); **every new operation gets a bulk API** ([docs/api/bulk.md](../api/bulk.md)).
9. **The cascade is symmetric** (follow-up decision, same review):
   hard-deleting the **last file** of a recording cascades to the recording and
   all its tagsets, behind a count-aware confirm — mirror of the last-tagset
   rule. And **`license` / `guest_playable` move to `recordings`** in the P0
   migration (one audio identity, one license).

## Data model

### `tagsets` (new)

```sql
CREATE TABLE tagsets (
  id               INTEGER PRIMARY KEY,
  recording_id     INTEGER NOT NULL REFERENCES recordings(id) ON DELETE CASCADE,

  -- Raw tag text (overlay — never silently mutated), exactly the descriptive
  -- columns moved off media_metadata:
  title            TEXT    NOT NULL,        -- required-name default applies
  artist           TEXT,
  album_artist     TEXT,
  album            TEXT,
  genre            TEXT,
  year             INTEGER,
  track_number     INTEGER,
  track_total      INTEGER,
  disc_number      INTEGER,
  composer         TEXT,
  comment          TEXT,

  -- Resolved overlay FKs (the SAME resolver media_metadata used: effectiveArtist
  -- / effectiveTrackArtist / album identity, docs/architecture/artist-album-model.md):
  artist_id        INTEGER REFERENCES artists(id),   -- performer
  album_artist_id  INTEGER REFERENCES artists(id),   -- album-grouping artist
  album_id         INTEGER REFERENCES albums(id),

  -- Catalog lifecycle (moved from files — the tagset is the reviewable,
  -- trashable unit; see Lifecycle):
  review_state     TEXT    NOT NULL DEFAULT 'draft'
                     CHECK (review_state IN ('draft','submitted','returned','approved')),
  review_note      TEXT,
  submitted_at     INTEGER,
  created_by       INTEGER REFERENCES users(id),  -- who offered this appearance
  deleted_at       INTEGER,                        -- tagset-level Trash

  -- Provenance: the file this appearance's tags were read from. Kept for audit /
  -- federation attribution; SET NULL when that blob is purged. A future
  -- origin_node column (federation) sits beside it.
  origin_file_id   INTEGER REFERENCES files(id) ON DELETE SET NULL,

  is_primary       INTEGER NOT NULL DEFAULT 0,  -- the recording's default appearance
  created_at       INTEGER NOT NULL
);

-- Appearance identity (see Tagset identity). Deliberately NOT a UNIQUE
-- constraint — see the implementation note below.
CREATE INDEX idx_tagsets_identity ON tagsets(recording_id, album_id, album_artist_id, disc_number, track_number);
CREATE INDEX idx_tagsets_album_id        ON tagsets(album_id);
CREATE INDEX idx_tagsets_artist_id       ON tagsets(artist_id);
CREATE INDEX idx_tagsets_album_artist_id ON tagsets(album_artist_id);
CREATE INDEX idx_tagsets_origin          ON tagsets(origin_file_id);
CREATE INDEX idx_tagsets_deleted         ON tagsets(deleted_at);
CREATE INDEX idx_tagsets_review    ON tagsets(review_state) WHERE review_state <> 'approved';
```

**Implementation note (P0): the identity key is a plain index, not UNIQUE.**
The draft's `UNIQUE` constraint contradicted the design's own review flow: in
cases B/C a *draft* appearance whose key collides with an existing approved one
must sit in the queue until the moderator denies it — so identical keys
legitimately coexist across review states. It would also have aborted the
migration wherever two renditions of one recording carry identical tags (the
FLAC+MP3-of-one-rip case), and SQLite `UNIQUE` can't cover the NULL disc/track
identities anyway. Identity dedup is therefore enforced entirely by the
attach/absorb operations (the `IS NOT DISTINCT FROM`-style transactional check
already mandated below); the index is their fast path.

### `media_metadata` (reduced)

Keeps **only** the tech columns (`duration_seconds`, `bitrate`, `sample_rate`,
`channels`, `codec`, `bit_depth`) plus `tag_format` and the `extracted_at`
bookkeeping stamp, PK `file_id`. Filled by `ffprobe` as today; a property of
the blob.

### `files` (changed)

- `recording_id` becomes **NOT NULL** (singleton-recording backfill, below).
- `review_state` / `review_note` / `submitted_at` **move to `tagsets`** — the
  tagset is the unit that carries the review *state*. This changes bookkeeping
  only: the submitted **file stays fully accessible to the moderator** — the
  queue's preview plays it, and the pending-blob gate (Visibility, below)
  serves it to owner + `content.moderate` exactly as today. Validating the
  audio is validating *that file*.
- `deleted_at` **stays**, but its meaning narrows to *rendition removal*
  (absorb, files-view delete): bytes kept until GC, restorable as a rendition.
  It is **not** the user-facing Trash anymore (that is `tagsets.deleted_at`).
- `license` and `guest_playable` **move to `recordings`** (decision 9).
- `uploaded_by`, `hash`, `storage_backend` unchanged.

### `recordings` (gains access/license)

`preferred_file_id` stays as the ladder override. A recording still carries no
tags — its display identity is its **primary tagset** — but it becomes the home
of **`license`** and **`guest_playable`** (one audio identity, one license; the
migration collapses per-file values, conflicts resolved by the best rendition's
values). The federation rule is untouched: access is a **local recording**
property, never imported from a tagset.

### Invariant enforcement

"If the DB can guarantee it, let it guarantee it":

- **Creation is atomic** — every path that creates a recording creates it *with*
  its first file and first tagset in one transaction (upload, import, split).
- **Deletion is atomic** — the cascade ops below run the whole
  last-tagset → recording → files chain in one transaction
  (`_txlock=immediate`, the [bulk pattern](../api/bulk.md)).
- **Triggers as a belt** where SQLite allows (e.g. reject a plain `DELETE` on
  the last tagset of a recording outside the cascade op), app-layer checks as
  suspenders.
- **Prune is the backstop**: a standing sweep finds invalid recordings (zero
  files, zero tagsets, dangling `recording_id`) and GCs them with a summary
  report — nothing can rot silently even if a bug slips a violation through.

## Tagset identity (appearance dedup)

Two appearances are **the same** when they share
`(recording_id, album_id, album_artist_id, disc_number, track_number)` — all
locally-resolved ids, never raw tag text (and never peer-supplied ids —
federation's dedup-spoofing defense). Attaching an appearance whose key already
exists on the recording keeps the existing one; it is the appearance-level
analogue of content-hash dedup on blobs.

SQLite caveat: `UNIQUE` treats NULLs as distinct, and `disc_number` /
`track_number` are legitimately NULL ([disc-numbering.md](disc-numbering.md)
keeps untagged distinct from 0/N). The attach/absorb ops therefore do their own
`IS NOT DISTINCT FROM`-style duplicate check inside the transaction; the
`UNIQUE` index is the fast path and the backstop for the non-NULL common case.

## The "meaningful tagset" rule

*Don't manufacture nameless appearances.* A tagset is **meaningful** iff it
resolves to a non-default artist **or** a non-default album — i.e. not
(`artist_id` and `album_artist_id` both the reserved `Unknown artist` entity
**and** `album_id` the reserved `Other` album). The reserved-entity test reuses
`DefaultArtistName` / `DefaultAlbumTitle` (`database/entities.go`) — an exact
check, not a heuristic.

Scope of the rule, precisely:

- **An upload's own tagset is always kept**, even all-null — it resolves to
  Unknown artist / Other, the UI highlights it as poor, but "a null tagset is
  just a tagset". Every recording keeps ≥ 1 tagset by invariant.
- Only when **absorbing extra appearances** (dedup paths: absorb, merge, the
  review flow's "take metadata, drop blob") is a **nameless** extra dropped —
  it adds nothing the surviving appearance doesn't already have, and there is
  no reason to grow the Unknown-artist bucket.

## Lifecycle — tagset-centric

### Upload

An upload is a **file plus an offered tagset** (read from its tags; possibly
empty → Unknown artist / Other):

- **New audio** (no fingerprint match) → new recording + file + draft tagset,
  one transaction.
- **Same audio, new bytes** (fingerprint matches an existing recording) → the
  file lands as a candidate rendition of that recording, the offered tagset as
  a draft on it.
- **Same bytes** (content-hash dedup) → no new file; the offered tagset lands
  as a draft on the blob's recording. (A re-upload of trashed content becomes
  simply a new draft tagset — the `StageRestoredFile` demotion generalizes.)

The draft tagset carries `created_by` = the uploader; "My uploads" lists the
caller's non-approved tagsets.

### Review states

The `draft → submitted → (returned ⇄) → approved` machine from
[moderation.md](moderation.md) moves unchanged onto the tagset — same guarded
transitions, same self-approve rule for `content.moderate` holders, same
duplicate exception (a duplicate-flagged submission always queues).

### Visibility

A tagset appears in the library iff it is **approved, non-trashed, and its
recording has ≥ 1 surviving (non-removed) file**. The blob gate follows: a blob
serves publicly iff its recording has ≥ 1 approved, non-trashed tagset;
otherwise only to the owner / `content.moderate` (the pending-blob rule,
re-expressed). A recording whose renditions are all removed keeps its tagsets
but drops out of the library until a rendition is restored.

### Trash (soft delete)

`tagsets.deleted_at`. Trash lists **tagsets**; restore restores the tagset,
which re-enters its prior review state (badge rule unchanged). Trashing the
last live tagset of a recording makes the recording dormant — files stay on
disk, nothing is lost, restore brings it all back. Soft delete never cascades.

> The Trash **page** now has three perspectives over the same not-in-library
> set — Appearances (this tagset mark), Recordings (whole recordings out of the
> library), and Files (soft-removed blobs) — and is the **only** place permanent
> delete happens. Full model: `docs/architecture/soft-delete.md`.

### Hard delete (the cascade)

From Trash permanent-delete, single or bulk:

- **Non-last tagset** → delete the tagset row. Done.
- **Last tagset** → delete the recording **and all its files** (rows + blobs,
  via the existing storage-aware reclaim — symlinked externals are unlinked
  only, per the data-sources invariant). The UI confirm is count-aware:
  *"…also permanently removes the recording and its N files."*

Both run in one transaction per batch + one summary audit row
(`BulkHardDeleteTrashedByHashes` becomes tagset-id-based and folds the cascade
into the same tx — the standing SQLITE_BUSY lesson).

### Rendition removal (the file side)

Renditions are removed on the duplicates/files/recordings surfaces (absorb, or
an explicit per-rendition delete): `files.deleted_at`, bytes kept, restorable
as a rendition. **Soft-removing the last surviving rendition is allowed** — the
recording goes dormant (tagsets kept, hidden from the library, fully
reversible). **Hard-deleting the last file cascades**, symmetric with the
tagset side (decision 9): behind a count-aware confirm (*"…also permanently
removes the recording and its N appearances"*) the recording and all its
tagsets go with it. The invariant holds because *both* hard-delete directions
cascade — neither side can strand the other.

### Prune / GC

Prune gains recording-awareness:

- A **missing/corrupt blob** removes that file row (as today) **and repairs
  the recording**: if other files survive, the recording just lost a
  rendition; if it was the last file, the recording is invalid → prune removes
  the recording and **all its tagsets**, reported in the prune summary.
- A standing **invalid-recording sweep** (zero files / zero tagsets) is part of
  every prune scan — the invariant's backstop.

## Upload & review rework (the big work item)

The moderation queue becomes a **tagset queue with recording context** — the
moderator gets the toolset to manage incoming files, tagsets, and recordings in
one place. Each submission is classified server-side (evolving
`IsDuplicateSubmission` from a boolean into a classification + compare):

| Case | What arrived | Queue shows | Moderator can |
|---|---|---|---|
| **A — new recording** | new audio, new bytes | "new recording" | approve (publishes tagset + recording + file), return, deny |
| **B — existing recording, new file** | same audio, different encoding | matched recording + **quality-ladder compare** (new file vs. current best) | approve the **appearance** and choose the bytes' fate: **drop the blob** (pure metadata absorb) or **keep it as a rendition** (it's better / a wanted tier); return; deny |
| **C — existing recording, existing file** | byte-identical (hash dedup) | matched recording, "no new bytes" | approve the appearance only; return; deny |

In B and C, if the offered tagset collides with an existing appearance
(identity key) there is nothing new at all — the queue says so, and deny is the
natural action. Deny = discard to Trash (tagset soft delete), as today.

**The classification is a suggestion, not a verdict.** The moderator validates
all three pieces himself and can override each independently:

- **The file** — the preview always plays the *submitted blob* (never the
  matched recording's existing best), because the audio under review *is* that
  file; the ladder compare sits beside it for A/B listening against the
  current best.
- **The recording assignment** — the queue states plainly where the submission
  will land ("creates a new recording" / "joins *recording X*"), and the
  moderator can **change it**: reassign to a different recording
  (search/pick), or force "this is actually new" when the fingerprint match is
  wrong — which pins the result (`recording_pinned`, same mechanism as split)
  so the resolver never re-merges it.
- **The tagset** — edit it in place (the shared modal) before approving, or
  return it to the uploader with a note for *them* to fix.

And the decisions per piece combine freely: approve the appearance while
dropping the bytes, keep the bytes while returning the tagset for rework,
accept one and deny the other. Approve, return-with-note, and deny remain the
terminal actions, exactly as in [moderation.md](moderation.md) — the rework
adds the recording context and the per-piece control, it does not change the
state machine.

Everything else carries over from [moderation.md](moderation.md): per-uploader
collapsible groups, return-with-note, preview plays the **submitted file**
through the page-local player, edit via the shared `track-edit.js` modal (now
patching the tagset), bulk approve/return/discard over `submitted` rows with
the filter/`all:true` select-all machinery, `selectable_total`, one audit row
per bulk action. The uploader's "My uploads" mirrors the same reshaping
(tagset rows, states, notes, remove = tagset trash).

**Federation preview (not designed now):** peer- or data-source-suggested
tagsets/recordings/files enter this *same* queue with extra verification steps
(provenance shown, re-check against the local library). The flow is built for
that shape; the steps themselves wait for the federation session.

## Absorb (library-side dedup)

The review rework handles duplication **at the gate**; absorb handles it for
what is **already in the library** — the original motivator. On
`/admin/duplicates`, gated `content.moderate`:

`POST /api/admin/duplicates/{recording_id}/absorb` — body names the kept
rendition (`keep_file_id`) and the renditions to absorb (`absorb_file_ids`).
One transaction:

1. Per absorbed file: read its tags, resolve the appearance key; if
   **meaningful** and **not a duplicate appearance**, attach a tagset to the
   recording (`origin_file_id` = that file). Otherwise no tagset.
2. Remove the absorbed renditions (`files.deleted_at` — bytes reclaimed later).
3. The kept rendition stays; the recording's primary tagset is unchanged.

One summary audit row. The selection is always the human's — never auto-absorb
(same principle as "fingerprint grouping is a suggestion, never an
auto-delete"). Restoring an absorbed rendition later re-adds only the **blob**;
appearance dedup prevents a doubled tagset.

## Shared operations layer

All surfaces (review, duplicates, recordings view) compose the same primitives —
each a single transaction, each with a bulk variant and one audit row:

- `AttachTagset(recording, tags, …)` — resolve + identity-dedup + meaningful
  rule; used by upload, review-approve, absorb, merge.
- `MoveTagset(tagset → recording)` — reassign a mis-attached appearance (the
  appearance-level split).
- `SetPrimaryTagset(recording, tagset)`.
- `RemoveTagset` / `RestoreTagset` (soft) and `HardDeleteTagsets` (the cascade).
- `AddRendition(recording, file)` / `RemoveRendition` (soft; last one allowed →
  dormant recording) / `HardDeleteFiles` (cascades on the last file).
- `SoftDeleteRecording` / `HardDeleteRecording` — the whole-recording delete on
  the recordings view: soft trashes **all** its tagsets (recording dormant,
  fully reversible); hard removes the recording **with all tagsets and all
  files** in one transaction, behind the count-aware confirm. The same cascade
  the last-tagset/last-file rules reach piecemeal, offered directly.
- `SplitRendition` — as today, plus: the split-off recording takes a **copy of
  the primary tagset** (so it is browsable); the moderator fixes tags after
  (the existing "split pairs with a tag fix" flow). Absorbed tagsets stay put
  unless explicitly moved.
- `MergeRecordings(a ← b)` — union renditions + tagsets; appearance dedup
  collapses identical appearances; ladder re-ranks. **Wanted; UX/mechanics
  still open** (below) — the primitive is designed, the surface is not.

## Admin surfaces

Deliberately **two perspectives** (owner accepts the added admin complexity):

- **Tagset surfaces = the existing pages, re-pointed.** Library, All files,
  Trash — the unified file-list
  ([file-management-view.md](file-management-view.md)) keeps its scopes,
  paging, select-all and bulk toolbars, but rows are **tagsets** ("a track as
  a regular user perceives it"). This is where day-to-day work stays.
  *Shipped divergence (P4):* **Review** and **My uploads** did **not** stay
  file-list scopes — they were rebuilt as bespoke standalone modules
  (`admin/moderation.js`, `mine-list.js`) so the moderator gets the per-piece
  decision card (ladder compare, drop-bytes, force-new) and the uploader sees
  review state/notes, which the shared component couldn't express;
  `file-list.js` was rolled back to pre-P4e. They keep their own paging,
  select-all and bulk toolbars and still edit through `track-edit.js`.
- **`/admin/recordings` (new, P5 — UX settled 2026-07-07)** — the
  recording-centric view: **all** recordings, newest first (`id DESC` — the
  ones an admin edits first), searchable (title/artist/album/`#id`), quick
  filter pills (**All** default / >1 rendition / >1 appearance / dormant /
  pinned), paged with the existing windowed infinite-scroll machinery
  (Admin·Library style). A recording row is a collapsed card (checkbox, `#id`,
  primary name, count chips, ★ best format, dormant/pinned badges, an
  **editable license/guest chip** opening a small recording-level editor, Play
  = ladder-best preview); it expands to **both arms stacked** — renditions
  first (ladder rank, tech, live/removed state; Play / Split off / Remove /
  Restore), the appearance list under it full-width (primary marked, review
  state; a live appearance offers Edit / Set primary / Move… / Remove, a
  trashed one **Restore** only) — plus the card footer's **Trash recording**
  (soft = trash all its tagsets; the whole recording then shows in the Trash
  page's Recordings lens). **Permanent delete lives only on the Trash page now**
  (`soft-delete.md`): `/admin/recordings` does soft ops + curation, and every
  hard delete — appearance, recording, or removed file — happens in the three
  Trash perspectives. **Merge is selection-based**:
  it lives only in the bulk bar, disabled until ≥2 recordings are ticked, so
  it adds no everyday weight; the confirm modal picks the surviving target
  (default = the recording holding the union's ladder-best rendition,
  switchable) and previews the result (renditions re-ranked, identical
  appearances collapse, target keeps its primary). **Move…** (MoveTagset)
  re-homes an appearance onto another *existing* recording via a search
  picker — an identical appearance on the target refuses the move; there is
  deliberately no "move to new recording" (an appearance without a blob can't
  play — that shape is Split off). Admin-shell page, `content.moderate`,
  page-local player, nav link (after Duplicates) + dashboard card, `nowebui`
  compiles it out — the `/admin/sources` / `/admin/duplicates` conventions.
- **Files view (P5 — settled)** — the physical perspective is **not a new
  page**: the existing Admin·Library "All files" table gains three columns —
  storage **backend**, file **state** (live / removed / trashed; removed =
  absorbed/dormant blobs, until now visible nowhere outside Trash) behind a
  **"Show removed" toggle (off by default)**, and the **recording link**
  jumping to `/admin/recordings` with that recording expanded. Paging/bulk
  machinery reused as-is.

`/admin/duplicates` stays as the focused dedup workbench (multi-rendition
recordings + absorb).

Every list/bulk operation across all three perspectives gets the batch
endpoints + filter/`all:true` semantics of [docs/api/bulk.md](../api/bulk.md)
from day one.

## Library, serving & addressing

- **Browse/search re-point to tagsets.** `ListArtists` /
  `ListAlbumsByArtistID` / `ListTracksByAlbumID` / search join `tagsets`
  instead of `media_metadata`, joining `files` only for the serving rendition
  and the visibility rule. This is the bulk of the implementation cost.
- **A library track is addressed by `tagset_id`** — an absorbed appearance has
  no blob of its own, so the file-hash identity no longer works. Every surface
  that keys a row on `hash` (library, queue, playlists, favorites, player)
  carries `tagset_id`; the play URL resolves server-side: tagset → recording →
  ladder-best rendition (or `preferred_file_id`) → blob. Playlists/favorites
  reference the **tagset** (decision 4); existing rows migrate 1:1.
- **The quality dropdown is unchanged** — renditions are a property of the
  recording, orthogonal to which appearance you entered from
  (`GET /api/tracks/{…}/renditions` re-keys accordingly).
- **Covers: no new work.** Covers attach to `album_id`; each tagset resolves
  its own album, so appearances carry their own covers. A recording has no
  cover of its own.
- **Editing:** `track-edit.js` + the metadata PATCH target a **tagset**
  (descriptive fields; re-resolution on identity-affecting changes unchanged).
  Tech fields stay file-keyed and read-only.

## Access control & license

**Decided (decision 9):** `license` and `guest_playable` live on the
**recording** — all renditions are the same content, so one license and one
guest flag per audio identity. The P0 migration collapses today's per-file
values (conflicts resolved by the best rendition's values). The visibility
rule stays byte-anchored: an appearance is playable iff its recording allows
it *and* has a surviving rendition — and a tagset can never widen access (the
federation lock: an imported appearance must never flip `guest_playable`).
Bulk license/guest edits on the tagset surfaces resolve to the recording, so
`BulkSetLicense` / `BulkSetGuestPlayable` re-target `recordings`.

## Federation notes (deferred, schema-ready)

Tagsets are the catalog-metadata half of federation's metadata-vs-stream split:
a node advertises *"recording `<fingerprint>` — I hold these renditions and
these appearances"*; peers reconcile the **union** of appearances over the
cross-node fingerprint index. Constraints this design already locks in so the
schema won't reshape later:

- **Provenance** — a peer-contributed tagset records its origin node
  (`origin_file_id` now; an `origin_node` column beside it when federation
  lands), so trust weighting and clean **revocation** (filter by origin, not
  destructive un-merge) are possible.
- **Local review** — peer- and data-source-offered tagsets/recordings/files go
  through the *local* review queue (the flow above), never auto-merge.
- **Access never imported** — a peer's tagset cannot change local
  access/license/guest flags.
- **Dedup keys are locally resolved** — a peer can't spoof
  `(album, track)` collisions to suppress a real appearance.

Everything else (sync, trust model, union mechanics) belongs to the
[federation.md](federation.md) design session.

## Phase plan (the decomposition)

Sequenced so each phase lands green and behavior-identical until its feature
switches on; P0+P1 are pure re-plumbing staged **before** any visible change so
regressions isolate to the data move.

- **P0 — Data model & migration. ✅ Done (migration `024_tagsets.sql`).** The
  `tagsets` table; decompose
  `media_metadata`; move `review_state`/`review_note`/`submitted_at` to
  tagsets and `license`/`guest_playable` to recordings; `files.recording_id`
  NOT NULL + singleton-recording backfill; one primary tagset per file
  backfilled from its old row (a **trashed file maps to a trashed tagset**
  with the file row kept live — preserving today's restore semantics exactly);
  invariant triggers + startup reconcile (`db.ReconcileTagsets`).
  *Result: identical behavior on new foundations — verified by diffing every
  listing/staging/admin endpoint against the pre-P0 server on a copy of the
  real library. All queries join tags through the file's offered tagset
  (`tagsetJoin`: alias `m`, 1:1 via `origin_file_id` until P1 re-keys
  addressing); every insert path creates recording + file + tagset + tech row
  in one transaction; hard delete cascades through the shared
  `hardDeleteFilesTx`; the fingerprint resolver moves files (with their
  tagsets) between recordings and GCs emptied singletons. The `Repository`
  interface and its DTOs kept their exact shapes, so the api layer and its
  fakes needed no changes. Sole visible change, per decision 9: conflicting
  per-rendition guest/license values collapsed to the best rendition's.*
- **P1 — Library & addressing on tagsets. ✅ Done (migration
  `025_playlist_tagsets.sql`).** Re-point browse/search/covers/
  visibility; `tagset_id` addressing through player, queue, playlists,
  favorites; play-URL resolution; renditions endpoint re-key.
  *Result: identical UX, tagset-addressed; the hash-keyed track dies (as the
  listening identity — `hash` stays on track rows as the origin file, the
  admin/file identity the file surfaces keep). Library queries are
  tagset-rooted (`visibleTagset` + `bestRenditionJoin`: approved non-trashed
  appearance with ≥1 surviving rendition, playing the ladder-best);
  playlists/favorites reference the tagset (`playlist_items.tagset_id`,
  existing rows migrated 1:1); the blob gate is recording-level
  (`BlobPubliclyVisible`); renditions moved to
  `GET /api/tagsets/{id}/renditions`; the persisted queue key bumped to
  `madshare-queue-v2` so stale hash-keyed queues drop once. My-uploads
  ownership checks stay on the file's own tagset (`FileReviewInfo`) —
  deliberately narrower than the recording-level gate, so a pending duplicate
  stays editable by its uploader.*
- **P2 — Lifecycle & GC. ✅ Done.** Tagset Trash/restore; the last-tagset
  hard-delete cascade (single + bulk, one tx); rendition removal + last-rendition
  dormancy; prune's recording repair + invalid-recording sweep.
  *Result: the hardlink semantics are live and safe end-to-end. The Trash
  permanent-delete (single `HardDeleteTrashedFileByHash` + bulk
  `BulkHardDeleteTrashedByHashes`) now cascades from the tagset through the
  shared `hardDeleteTagsetsTx`: a non-last appearance drops only its tagset (the
  blob survives — another appearance may play it), the last appearance GCs the
  recording and all its files (blobs reclaimed post-commit). The API stays
  hash-addressed (the tagset/files scopes are P5); the cascade underneath is
  tagset-first, so a multi-rendition recording's blobs are never destroyed by
  trashing one appearance. `RemoveRendition`/`RestoreRendition` soft-remove a
  blob (files.deleted_at); removing the last surviving rendition is allowed and
  makes the recording dormant (hidden by `visibleTagset`, fully reversible) —
  per decision 9, superseding the phase one-liner's "refusal". Prune GCs a
  blob-loss recording via the existing file-first cascade and runs a standing
  `SweepInvalidRecordings` backstop each confirmed pass (reported as
  `InvalidRecordings`). The `deleted`-count return let the Trash "N removed"
  tally and blob reclaim stay correct when a non-last delete frees no bytes.*
- **P3 — Operations layer + absorb. ✅ Done.** Absorb (`database/absorb.go`:
  `AbsorbRenditions` + `BulkAbsorbKeepBest`), the meaningful rule + appearance
  dedup helpers (`loadAppearances` resolves the reserved-bucket check;
  `appearanceKey` does the NULL-safe identity dedup in Go), and the
  `/admin/duplicates` absorb UI + endpoints
  (`POST /api/admin/duplicates/absorb/{recording_id}` single;
  `POST /api/admin/duplicates/absorb` bulk keep-best over `recording_ids` or
  `all:true`). `SplitRendition` gained the tagset-less fallback (copies the
  source's primary appearance so a split absorbed-blob stays browsable).
  *Result: "keep the best blob, preserve every appearance" is usable in the
  library.* Scope notes: same-recording absorb keeps each absorbed file's
  existing appearance in place (no tagset creation needed) and drops it only if
  nameless or a duplicate identity — soft-removing the absorbed blobs
  (`files.deleted_at`, restorable); the kept rendition and its appearance are
  never touched (primary re-promoted if a dropped tagset held it). The endpoint
  URL is `/duplicates/absorb/{recording_id}` (not the design's
  `/duplicates/{recording_id}/absorb`) to avoid a chi wildcard-name clash with
  `/duplicates/{file_id}/split`. `MoveTagset` / `SetPrimaryTagset` are **deferred
  to P5** — their only surface is the recordings view; building them now would
  yield unwired, untestable primitives.
- **P4 — Review & upload rework. ✅ Done.** The classified queue (cases A/B/C
  with ladder compare and the appearance-vs-rendition decision), upload's offered
  tagset (incl. byte-dup → draft tagset), My-uploads on tagsets, bulk + audit.
  *Result: moderation resolves duplication at the gate, per-piece. The queue and
  My-uploads are tagset-addressed (`ReviewEntry.TagsetID`; ~12 repository methods
  + handlers + routes re-keyed hash→`tagset_id`; submit/bulk bodies take
  `{tagset_ids}`; metadata edit via `UpdateTagsetMetadata`/`TagsetMetadataByID`).
  `ClassifySubmission` (`GET /api/admin/moderation/{tagsetID}/classify`) derives
  the case off the file's already-resolved recording — `blobPublished` → C
  (`no_new_bytes`), else `recordingPublished` → B (`new_appearance`), else A
  (`new_recording`) — plus the NULL-safe appearance-collision flag and the
  ladder compare (`CurrentBest` vs `Submitted`, `SubmittedIsNewBest`).
  `ApproveSubmission(tagsetID, dropBytes, forceNew)` publishes the appearance in
  one tx and applies the per-piece overrides atomically: `forceNew` splits the
  blob into a new `recording_pinned` recording (skipped when the blob already
  carries an approved appearance — a byte-dup can't be "actually new"); `dropBytes`
  soft-removes the submitted rendition after publishing (case-B absorb-at-the-gate;
  dropping the only rendition → dormant recording). A byte-dup upload calls
  `AttachDraftTagset` (NULL-safe identity dedup) to offer a draft appearance on
  the held recording (enabling case C); a `reprTagset` "representative appearance"
  pick keeps every files-rooted surface 1:1 with the blob. Moderator per-appearance
  edit lands at `GET`/`PATCH /api/admin/moderation/{tagsetID}/metadata`.
  Scope note: the Review queue and My-uploads shipped as **bespoke standalone
  modules** (`admin/moderation.js` rewritten + new `mine-list.js`), **not** the
  re-pointed `file-list.js` scopes the "Admin surfaces" section sketched —
  `file-list.js` was rolled back to pre-P4e and now serves only Files/Trash/
  Library/Duplicates. The review card is per-piece (per-uploader collapsible
  groups → collapsed submission cards → expandable 3-piece decision card with the
  ladder compare + keep/drop-bytes + force-new for case B). `/admin/duplicates`
  absorb was also unified into the checkbox selection flow (ticked renditions
  folded into the best unticked one; plain keep-best routes through the bulk
  `/absorb` endpoint, custom selections loop the per-recording endpoint).*
- **P5 — Recordings + files admin views. ✅ Done.** UX settled 2026-07-07
  (clickable mock, owner-reviewed; open point 1 resolved — see "Admin
  surfaces"). Primitives (`database/curate.go`, each one transaction, one
  audit row): `MergeRecordings(target, sources)` — appearances move with
  identity dedup (target's copy wins, nameless dropped, same rules as absorb),
  renditions move **pinned** (a manual merge is a human identity decision the
  resolver must never regroup), target keeps primary + license/guest, sources
  removed; `MoveTagset` (refusals are typed outcomes: same-recording /
  last-appearance — "merge instead" — / NULL-safe identity collision);
  `SetPrimaryTagset`; `TrashRecording` + `BulkTrashRecordings` (soft = all
  appearances to Trash); `HardDeleteRecording` (routes through the shared
  `hardDeleteTagsetsTx` cascade, returns blobs + counts);
  `SetRecordingAccess` (license vocab + manual-guest semantics of the
  hash-addressed setters); `ListRecordings`/`CountRecordings` (newest first,
  filter pills, `#id`/any-tagset substring search, limit/offset) +
  `GetRecordingDetail` (both arms incl. removed blobs + trashed appearances).
  `RemoveRendition`/`RestoreRendition` promoted into the `Repository`.
  `RestoreTagset` (soft-undelete one appearance) + `HardDeleteTrashedTagset`
  (permanent delete of a trashed appearance through the same
  `hardDeleteTagsetsTx` cascade — last one → recording + files GC'd, blobs
  returned; refuses a live appearance) close the trashed-appearance loop the
  recordings view exposes.
  Endpoints under `/api/admin`: `GET/POST /recordings…` (list / merge / bulk
  trash), `GET/DELETE /recordings/{id}` (+ `/primary`, `/trash`, PATCH
  `/access`), `POST /tagsets/{id}/move`, `POST /tagsets/{id}/restore`, `DELETE
  /tagsets/{id}` (409 on a live appearance), `POST /renditions/{id}/{remove,
  restore}`; gates: curation = `content.moderate`, deletes = `file.delete`,
  access = `metadata.edit`. UI: bespoke `admin/recordings.js` (windowed
  virtual-list page scroll, expandable two-arm cards, merge/move/access
  modals, page-local player, `#<id>` deep link) + nav link and dashboard card;
  All-files gained the `storage` column (backend, removed state, recording
  link) via the new generic `scope.cells`/`rowClass` hooks in `file-list.js`,
  fed by `FileFilter.ShowRemoved` (`show_removed=1`, moderation-gated) and
  `FileListEntry.StorageBackend/RecordingID`.
  *Result: full curation from every perspective — verified live (merge / move
  refusals / dormancy round-trip / show-removed / whole-recording deletes /
  browser smoke with zero console errors).*
- **P7 — Re-root the file surfaces on `recording_id`. 🔴 Open (2026-07-10).**
  **Files belong to recordings, not to appearances.** `files.recording_id` is
  the structural link (P0); `tagsets.origin_file_id` is *provenance* — an audit
  column, nullable, `ON DELETE SET NULL`. Several queries nevertheless
  reconstruct "this file's appearance" through it, via `reprTagset`
  (`files.go:32`) and the **INNER** `tagsetJoin` (`files.go:44`). A rendition
  with no tagset of its own — a valid, by-design state that `MergeRecordings`
  and `AbsorbRenditions` both produce, since appearance dedup drops the
  duplicate tagset while the blob moves — then silently vanishes from every
  files-rooted surface, while still streaming fine (access is recording-rooted
  and correct). Reproduced on `HEAD`; findings and evidence in
  `.issues/open-issues.md` ("Recording-tagsets P7").

  P5's "Admin surfaces" text above describes a Trash Appearances lens the
  implementation never delivered: it is one row per file, not per appearance.

  - **P7a** — log the gap, reopen this doc, correct `soft-delete.md` and the
    over-stated "fixed" claims in `.issues/open-issues.md`. *(done)*
  - **P7b — re-root the query layer. ✅ Done (2026-07-10).**
    `reprTagset` searches the file's **recording**, so every file is covered and
    the INNER `tagsetJoin` stays valid — no file can drop out of a surface.
    `analysis.go` and `access.go`'s `liveFileRecordingSubquery` gate on
    `t.recording_id = f.recording_id`, mirroring the already-correct
    `FileAccessibleByHash`; `visibleFile` and that gate now agree, where before
    a blob could be servable yet unlisted. `ReconcileTagsets` heals at
    **recording** grain, so a rendition never gets an invented appearance.

    **The precedence rule** (learned the hard way — a flat recording-rooted
    lookup shipped a bug in draft): the blob's **own** offered appearance wins
    when it has one, because the *per-blob lifecycle* — review state, trash
    mark — lives there. A rendition awaiting review on an already-published
    recording must not borrow the recording's approved primary; it would leak
    into the live All-files listing and its bytes would be misfiled from Review
    into Library. Only a blob with no appearance of its own falls back to the
    recording's. Pinned by `TestReprTagset_OwnAppearanceWinsOverRecording`.

    So the split is: **descriptive identity + recording-level facts** resolve
    through `recording_id`; **per-blob lifecycle** stays with the blob's own
    appearance. `originTagset` / `originTagsetJoin` keep the provenance lookup
    for the two surfaces that genuinely need it — the hash-addressed Trash
    listing (untouched by P7b; P7c re-roots it) and the file-addressed metadata
    edit, which prefers the blob's own appearance and falls back rather than
    404-ing.

    `review.go`'s `reviewFrom` INNER JOIN is **deferred to P7d**: it can strand
    nothing today, because every file-delete path removes the referencing
    tagsets before the row dies, so `origin_file_id` never reaches NULL.
  - **P7c** — Trash Appearances lens `FROM tagsets m LEFT JOIN files f`, one
    row per appearance, switched onto the tagset-addressed
    `POST /api/admin/tagsets/{id}/restore` + `DELETE /api/admin/tagsets/{id}`.
    The latter is currently **dead code**: `b3200c8` stripped its only caller.
  - **P7d** — *then* "Add appearance" on the recording card becomes small and
    safe (`CreateAppearance`: resolve entities → meaningful rule → identity
    dedup → insert non-primary). Blocked on P7b, because a blobless appearance
    is invisible in the review queue while `reviewFrom` INNER-joins `files`.

  **Open decision:** whether P7b should *surface* orphan renditions in All
  files (with a "no appearance" badge), or whether merge/absorb should stop
  creating them — re-pointing the dropped appearance's `origin_file_id` at the
  surviving blob, as `HardDeleteRemovedFile` already does
  (`trash_files.go:190`). The second changes merge's behaviour; owner's call.

- **P6 (deferred) — Federation.** Tagset sync, trust, union reconcile,
  peer-review steps — per the federation design session.

## Test plan

Tests land **with each phase**. Highest-risk: the **cascade/GC rules** and the
**identity/meaningful rules** — they decide whether curated metadata or audio
is silently lost. Layers map onto the existing surfaces (`database/*_test.go`,
the api `fakeRepo`, `tests/js`, `tests/playwright`, `tests/k6`).

- **Migration round-trip (P0):** pre-migration fixture → post-migration:
  every file's descriptive tags on exactly one primary tagset (column-level
  equality), tech still on `media_metadata`, every file with a non-null
  `recording_id`, trashed files mapped to trashed tagsets, idempotent re-run.
  Bump the `database_test.go` version/table assertions; extend `fakeRepo`.
- **Resolver parity (P0):** the tagset resolver yields the same
  `artist_id`/`album_artist_id`/`album_id` as the old path for identical tags
  (shared code — assert no drift).
- **Identity & meaningful rules (P0/P3):** table-driven — appearance dedup
  incl. the NULL disc/track cases; nameless-extra dropped on absorb but an
  upload's own null tagset kept.
- **Cascade & GC matrix (P2, critical):** non-last tagset delete → row only;
  last-tagset delete → recording + files gone, blobs reclaimed
  (storage-aware: external symlinks unlinked, originals intact); bulk sets
  mixing last/non-last in one tx; soft delete never cascades; restore
  re-enters prior state; soft-removing the last rendition → dormant recording
  (hidden, reversible); hard-deleting the last file cascades to recording +
  tagsets; whole-recording soft delete trashes all its tagsets (reversible)
  and hard delete removes recording + tagsets + files in one tx; prune repairs
  a blob-loss recording and sweeps invalid recordings; invariants hold after
  every operation (a generic post-op assertion helper).
- **Absorb (P3):** happy path, atomic rollback on injected failure, drop rule,
  dedup no-op, authz (403/400/404/409), one audit row.
- **Review classification (P4):** cases A/B/C derived correctly (fingerprint,
  hash-dup, tag-fallback when fpcalc absent); B's keep-vs-drop-blob outcomes;
  identity-collision reported; self-approve suppressed on duplicates; the
  moderator's overrides — reassign to another recording, force-new with
  `recording_pinned` (resolver never re-merges), tagset edit before approve —
  and free combination of per-piece decisions; bulk semantics.
- **Visibility & access (P1):** N-tagset recording = N library tracks, each
  playing the ladder-best blob; zero-surviving-rendition recording hidden,
  restore re-surfaces; a tagset can never widen access.
- **Concurrency (P2/P3):** cascade + absorb + bulk under `_txlock=immediate`,
  no `SQLITE_BUSY` (`database/busy_test.go`).
- **Client (P3–P5):** `tests/js` for the selection/classification arithmetic
  as pure functions; Playwright end-to-end — absorb → song appears under both
  albums playing one blob; review case B both outcomes; trash-restore of a
  tagset; last-tagset permanent delete with the count-aware confirm. (Known
  login DOM facts; never PW install-deps on Fedora.)
- **Performance (P1):** one k6 check that browse-by-album latency does not
  regress after the join re-point — the only player-facing perf risk.

## Non-goals (v0)

- **Auto-absorb / auto-merge** — every absorb, merge, and deletion is a human
  action.
- **A track-level "work" entity** (one composition across recordings) — out,
  as in recordings.md. Tagsets are appearances of **one** recording.
- **Multi-credit "feat." parsing** — a tagset has one performer, same as today.
- **Per-appearance quality preference** — every appearance plays the
  recording's ladder-best; no per-tagset rendition pin.
- **Federation sync itself** — model + provenance only.

## Open points (small — everything else is decided)

1. ~~**Merge/split mechanics & UX** on `/admin/recordings`~~ — **resolved
   2026-07-07** (owner review of the clickable mock): merge is selection-based
   from the bulk bar with a target-picker confirm (default target = holder of
   the union's ladder-best rendition); appearance-level split = **Move…**
   (MoveTagset) with a search picker over existing recordings; blob-level
   split stays **Split off** (SplitRendition). Details in "Admin surfaces".
2. **External-source & federation tagset review details** — deferred by
   decision 7 to the federation session (includes data-sources linked imports
   offering tagsets/recordings for local review).

## Impact on existing docs

Per the project's prune-legacy rule, each doc is updated when its phase ships
(no "migration history" sections):

- [recordings.md](recordings.md) — "NULL = implicit recording" dies at P0;
  duplicates page gains absorb at P3; display-name source becomes the primary
  tagset.
- [moderation.md](moderation.md) — ✅ rewritten at P4 (tagset queue,
  classification cases A/B/C, per-piece approve, states on tagsets).
- [soft-delete.md](soft-delete.md) — tagset-level Trash + the cascade at P2.
- [license-access.md](license-access.md) — license/guest on the recording (P0).
- [file-management-view.md](file-management-view.md) — tagset scopes at P1,
  files scope at P5.
- [docs/api](../api/) `metadata.md`, `playlists.md`, `search.md`, `bulk.md`,
  `upload.md` — tagset addressing (P1), new bulk ops (P2–P5), offered-tagset
  upload semantics (P4).
- [docs/ui/player-and-queue.md](../ui/player-and-queue.md) — `tagset_id` in
  the queue model (P1).
- [federation.md](federation.md) — keeps pointing here for the payload shape.

## Gotchas

- The P0 migration bumps `database_test.go`'s version/table assertions, and
  every new `Repository` method breaks the api package's `fakeRepo` (the
  standing gotcha) — P0 and P3/P4 add many.
- Decomposing `media_metadata` + moving review state touches **every** query
  that reads descriptive tags or filters on `visibleFile` — the largest single
  change in the library layer. Keep P0/P1 behavior-identical and land them
  alone.
- The `visibleFile` SQL fragment (`f.deleted_at IS NULL AND f.review_state =
  'approved'`) is joined in many places; its replacement (approved non-trashed
  tagset + surviving rendition) must be a single shared fragment again, or the
  surfaces will drift.
- All hard-delete entry points (Trash permanent single, bulk, prune) must go
  through the **same cascade op** — a second code path reintroduces the orphan
  risk the invariant exists to prevent, and the bulk path must fold the
  cascade into its one transaction (SQLITE_BUSY lesson).
- The required-name default for a tagset `title` has no filename to fall back
  to once the origin blob is gone — acceptable because an upload's own tagset
  is created while the file exists, and absorbed nameless tagsets are dropped;
  assert the edge in tests.
- SQLite `UNIQUE` + NULL disc/track: identity dedup cannot rely on the index
  alone (see Tagset identity).
