# Plan: artist/album normalization (overlay)

**Status:** planned (owner-approved 2026-06-07). Design:
`docs/architecture/artist-album-model.md`.

**Sequencing:** do `docs/plans/access-roles-only.md` **first**. Once Layer B is
gone, `accessClause` no longer references artist/album strings, so the query
rewrite here is a pure library/cover change with no access-control risk. Migration
numbers below assume the access-drop migration took `011`.

## Goal

Add stable `artists`/`albums` entities and `artist_id`/`album_id` FKs on
`media_metadata`, keeping the raw tag text (overlay). Re-key cover tables to
entity IDs. Enable rename and (later) merge without orphaning covers.

## Shared resolver (build first, used everywhere)

A single package-level helper is the contract that prevents import/backfill drift:

```go
// normalizeKey: NFC → trim → collapse internal whitespace → lowercase.
func normalizeKey(s string) string

// resolveAlbumArtist: get-or-create the artist + album entities for a track's
// tags, returning their ids. Idempotent; uses INSERT … ON CONFLICT on the
// norm_* unique keys so concurrent imports converge (cf. SetAlbumCoverIfAbsent).
func (db *DB) resolveAlbumArtist(ctx, tags) (artistID, albumID int64, err error)
```

`resolveAlbumArtist` applies the identity rules from the design doc (effective
artist = album_artist|artist|placeholder; album = (artist_id, norm_title);
empty title → unknown-album placeholder). Concurrency: same atomic-claim pattern
already proven for covers — rely on the `UNIQUE` constraints + `ON CONFLICT`, not
on a read-then-write check.

## Phase 1 — schema + resolver + backfill

1. **Migration `012_artist_album_entities.sql`:**
   - `CREATE TABLE artists (…)`, `CREATE TABLE albums (…)` (see design doc).
   - `ALTER TABLE media_metadata ADD COLUMN artist_id …; ADD COLUMN album_id …;`
     plus the two indexes.
   - Re-key the image tables. SQLite can't drop/rename a PK column in place, so
     rebuild: create `artist_images_new(artist_id PK, …)` /
     `album_images_new(album_id PK, …)`, leave them empty (the backfill fills
     them from the old tables via resolution), drop the old tables, rename. **Or**
     keep the old tables until the backfill has copied rows, then drop — decide in
     implementation; the safe order is: add new tables, backfill copies, drop old.
2. **Resolver** (`normalizeKey`, `resolveAlbumArtist`) in `database/`.
3. **Startup backfill reconcile pass** in `madshare.go` (next to the orphan-blob
   pass): for every `media_metadata` row with NULL `artist_id`/`album_id`,
   resolve + set the FKs; for every old `artist_images`/`album_images` row,
   resolve the name(s)→entity and insert into the new image tables. Idempotent and
   re-runnable; logs a one-line summary.
4. **Tests:** resolver unit tests (normalization cases, compilation/Various
   Artists, empty title→placeholder, concurrent get-or-create single-winner);
   backfill test on an on-disk DB seeded with pre-entity rows + cover rows.

> After this phase the FKs exist and are populated but nothing reads them yet —
> safe to ship incrementally.

## Phase 2 — import path

Wire `resolveAlbumArtist` into the upload/metadata-extract path so new inserts
set `artist_id`/`album_id` inline (no reliance on the backfill for fresh
uploads). The `PATCH /api/files/{hash}/metadata` edit path must **re-resolve**
entities when `artist`/`album_artist`/`album` change (and leave covers attached
to whatever entity the track now points at). Tests for both paths.

## Phase 3 — query rewrite (library + search)

Rewrite `database/library.go` to JOIN on `artists`/`albums` instead of `GROUP BY`
over COALESCE expressions (see design doc query shape):

- `ListArtists`, `ListAlbumsByArtist`, `ListTracksByAlbumArtist`, `search`.
- With Layer B gone these are the **only** variants (the `*Guest` split from the
  access plan stays: full-library vs guest predicate, but neither references
  groups). Confirm the access-drop rename landed first.
- **Routing decision:** keep the existing name-based endpoints
  (`?artist=`/`?album=`) working by resolving name→id at the handler edge, so the
  front-end need not change in this phase. Add `?artist_id=`/`?album_id=` as the
  preferred params; migrate the UI later. (Avoids a lockstep API+UI change.)
- Update `ArtistEntry`/`AlbumEntry` to carry the new `ID` (additive).
- Tests: the library/search query tests assert the same results as before plus
  stable IDs across calls.

## Phase 4 — cover handlers on IDs

Point `UpsertArtistImage`/`GetArtistImage`/`UpsertAlbumImage`/`GetAlbumImage`,
`SetAlbumCoverIfAbsent`, `HasAlbumCover`, the image-status endpoint, and the
embedded-cover claim path at `artist_id`/`album_id`. The `image_processing_jobs`
queue and `<files_dir>/images/<base_key>/` layout are unaffected (keyed by
`base_key`, not by album identity). Update `docs/api/cover-images.md`.

## Phase 5 — rename (and optionally merge)

- **Rename:** admin endpoint + UI to edit `artists.name` / `albums.title` (updates
  `norm_*` too). One-row update; covers/tracks follow via FK. This is the payoff
  of the whole change and the answer to the Phase-5 cover-rename workaround in
  `upload-and-covers.md` (that workaround can then be removed).
- **Merge** (own sub-phase, can defer): "B → A" — repoint `media_metadata` and
  `albums`, collapse colliding albums on `(A, norm_title)`, move covers if the
  target lacks one, delete B. Audit-logged.

## Known test/inter-dependency gotchas

- Each new migration bumps the "latest migration" assertion in
  `database/database_test.go` and the schema-snapshot tests
  (per the migration-gotchas note) — update them.
- New `Repository` methods (or signature changes to existing ones) break the api
  `fakeRepo` in `api/handlers_test.go` — add the stubs.
- Image-table rebuild in migration `012` must preserve `updated_at`/`object_key`;
  back it with a test that a pre-existing cover survives the backfill onto its new
  ID.

## Rollout

Phases are independently shippable: 1 (dormant FKs) → 2 (new uploads resolved) →
3 (queries read IDs) → 4 (covers on IDs) → 5 (rename/merge). Each lands with its
own tests; no big-bang cutover.
