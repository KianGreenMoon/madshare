# Recording tagsets — multiple metadata appearances per audio

**Status:** ⚠️ **DRAFT for review — nothing built.** Design only; needs Kian's
sign-off (see [Open questions](#open-questions)) before any migration or code.
Extends [Recordings](recordings.md) (same-audio grouping & renditions) and the
[artist/album overlay](artist-album-model.md); the metadata payload it defines is
what travels in [Federation](federation.md) (seed doc), so it's flagged throughout.

## Problem

The recordings overlay groups same-audio `files` into a **recording**; each member
file is a **rendition** (a specific encoding). Dedup on the `/admin/duplicates`
page today offers a moderator exactly two outcomes:

- **Delete** the redundant rendition (soft-delete the blob). This frees storage —
  but it also **destroys that rendition's tags**, because tags live 1:1 on the
  file (`media_metadata` is PK `file_id`). If the deleted copy was *track 3 on a
  compilation* while the kept copy was *track 5 on the studio album*, the
  compilation appearance is gone with the blob.
- **Split** the rendition into its own recording (it was a genuinely different
  recording mis-grouped — a live take, a cover).

There is no third outcome — and it's the one that actually matters for a media
library: **the same recording legitimately appears on several releases.** A song
ships as a single, on the studio album, on a "best of", on a label compilation —
each with a *different* album, often a different album-artist (a "Various
Artists" comp), a different track/disc number, sometimes a different title. And
the duplicates are usually *also* different encodings (a pure 128 kbps MP3 from
the comp vs. a FLAC from the album). The moderator wants to **keep the one best
blob and preserve every distinct appearance** — not choose between storage and
metadata.

For federation this is the same need, sharpened: a node wants to hold **one
best-quality master** of a recording and carry the **union of metadata
appearances** the network knows about, rather than N redundant blobs.

## Goal — the moderator's flow

From `/admin/duplicates`, for a recording with several renditions:

1. Keep the best rendition (the quality ladder's `#1`, or a manual pick).
2. **Absorb** the other renditions' tags onto the recording as **additional
   tagsets** — so the song still appears under their albums/artists.
3. Trash the absorbed renditions' **blobs** (existing soft-delete; storage
   reclaimed on prune).
4. **Drop** any absorbed tagset that carries **no real artist/album** — no point
   inflating the "Unknown artist / Other" bucket with a nameless duplicate.

The result: one audio identity, one surviving blob, **N library appearances**,
each playing that blob.

## Model: a recording owns N tagsets

Two **independent** one-to-many relationships hang off a recording:

```
                         ┌─────────────────────────────┐
              renditions │  recording  (audio identity) │ tagsets
        (physical: which │     id, preferred_file_id     │ (logical: where it
         blob to serve)  └──────┬───────────────┬────────┘  appears in library)
                                │               │
         ┌──────────┬───────────┘               └───────────┬──────────────┐
      files (blob) files (blob)            recording_tagset   recording_tagset
      FLAC master  MP3 320               "Studio Album, trk5"  "VA Comp, trk3"
      (tech specs)  (tech specs)          → artist/album ids    → artist/album ids
```

- **Renditions** = `files` rows with the same `recording_id` (today's model,
  unchanged). They carry the **physical** properties: bytes, hash, and the
  *tech* columns (duration, bitrate, sample-rate, codec, bit-depth).
- **Tagsets** = the **logical** appearances: title + artist + album-artist +
  album + track/disc/year/genre, each resolving to `artists`/`albums` entities
  via the existing resolver. New.

This decomposition is the conceptual core: **tech specs belong to the blob;
descriptive tags belong to the appearance.** Today `media_metadata` conflates
them in one file-keyed row because every file had exactly one appearance. To let
one audio appear several times we must split that conflation.

### Where the descriptive tags live — the central decision

Two ways to introduce tagsets; this is **[Open question 1](#open-questions)** and
drives the whole migration's size. I recommend **A**.

**Approach A — decompose `media_metadata` (recommended target).** Split the table
along the physical/logical line:

- `media_metadata` (PK `file_id`) keeps **only tech columns** + `tag_format`.
- A new **`tagsets`** table (one-to-many on `recording_id`) holds the
  **descriptive** columns + the resolved `artist_id` / `album_artist_id` /
  `album_id`. Every file's original tags migrate to **one** tagset (its
  `is_primary` appearance).
- Requires every file to have a `recording_id` (see [Singleton
  recordings](#every-file-becomes-a-recording)).
- The library browses **tagsets**, not `media_metadata`. Cleaner end-state, but
  it touches the whole library/search/cover/review query layer — a real cost,
  acceptable pre-release (the project already favors clean models over carried
  legacy paths).

**Approach B — additive `recording_tagsets` (lighter).** Leave `media_metadata`
1:1 with files exactly as today (it stays the "primary" appearance of a surviving
file). Add a `recording_tagsets` table that holds **only the extra** appearances
created by absorb. The library browses the **union** of the two. Less migration,
but two parallel sources of "a library track" that every browse/search/cover/
access query must union forever — and a fragility: the primary appearance is
pinned to a *file* that may itself later be trashed.

The rest of this doc is written for **Approach A** (with notes where B differs).
The new table:

```sql
CREATE TABLE tagsets (
  id               INTEGER PRIMARY KEY,
  recording_id     INTEGER NOT NULL REFERENCES recordings(id) ON DELETE CASCADE,

  -- Raw tag text (overlay — never silently mutated, survives re-import & sync),
  -- exactly the descriptive columns moved off media_metadata:
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

  -- Resolved overlay FKs (the SAME resolver as media_metadata used: effectiveArtist
  -- / effectiveTrackArtist / album identity, docs/architecture/artist-album-model.md):
  artist_id        INTEGER REFERENCES artists(id),   -- performer
  album_artist_id  INTEGER REFERENCES artists(id),   -- album-grouping artist
  album_id         INTEGER REFERENCES albums(id),

  -- Provenance: the file this appearance's tags were read from. Kept for audit /
  -- federation attribution; SET NULL when that blob is finally purged.
  origin_file_id   INTEGER REFERENCES files(id) ON DELETE SET NULL,

  is_primary       INTEGER NOT NULL DEFAULT 0,  -- the recording's default appearance
  created_at       INTEGER NOT NULL,

  -- Dedup appearances within a recording (see Tagset identity):
  UNIQUE (recording_id, album_id, disc_number, track_number)
);
CREATE INDEX idx_tagsets_recording ON tagsets(recording_id);
CREATE INDEX idx_tagsets_album_id  ON tagsets(album_id);
CREATE INDEX idx_tagsets_artist_id ON tagsets(artist_id);
```

### Every file becomes a recording

Approach A needs tags to hang off a recording, so **every file must resolve to a
recording** — including fpcalc-absent installs where today `recording_id` stays
NULL ("its own implicit recording"). Change: the resolver creates a **singleton
recording** for any file that matches nothing (it already does this when a
fingerprint matches nothing — we'd extend it to the no-fingerprint case). Cost:
one extra `recordings` row per file (cheap). Benefit: one uniform place for tags,
no polymorphic "tagset belongs to a file *or* a recording". This is a small but
real change to [recordings.md](recordings.md)'s "NULL = implicit recording" rule
and should be confirmed ([Open question 2](#open-questions)).

## Tagset identity (dedup of appearances)

Absorb must not create a **second copy of the same appearance**. Two renditions
of one recording that were both *track 5 on the studio album* must collapse to a
single tagset, not two identical ones. Proposed identity key (the `UNIQUE` above):

> `(recording_id, album_id, disc_number, track_number)`

When absorbing a tagset whose resolved key already exists on the recording, **keep
the existing one** (it's the same appearance) — do not insert. This is the
appearance-level analogue of content-hash dedup on blobs.

The primary tagset is whichever the surviving (kept) rendition contributes;
absorbed tagsets that *differ* in album/disc/track become additional rows. Open
sub-question: should the key include `album_artist_id` (a comp vs. a same-titled
studio album under different album-artists)? Likely yes — folded into [Open
question 3](#open-questions).

## The "meaningful tagset" rule (drop nameless appearances)

Kian's constraint: *don't save a tagset with no artist information — no reason to
grow the "Unknown artist" scope.* Precise predicate, proposed:

> A tagset is **meaningful** iff it resolves to a **non-default artist or a
> non-default album** — i.e. *not* (`album_artist_id` and `artist_id` both the
> reserved `Unknown artist` entity **and** `album_id` the artist's reserved
> `Other` album).

A nameless duplicate adds nothing the surviving blob doesn't already have, so on
absorb it is **dropped** (its blob is trashed; its empty tagset is discarded). The
reserved-key test reuses `DefaultArtistName` / `DefaultAlbumTitle`
(`database/entities.go`), the exact mechanism the library already uses to pin the
Unknown buckets last — so "nameless" is an exact check, not a heuristic.

This rule applies **only at absorb time**. A normal single-tagset upload that
happens to be untagged still gets its (Unknown/Other) primary tagset as today —
we never strip a file's own appearance, we just don't *manufacture extra* empty
ones.

## Serving & playback

All tagsets of a recording share the recording's renditions. Playing **any**
appearance resolves: tagset → recording → ladder-best rendition (or
`preferred_file_id`) → stream that blob. The quality dropdown
(`GET /api/tracks/{hash}/renditions`, [recordings.md](recordings.md) P4) is
unchanged — it's a property of the recording, orthogonal to which appearance you
entered from.

**Addressing shift (API impact).** Today a library track is addressed by **file
hash** and its play URL is `/files/<hash>/<name>`. An absorbed appearance has *no
blob of its own*, so the library track identity must become the **tagset id**, and
its play URL resolves server-side to the recording's best rendition hash. This is
the single biggest client-facing change — every place that keys a track row on
`hash` (library list, queue, playlists, favorites, player) gains a `tagset_id`.
**Playlists/favorites in particular**: do they reference a tagset (a specific
appearance) or a recording (the audio)? Proposed: a **tagset** — a user who
favorited the comp version meant that appearance. [Open question 4](#open-questions).

## Covers

No new work: covers attach to `album_id` (entity), so different albums already
carry different covers, and each tagset resolves its own `album_id`. The recording
itself has no cover (it has no single identity — consistent with
[recordings.md](recordings.md): "a recording carries no tags of its own").

## Library / browse / search impact

Under Approach A the browse/search queries (`database/library.go`,
`ListArtists` / `ListAlbumsByArtistID` / `ListTracksByAlbumID` / search) re-point
their `media_metadata` joins to **`tagsets`**, joining `files` only for the
*serving* rendition and `deleted_at`/access filtering of the **recording's**
renditions. A recording with no surviving (non-trashed) rendition has nothing to
play — its tagsets must drop out of the library even though the tagset rows
persist (so a future restore re-surfaces them). The guest/access filter (below)
must therefore evaluate against *the renditions*, not the tagset.

This is the bulk of the implementation cost and why this is "bigger than it
looks."

## Editing & the track-edit modal

`track-edit.js` + `applyMetadataPatchTx` edit a **file's** `media_metadata`. Under
A they edit a **tagset** (descriptive fields) — mechanical, but the patch target
becomes `tagset_id` not `file_id`, and re-resolution of `artist_id`/`album_id` on
an identity-affecting change is unchanged. Tech-only fields (read-only in the
modal anyway) stay file-keyed. The duplicates page's per-rendition "Edit tags"
becomes per-**tagset**.

## The absorb operation (new moderator action)

New, gated **`content.moderate`**, on `/admin/duplicates`. Proposed shape:

`POST /api/admin/duplicates/{recording_id}/absorb` with a body naming the **kept**
rendition (`keep_file_id`) and the renditions to absorb-and-trash
(`absorb_file_ids`). Atomic (one tx):

1. For each `absorb_file_id`: read its descriptive tags, resolve the appearance
   key, and **if meaningful and not a duplicate appearance**, insert a `tagset`
   on the recording (`origin_file_id` = that file). Otherwise skip the tagset.
2. **Soft-delete the absorbed blobs** (existing soft-delete → Trash; storage
   reclaimed on prune, exactly like a normal duplicate delete).
3. The kept rendition stays; its tagset is the recording's `is_primary`.

One `file.bulk_delete`-style audit row (mirrors the batched bulk-delete pattern).
The selection is always the human's — never auto-absorb (same principle as
"fingerprint grouping is a suggestion, never an auto-delete").

### Reversibility

Absorbed blobs go to **Trash**, so restoring a trashed rendition re-adds it as a
playable rendition of the recording. Open: on restore, does its **original tagset**
come back too (and would it then duplicate an already-absorbed tagset)? Proposed:
the absorbed tagset already represents it; restore only re-adds the **blob**
(rendition), and the appearance-dedup key prevents a double. [Open question
5](#open-questions).

## Split interaction

`SplitRendition` (the inverse: detach a mis-grouped rendition into its own pinned
recording) must now also decide **which tagsets travel with the split-off
rendition**. Proposed: the split-off rendition takes a **copy of the primary
tagset** (so the new recording is browsable), and the moderator fixes it via the
edit modal (the existing "split is usually paired with a tag fix" flow). Absorbed
tagsets stay with the original recording unless explicitly moved. Worth confirming
but low-risk.

## Recordings admin page — managing tagsets under a recording

A second admin surface, **recording-centric**, complementing `/admin/duplicates`
(which is blob/dedup-centric). Proposed route `/admin/recordings`, gated
**`content.moderate`**, in the admin shell (separate tab, page-local preview
player — same pattern as the duplicates page). Where the duplicates page asks
*"which recordings hold redundant blobs?"*, this page asks *"what does this
recording look like — all its appearances and all its renditions — and how do I
curate it?"*

Per recording it shows **both arms of the model**:

- **Renditions** (the blobs): tech + quality-ladder rank, best marked, preview via
  the shared player, trash (reuses the duplicates delete, subject to the
  [deletion guard](#deletion-safety-never-orphan-an-appearance) below).
- **Tagsets** (the appearances): album-artist / album / title / disc·track, the
  primary marked. Per tagset: **Edit** (shared `track-edit.js` modal), **Set
  primary**, **Remove appearance**, and **Move to another recording** (the
  appearance-level analogue of split — reassign a mis-attached appearance).

Default scope: recordings with **>1 tagset or >1 rendition** (the ones worth
curating), plus a search box; single-everything recordings are the long tail and
hidden by default. Reuses the shared **player core** + **`track-edit.js`** modal
(no new player/editor) and follows the established admin-page conventions — nav
link + dashboard card with a count, `nowebui` compiles it out — same as
`/admin/sources` and `/admin/duplicates`.

This page is also **where a blocked hard-delete is resolved** (below): remove or
move the appearances off a recording, or merge it into one that keeps a blob, then
the redundant blob prunes cleanly. A recording-level **merge** (fold one
recording's tagsets + renditions into another) is the natural heavy operation here
but is deferred to a follow-up ([Open question 8](#open-questions)).

## Deletion safety: never orphan an appearance

A tagset has no bytes of its own — it plays through the recording's renditions. So
**permanently deleting the last rendition of a recording that still carries
appearances would silently discard curated metadata** (exactly the album
appearances absorb exists to preserve). That must not happen by accident.

**The guard** runs at every hard-delete boundary — prune, Trash permanent-delete,
and the batched bulk hard-delete (`BulkHardDeleteTrashedByHashes`). Before removing
a `files` row, check its recording:

- The recording keeps **≥1 other surviving rendition** → safe; proceed (the
  appearances still play).
- This is the recording's **last** rendition, and:
  - its only tagset is **this file's own appearance** (`origin_file_id` = the file
    being deleted) → a normal whole-track delete; remove that tagset along with the
    file. **Allowed.**
  - the recording has **other** tagsets (`origin_file_id` ≠ this file, or NULL —
    appearances absorbed from already-purged copies) → deleting the blob would
    orphan them. **Blocked.**

A blocked delete is **not** a dead end: the moderator resolves it on the recordings
page — **remove** the extra appearances (if unwanted), **move** them to another
recording that has a blob, or **merge** recordings — after which the blob prunes
normally. For the deliberate case, a `content.moderate` **override** ("permanently
delete and discard N appearances") is offered behind a count-aware confirm, so
curated metadata is never thrown away without an explicit choice
([Open question 9](#open-questions)).

The invariant, precisely: **a recording with appearances always retains a playable
rendition, unless a moderator explicitly discards those appearances.** Trash
(soft-delete) is reversible, so it never triggers the guard — only permanent
removal does.

This is reachable *only* under the tagset model: today tags cascade away with their
file (`media_metadata` PK `file_id`), so there is nothing to orphan. Once tags
outlive blobs, this guard is what keeps the model honest.

## Access control, license, review state

Today these are **file-level** (`files.license`, `guest_playable`, `review_state`,
`deleted_at`). With one blob serving N appearances, the natural home is the
**rendition** (file) that actually serves — a tagset has no bytes to license or
gate. So:

- **Guest/access/license** of an appearance = that of the rendition it streams
  (the recording's served blob). The library access filter evaluates the
  **renditions**, not the tagset.
- **Review state**: an absorbed tagset comes from an *already-reviewed*
  duplicate. Does it need its own moderation, or does it inherit the recording's
  approved state? Federation makes this sharper (an appearance arriving from a
  peer). This is **[Open question 6](#open-questions)** and the trickiest — see
  Federation.

## Federation notes (and security)

A recording is "the first content identity portable across nodes"
([recordings.md](recordings.md)). Tagsets are the **catalog-metadata half** of the
metadata-vs-stream split [Federation](federation.md) plans ("pull/cache a peer's
catalog metadata so a friend's library is browsable, stream audio on demand by
hash"): a node advertises *"recording `<fingerprint>` — I hold a FLAC master and
these appearances"*, peers join on the **cross-node fingerprint index**
([recordings.md](recordings.md), [federation.md](federation.md) open Q6) and
reconcile the **union** of appearances. The optimization story the user named —
keep the best master, drop redundant blobs, but never lose an album appearance —
falls straight out of this model, and rides the same content-hash "just another
holder of hash X" dedup federation already leans on for replication.

**Security / trust (designed in [federation.md](federation.md); the constraints
tagsets place on that design, noted now):**

- **Tagset poisoning.** A malicious or sloppy peer could attach bogus or abusive
  appearances to a recording another node holds (wrong/defamatory artist, NSFW
  album art via a poisoned `album_id`). A peer-contributed tagset therefore records
  its **origin node** — the appearance-level analogue of the per-file origin-node
  reference federation.md already plans (`auth.md` §8) — and is shown/merged only
  per the **trusted-peer** policy: local tagsets trusted, peer tagsets
  weighted/filtered by the federation trust layer. A local moderator must be able to
  **reject** a peer-contributed appearance (it routes through moderation's
  "federation approvals" audit hook).
- **Cross-context license/access leak.** An appearance arriving from a peer must
  **not** silently widen local access (e.g. mark a recording guest-playable
  because a peer's tagset said so). Access stays a property of the **local
  rendition**, never imported from a tagset. (This is why access lives on the
  rendition, not the tagset — above.)
- **Dedup-key spoofing.** A peer crafting a tagset whose `(album_id, track)` key
  collides to suppress a real appearance — mitigated by keying appearance dedup on
  **locally-resolved** entity ids, not peer-supplied ones.
- **Revocation.** federation.md treats clean peer revocation as a hard requirement:
  removing a peer from the trusted table must cut its content access *and* drop or
  hide the appearances it contributed. A peer-sourced tagset must therefore stay
  attributable to its `origin_node` rather than silently folding into the local
  union — revocation is a filter on origin, not a destructive merge to undo.

All federation behavior is **deferred** to [federation.md](federation.md)'s design
session — this section only fixes the data model (provenance columns, access on the
rendition) so it doesn't have to be reshaped when Phase 4 arrives (the same way the
fingerprint table was "storage now, index later").

## Phase plan (proposed)

- **P0 — Data model.** Migration: introduce `tagsets`, decompose `media_metadata`
  (descriptive → tagsets, tech stays), backfill one primary tagset per file,
  give every file a singleton recording. Resolver + backfill (mirrors the
  `BackfillRecordings` / `FoldUnknownBuckets` startup-pass pattern). Data layer
  only; library still reads the primary tagset, so behavior is identical.
- **P1 — Library/serving on tagsets.** Re-point browse/search/cover/access
  queries to tagsets; tagset-id addressing + play-URL resolution; player/queue/
  playlists carry `tagset_id`. No behavior change yet (one tagset each).
- **P2 — Absorb action + deletion guard.** `/admin/duplicates` "Keep best + absorb
  others" UI + `POST …/absorb`; meaningful-tagset drop rule; appearance dedup;
  audit. The deletion guard (prune / Trash permanent-delete / bulk hard-delete)
  lands here too — absorb is what first creates a single-blob, multi-appearance
  recording, so the guard must exist before it can be orphaned. The visible feature
  lands here.
- **P3 — Recordings admin page + split-with-tagsets.** `/admin/recordings`
  (recording-centric: renditions + tagsets, edit / set-primary / remove / move
  appearance), the resolution surface for blocked deletes; split carries a copy of
  the primary tagset; edit-modal-on-tagset polish. (Recording-level merge deferred.)
- **P4 (future) — Federation.** Tagset sync, provenance/trust, union reconcile.

## Test plan

Tests land **with each phase**, not after. The two highest-risk areas — the
**deletion guard** and the **meaningful-tagset / appearance-dedup rules** — are
table-driven and exhaustive; they decide whether curated metadata or audio gets
silently lost, so they get the most coverage. Layers map onto the project's
existing surfaces (`database/*_test.go`, the `api` package's `fakeRepo`,
`tests/js`, `tests/playwright`, `tests/k6`).

### Migration & backfill (P0) — `database`

- **Decomposition round-trip**: a fixture DB at the pre-migration schema, after
  migrate, has every file's old descriptive tags on exactly **one** primary tagset,
  tech columns still on `media_metadata`, and **no data loss** (assert column-by-
  column equality against the pre-migration snapshot).
- **Singleton recordings**: every file ends with a non-null `recording_id` (incl.
  fpcalc-absent fixtures); no file is left tag-homeless.
- **Idempotent backfill**: the startup pass re-run is a no-op (mirrors the existing
  `BackfillRecordings` / `FoldUnknownBuckets` idempotency tests).
- **Standing regression updates** (the migration/repo gotcha): bump
  `database_test.go`'s schema-version + table-set assertions for the new `tagsets`
  table; extend the `api` package's `fakeRepo` for every new `Repository` method
  (absorb, tagset CRUD, guard check) or the api tests won't compile.

### Resolver, identity & rules (P0/P2) — `database`

- **Resolver parity**: the tagset resolver produces the *same* `artist_id` /
  `album_artist_id` / `album_id` as the `media_metadata` resolver for identical
  tags (shared code — assert they can't drift).
- **Appearance dedup** (the `UNIQUE` key): inserting two tagsets with the same
  `(recording_id, album_id, disc, track[, album_artist])` collapses to one.
- **Meaningful-tagset predicate** — table-driven, including the literal-reserved-key
  cases (a tagset literally tagged `Unknown Artist` / `Other` is *dropped*; one
  with any real artist **or** real album is *kept*; album-only and artist-only both
  kept).

### Absorb operation (P2) — `database` + `api`

- Happy path: keep best, absorb N → recording gains the meaningful tagsets,
  absorbed blobs go to Trash, **one** audit row.
- **Atomic rollback**: an injected mid-absorb failure leaves no partial tagsets and
  no half-trashed blobs (single-tx assertion).
- Drop rule: absorbing a nameless duplicate trashes its blob but creates **no**
  tagset.
- Dedup: absorbing an identical-album/track copy adds no new appearance.
- Authz: non-`content.moderate` → `403`; bad ids → `400/404/409`.

### Deletion guard (P2) — `database` + `api` *(critical path)*

Exhaustive matrix:

- last rendition + only the file's own tagset → **allowed**, tagset removed with it;
- last rendition + extra tagsets (`origin_file_id` ≠ file, **and** the NULL case) →
  **blocked**, nothing deleted;
- non-last rendition (≥1 other survives) → **allowed** regardless of tagsets;
- override flag + `content.moderate` → deletes blob **and** discards N appearances;
  without the permission → still blocked;
- **all three entry points** enforce it — single prune, Trash permanent-delete, and
  the batched `BulkHardDeleteTrashedByHashes` (a bulk set mixing safe + guarded rows
  behaves per Open Q9's decided semantics);
- Trash (soft-delete) never trips the guard.

### Library, serving & access (P1) — `database` + `api`

- A 1-blob / N-tagset recording surfaces as **N** library tracks; browse by each
  `album_id` shows its appearance; each appearance's play URL resolves to the
  recording's ladder-best rendition hash.
- A recording with tagsets but **zero** surviving renditions drops out of the
  library; restoring a rendition re-surfaces them.
- **Access on the rendition, not the tagset**: an appearance is guest-hidden /
  gated iff its serving rendition is — assert a tagset can never widen access (also
  the cheap federation-safety lock to land now: an imported tagset must not flip
  `guest_playable`).
- Tagset-id addressing: playlists / favorites / queue carry `tagset_id` and resolve
  to the right appearance (per Open Q4).

### Concurrency (P2) — `database/busy_test.go`

Absorb and guarded bulk-delete are multi-row single-tx ops; add a regression that
they run under `_txlock=immediate` with **no `SQLITE_BUSY`** (the bulk paths have
bitten us before — the guard must be folded *into* the bulk tx, not a second one).

### Client (P2/P3) — `tests/js` + `tests/playwright`

- **`tests/js`** (node `--test`, the `queue-ops` style): the recordings-page
  selection arithmetic — the "keep best + absorb others" selection model and
  rendition-vs-tagset selection — as pure functions, no DOM.
- **`tests/playwright`** (chromium-only): the moderator end-to-end —
  (a) `/admin/duplicates` keep-best + absorb → the song then appears under **both**
  albums in the library and both play the same blob;
  (b) `/admin/recordings` edit an appearance, remove an appearance, hit a **blocked**
  delete and see it refused, then resolve it and prune cleanly.
  (Reuse the known login DOM facts; never run PW install-deps on Fedora.)

### Performance (P1) — `tests/k6` (light)

The feature is moderator-side, so load testing is minimal — but Approach A re-points
the hot `ListTracksByAlbumID` / search joins onto `tagsets`. One k6 check that
browse-by-album latency does **not** regress against the pre-migration baseline
guards the only player-facing perf risk.

## Non-goals (v0)

- **Auto-absorb / auto-merge appearances** — like grouping, absorb is always a
  human action on the duplicates page.
- **A track-level "work" entity** (one composition across different recordings) —
  still out, same as [recordings.md](recordings.md). Tagsets are appearances of
  **one** recording, not a cross-recording grouping.
- **Multi-credit "feat." parsing** in a tagset — a tagset still has one performer,
  same limitation as today's `media_metadata`.
- **Federation sync itself** — model only (above).
- **Per-appearance quality preference** — every appearance plays the recording's
  ladder-best; no per-tagset rendition pin.

## Open questions

1. **Approach A (decompose `media_metadata`) vs. B (additive
   `recording_tagsets`)?** A is the clean end-state and what this doc assumes, but
   it's a library-wide migration; B is lighter but carries two parallel "library
   track" sources forever. *Recommendation: A* (pre-release; the project favors
   clean models over legacy paths).
2. **Make `recording_id` non-null for every file** (singleton recording even
   without a fingerprint)? Needed for A. *Recommendation: yes* — small cost, big
   simplification. Confirms a change to recordings.md's "NULL = implicit
   recording".
3. **Appearance dedup key** — `(recording_id, album_id, disc_number,
   track_number)`, and should `album_artist_id` be in it? *Recommendation: include
   album_artist_id* (distinguishes a comp from a same-titled studio album).
4. **Do playlists/favorites/queue reference a tagset (appearance) or a recording
   (audio)?** *Recommendation: tagset* — the user picked a specific appearance.
5. **On restoring a trashed absorbed rendition, re-add only the blob, relying on
   appearance-dedup to avoid a duplicate tagset?** *Recommendation: yes.*
6. **Review state of an absorbed tagset** — inherit the recording's approved state,
   or require its own moderation pass? Sharper under federation (a peer-contributed
   appearance routes through moderation's "federation approvals" hook,
   [federation.md](federation.md)). *Leaning: inherit for locally-absorbed, gate for
   peer-sourced — but this needs Kian's call.*
7. **Federation trust for tagsets** — confirm the direction (provenance +
   trust-weighted union, revocable by `origin_node`, access never imported from a
   tagset) so the P0 schema carries the right columns even though sync is deferred to
   [federation.md](federation.md). Open there: whether the sharing-scope model needs
   **per-album/artist** federated scopes ([federation.md](federation.md) open Q3) —
   tagsets are album/artist-grained, so they're the level such a scope would bite.
8. **`/admin/recordings` default scope** — show only recordings with >1 tagset or
   >1 rendition (+ search), and **defer** recording-level merge to a follow-up?
   *Recommendation: yes to both* (the long tail of single-everything recordings is
   noise; merge is a heavier op best designed on its own).
9. **Blocked hard-delete behavior** — hard-block with an explicit `content.moderate`
   "discard N appearances" override, vs. always-allow behind a confirm?
   *Recommendation: block + explicit override* — losing curated appearances should
   require an intentional act, not a reflexive "yes".

## Gotchas

- The P0 migration bumps `database_test.go`'s version/table assertions and any new
  `Repository` method breaks the `api` package's `fakeRepo` (the standing
  migration/repo gotcha).
- Decomposing `media_metadata` touches **every** query that reads descriptive tags
  — this is the largest single change in the project's library layer. Stage it
  behind P0/P1 (identical behavior) before the visible P2 feature so a regression
  is isolated to the data move, not the new action.
- The required-name default for a tagset `title` has no filename to fall back to
  when the origin blob is gone — but the meaningful-tagset rule drops nameless
  tagsets anyway, so a kept tagset always has real tags. Confirm the edge.
- The deletion guard must cover **all three** hard-delete paths — single prune,
  Trash permanent-delete, and the batched `BulkHardDeleteTrashedByHashes` — or a
  bulk prune silently reintroduces the orphan it's meant to prevent. The bulk path
  needs the recording check folded into its single-tx delete, not bolted on after.
