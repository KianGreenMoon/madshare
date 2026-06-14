# Recordings — same-audio grouping & renditions

**Status:** proposed (design only; not implemented). Builds on the
artist/album overlay (`docs/architecture/artist-album-model.md`) and the
moderation queue (`docs/architecture/moderation.md`). Federation-relevant: a
recording is the first content identity that is **portable across nodes**.

## Problem

Content-hash dedup (`files.hash UNIQUE`) collapses byte-identical uploads, but
the *same audio in a different encoding* — a FLAC master and a 320 kbps MP3 of
one track — has different bytes, so today it lands as two unrelated `files`
rows with no link between them. The library shows the same song twice; nothing
knows they are renditions of one thing; there is no way to pick the best one to
play, prune accidental re-uploads, or (on federation) recognise that two nodes
hold the same track.

Tags can't solve this: they are dirty, and `artist+album+title` collides across
live/studio/remaster/radio-edit versions that are genuinely *different*
recordings. We need an identity derived from the **audio itself**.

## Model: a recording overlay on files

Same shape as the artist/album overlay — a surrogate entity plus an FK, with the
raw data left untouched. A **recording** is a group of `files` that are the same
audio; each member file is a **rendition** (a specific encoding). Most
recordings have exactly one rendition; the interesting ones have several.

```sql
CREATE TABLE recordings (
  id                INTEGER PRIMARY KEY,
  created_at        INTEGER NOT NULL,
  -- nullable manual override of the auto-ranked "best" rendition (see Quality
  -- ladder). NOT surfaced in the UI in v0 — an escape hatch for the one case
  -- the ladder gets wrong (lossy-sourced upscales). NULL = use the ladder.
  preferred_file_id INTEGER REFERENCES files(id) ON DELETE SET NULL
);

-- Overlay FK on the blob (recording identity is content-derived, so it lives on
-- files, next to hash — not on media_metadata, which carries tag-derived ids).
ALTER TABLE files ADD COLUMN recording_id INTEGER REFERENCES recordings(id);
-- A file the moderator has manually split/pinned: the resolver must never
-- re-merge it on a future pass (see Split-off).
ALTER TABLE files ADD COLUMN recording_pinned INTEGER NOT NULL DEFAULT 0;
CREATE INDEX idx_files_recording ON files(recording_id);

-- The fingerprint is a property of the audio bytes, computed once at ingest.
-- Kept in its own table (it is a few KB, cold relative to the hot files row,
-- and federation will share it).
CREATE TABLE audio_fingerprints (
  file_id      INTEGER PRIMARY KEY REFERENCES files(id) ON DELETE CASCADE,
  algo         TEXT    NOT NULL,   -- e.g. 'chromaprint', for forward-compat
  algo_version TEXT,               -- fpcalc / chromaprint version string
  duration     REAL,              -- fingerprinted duration (fpcalc reports it)
  fingerprint  BLOB    NOT NULL,   -- the compressed fpcalc fingerprint
  created_at   INTEGER NOT NULL
);
```

The recording is an **editable overlay**: it groups files, but the files' bytes,
hashes, and tags are never mutated. A recording carries no tags of its own — its
display name is derived from its best rendition's `media_metadata` (so renaming
is still an artist/album/track-tag concern, unchanged).

Recordings are **independent of tags and of artist/album**. Two files with
slightly different tags but the same audio are one recording; a recording's
renditions may even span albums (a track on the album and on a compilation).
For display/browse the library keeps using the tag-derived entities; the
recording layer sits *underneath* a track and only matters where >1 rendition
exists.

## Identity: acoustic fingerprint

Identity is the **Chromaprint acoustic fingerprint** (the engine behind
AcoustID/MusicBrainz), computed at ingest by shelling out to **`fpcalc`** — no
CGo, same pattern as the planned `ffprobe` call. It reads only the first ~2 min,
downsamples to a chromagram (12 pitch classes over time), and emits a compact
int-array fingerprint. Because it is coarse and pitch-based, it is **robust to
re-encoding**: FLAC and 128 kbps MP3 of one master fingerprint nearly
identically — exactly the case content-hash can't catch.

Two files are the same recording when their fingerprints align within a
bit-error-rate threshold. Known limits (why this is a **suggestion**, never an
auto-destructive merge):

- **Speed/pitch shifts** (vinyl rip 1% fast, 432 Hz re-tune) break matching —
  false negative, lands as a separate recording. Acceptable (no data lost).
- **Structural edits** (radio edit, extra leading silence) only match on the
  overlap; big edits fall below threshold — also a clean false negative.
- Heavy remasters can drift past threshold.
- It identifies the **recording**, so covers and live versions correctly come
  back *different* — which is the goal, and what makes "play the best one" safe.

So fingerprinting is confident enough to **auto-group as a candidate**, not
confident enough to auto-delete. All deletion stays human-confirmed.

### Matching at scale

Comparing two fingerprints is cheap (Hamming distance). "Which existing file
matches this one?" is the scaling concern. v0 single-server: shortlist by a
coarse sub-fingerprint bucket (or, lacking an index, scan within the same
tag-key cluster) and bit-compare only the shortlist. A real index and
cross-node matching are the **federation** problem and deferred — the table
above is the storage that a later index sits on top of.

### Graceful degradation (no hard dependency)

Both binaries are optional and degrade **independently**; startup must **warn,
not fail** when either is absent — same spirit as `Deps.SourceRoot == ""`
disabling `/source`.

**`ffprobe` absent** → the tech columns (`duration_seconds / bitrate /
sample_rate / channels / codec / bit_depth`) stay NULL. The quality ladder
degrades to **format + size only** — codec class inferred from the persisted
canonical MIME, size from the `files` row, both available with no binary — and
the duplicates / moderation side-by-side compare shows only format + size, not
bitrate or sample rate.

**`fpcalc` absent** → no `audio_fingerprints` row, the resolver leaves
`recording_id` NULL (every file is its own implicit recording), and the
recordings overlay lies dormant: the duplicates admin page, which lists
multi-rendition *recordings*, is necessarily empty because nothing is grouped.

Rather than disabling duplicate handling outright, fpcalc-absent falls back to
an **active tag-collision check at the moderation gate**. Tag identity is far
weaker than fingerprinting (see Problem), so this is a fallback *warning* —
never grouping, never deletion:

- At submit time, derive whether another **approved, non-trashed** file shares
  the same **artist + album + title**. If so, the submission is duplicate-flagged
  exactly like a fingerprint match: the `content.moderate` self-approve shortcut
  is **suppressed** (even for admins/moderators), the uploader gets a warning
  (the same popup channel as the post-upload info notice), and it routes to the
  moderation queue for a human decision. Mirrors Moderation integration.
- The flag is **derived, not stored** — same as the fingerprint flag (see
  Moderation integration). When `fpcalc` is later installed and the backfill
  runs, identity switches from tags to fingerprint truth with no migration and
  no stale "tagged as duplicate" rows to clean up.
- **Default / empty tag-keys are excluded** from matching. The required-name
  defaults turn an untagged file into `Unknown artist / Other / <filename>`, so
  matching on those would flag every untagged upload against every other one.
  Only real, non-default `artist + album + title` triples participate.

This fallback touches **only the moderation gate** — it never creates a
recording, sets `recording_id`, or populates the duplicates page. Tags can't
safely group audio (see Problem); they can only raise a "look at this" flag for
a human.

### MBID — deliberately out (for now)

A MusicBrainz recording id already in a file's tags is a free, authoritative
identity, but we chose fingerprint-only to keep identity content-derived and
tag-independent. An MBID corroborator is a cheap **additive** signal later (skip
fpcalc when a trusted MBID is present) — noted, not built.

## Quality ladder (no preferred picker)

Within a recording, renditions are ranked by a **deterministic** ladder computed
from the tech columns — no human picks the default. The ladder, best-first:

1. **Codec class** — lossless (`flac`, `alac`) before lossy (`mp3`, `aac`,
   `vorbis`, `opus`).
2. Then the ladder **branches on class**, because the meaningful axis differs:
   - **Lossy** → **bitrate** (`media_metadata.bitrate`), higher first. (128 vs
     320 kbps is a real quality difference.)
   - **Lossless** → **sample rate** then **bit depth** (24/96 above 16/44).
     Bitrate is *not* used here: a lossless file's bitrate is a byproduct of how
     busy the audio is, not a quality dial, so a dense track would wrongly
     outrank a sparse one at the same resolution.
3. **File size** as the final tiebreak.

`bit_depth` is the column that distinguishes two lossless renditions (it only
bites for libraries holding multiple lossless copies of one recording at
different resolutions); added in P0 alongside the other tech columns.

The one case the ladder gets wrong is a **lossy-sourced upscale** (a 320 MP3
re-encoded to FLAC): fpcalc correctly groups it, but the ladder sees "lossless"
and ranks the fake top. `recordings.preferred_file_id` is the escape-hatch
override for exactly this, left in the schema but unsurfaced in v0. A future
refinement is spectral-cutoff detection to flag upscales automatically — not
now.

The ladder needs the tech columns, which are **NULL in v0** — see Prerequisites.

## Renditions & adaptive playback

A "rendition" is just a file viewed as a member of a recording — there is no
separate rendition table. The play path exposes the recording's renditions and
lets the client choose:

- A track's API payload gains its recording's rendition list: per rendition the
  `hash`, codec/format, bitrate, sample rate, size, duration, and ladder rank.
- The player gets an **Auto / High / Low** quality control. Auto = the ladder's
  best that fits; Low/High = walk the ladder. (Auto can later measure bandwidth;
  a federation node can advertise its own ceiling.)
- Delivery is plain **HTTP range requests** — no segmenting needed for "pick one
  rendition and stream it." **Confirmed already in place:** `/files/*` is served
  by `http.FileServer` (`api/api.go`, `fileServer()`), which streams every blob
  through `http.ServeContent` — full `Accept-Ranges`/`206`/`If-Range` support,
  so a browser already seeks/resumes a 2-hour podcast over the content-hash
  blob. P4 adds no delivery machinery.

**Segmented ABR (HLS/DASH) is explicitly a video-era feature, not built here.**
Mid-stream quality switching is the *only* thing segmenting buys over range +
rendition-pick, and for audio it isn't worth the cost (full ffmpeg transcode per
rendition, storage × renditions in many tiny segment files, a derived-artifacts
store outside the one-file-one-hash model, and an `hls.js` front-end dependency
against the vanilla-JS rule). The rendition abstraction is kept deliberately
**format-agnostic** — "a playable thing with tech specs + a URL" — so a later
video rendition can *be* an HLS manifest without reshaping this model.

## Duplicates / variants admin page

A new moderator-accessible sub-page (gated **`content.moderate`** client-side,
like `/admin/moderation`; reuses the shared `track-edit.js` modal and a
page-local player). Lists every recording with **>1 non-trashed rendition**:

- Per recording, the renditions side by side with tech info (format, bitrate,
  sample rate, duration, size) and the ladder rank, the best one marked, plus a
  **suggestion** ("keep #1, others are strictly lower quality" / "these differ
  only in format — may be intentional variants").
- **Delete a duplicate, with confirmation** — reuses the existing soft delete
  (`docs/architecture/soft-delete.md`); the blob and its `files` row go to
  Trash like any other delete. No auto-delete, ever.
- **Split off as a separate recording** — the "save as another composition"
  action: detach a rendition into its own new recording and set
  `recording_pinned = 1` so the resolver never re-merges it (fingerprint
  identity means a tag edit alone wouldn't re-group it — the pin is what makes
  the human's "this is actually the live version" stick). Typically paired with
  a tag fix via the existing metadata edit.

No preferred-picker UI: the ladder is automatic; `preferred_file_id` is set only
by the (future) override path.

## Moderation integration

An upload whose fingerprint matches an **existing recording that already has an
approved rendition** is a possible duplicate and is treated specially:

- It is **never auto-approved** — the `content.moderate` self-approve shortcut
  (`docs/architecture/moderation.md`) is **suppressed** for a duplicate-flagged
  submission, even for admins/moderators. It must pass an explicit human look.
- The moderation queue **highlights** it as "variant of an existing recording"
  and shows it side by side with the existing rendition(s) + tech comparison, so
  the moderator decides: keep as an intentional variant, discard as redundant,
  or split (it's actually different).

The flag is **derived**, not a stored state: a submission is duplicate-flagged
iff its recording already has another approved, non-trashed rendition — no new
column on the review state machine. (Suppressing self-approve keys off that same
derivation at submit time.) This depends on the fingerprint existing at submit
time, so ingest fingerprinting must complete before/at submission.

## Resolver & backfill

Mirrors the artist/album startup reconcile pass exactly (the orphan-blob /
`FoldUnknownBuckets` pattern in `madshare.go`):

- **Inline at upload** — after the analysis job computes the fingerprint, the
  resolver matches it against existing fingerprints and either assigns the file
  to the matched recording or creates a new one. Pinned files are skipped.
- **Startup backfill** — idempotent, processes only `files` with NULL
  `recording_id` (and `recording_pinned = 0`). For existing blobs that predate
  fingerprinting it must first compute the fingerprint (a one-time, heavier
  pass — runs through the analysis job queue below, not inline at boot).

Single-rendition recordings are the norm; the resolver creating "a recording per
file" for unmatched audio is expected and cheap.

## Prerequisites (P0) — ingest media analysis

The whole feature rests on two ingest-time decode passes we don't do yet, both
shelling out to an external binary (no CGo), both naturally one job:

- **`ffprobe`** → populates the reserved-but-NULL tech columns
  (`media_metadata.duration_seconds / bitrate / sample_rate / channels / codec`)
  and adds a new **`bit_depth`** column. Needed for the quality ladder and the
  duplicates table's tech info.
- **`fpcalc`** → the `audio_fingerprints` row. Needed for identity.

Proposed as a single DB-backed **`media_analysis_jobs`** queue drained by a
worker pool, mirroring `image_processing_jobs` / `imageproc.Pool` (migration 009):
enqueue on upload, run ffprobe + fpcalc, write the columns + fingerprint row,
then resolve the recording. Reusing that pattern gives us stale-job reset on
boot and a bounded worker pool for free. Both tools are optional (degrade per
above).

## Phase plan

- **P0 — Ingest media analysis.** `media_analysis_jobs` queue + worker; ffprobe
  fills tech columns, fpcalc fills `audio_fingerprints`. Backfill existing blobs.
- **P1 — Recording overlay.** `recordings` + `files.recording_id` /
  `recording_pinned` + resolver (inline + startup backfill). Data layer only.
- **P2 — Duplicates admin page.** List multi-rendition recordings, tech compare,
  delete-with-confirm, split-off. Moderator-accessible.
- **P3 — Moderation integration.** Derived duplicate flag, suppress self-approve,
  side-by-side highlight in the queue.
- **P4 — Adaptive playback.** Rendition list in the track API + Auto/High/Low
  player control + range-request delivery.
- **P5 (future) — ffmpeg auto-transcode.** Generate missing renditions ourselves
  *into* this model (e.g. a small streaming copy beside a lossless master).

## Future directions (beyond P5)

### Canonical master + derived streaming renditions

The natural end state of the ladder + P5: designate a recording's top rendition
as its **canonical master** (the `preferred_file_id` slot) and have ffmpeg
generate the smaller streaming renditions *from it* — the small copies become
ordinary (lower) renditions feeding the Auto/Low playback tier.

- **Always transcode from the best — ideally lossless — master**, never from an
  already-lossy copy, or you stack generational loss (FLAC→Opus beats MP3→Opus
  audibly).
- **Derived renditions are generated, not uploaded:** they carry provenance
  (`derived_from` = the master file), **skip moderation** (the master was
  already approved), and **skip fingerprint matching** (their parent is known).
- **Federation:** shared fingerprint identity lets a node discover that a peer
  holds a *higher-ladder* rendition of a recording it already has and pull that
  better master. Streaming copies are then cheap to regenerate **locally per
  node** (CPU is cheaper than re-transferring large files) rather than shipping
  them around — a federation-phase trade-off.

### Authenticity / tamper checks (MusicBrainz-assisted)

Important framing: fingerprinting is an **identity** tool, not a **forensics**
one. An AcoustID/MusicBrainz match tells you *"this is recording X"* (→ an MBID);
it is deliberately tolerant of re-encoding, so a match is **not** proof of an
unmodified original. Acceleration in particular *breaks* the fingerprint
(a sped-up file fails to match) rather than being flagged by it.

What *does* work, as additive heuristics:

- **MBID → canonical-duration cross-check.** Match the fingerprint to an MBID,
  then compare our `duration_seconds` to MusicBrainz's authoritative track
  length. Everything matches but our copy is ~3 % shorter → a strong
  "time-stretched / accelerated / edited" flag. The practical "is this
  tampered?" signal.
- **Spectral-cutoff detection** catches *fake* lossless (a FLAC transcoded up
  from a 128 kbps MP3 shows a ~16 kHz ceiling) — also the ladder's upscale
  blind spot (see Quality ladder).

These are exactly the use cases that would justify **reintroducing the
MusicBrainz/AcoustID corroborator** deferred above (additive signal). Cost to
weigh: AcoustID is an online API (key + network) or a large local MusicBrainz
mirror — relevant for an offline/federated node.

The honest ceiling: audio analysis cannot *prove* a file is the bit-for-bit
original. True authenticity/provenance is a **trust** concern — a trusted node
**cryptographically signs** "this blob is the master I vouch for" — and belongs
to the federation trust layer (`docs/architecture/auth.md` §8, Phase 4), not to
fingerprinting.

## Non-goals (v0)

- **Segmented ABR / HLS / DASH** — video-era, not audio. Range + rendition-pick
  only (see Renditions & adaptive playback).
- **Cross-node fingerprint matching / a fingerprint index / AcoustID lookups** —
  the federation problem; the storage is here, the index is not.
- **MBID-based identity**, spectral-cutoff upscale detection, and any
  tag-based **grouping** — additive later, not built. (The fpcalc-absent
  tag-collision check in Graceful degradation is a moderation *warning*, not
  grouping: it never creates a recording or sets `recording_id`.)
- **Auto-merge or auto-delete** — fingerprint grouping is a suggestion; every
  deletion is human-confirmed; every split is human-pinned.
- **A track-level "work" entity** (one composition across different recordings/
  performances) — fingerprinting deliberately keeps performances separate; a
  looser "work" grouping is out of scope.

## Gotchas

- New migration (next is **019**) bumps the `database_test.go` version/table
  assertions; new `Repository` methods break the api package's `fakeRepo`
  (`docs/` migration-gotchas note).
- Fingerprint/tech analysis depends on external binaries — keep startup a
  warning, never fatal, when they're absent.
