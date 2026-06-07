# Artist & album entities

**Status:** design (owner-approved 2026-06-07). Implementation plan:
`docs/plans/artist-album-normalization.md`. Assumes the roles-only access model
(`docs/plans/access-roles-only.md`) — artist/album strings carry **no**
access-control meaning, so this is a pure library/cover/grouping change.

## Problem

Today there is no artist or album entity. An artist is the SQL expression
`COALESCE(NULLIF(album_artist,''), NULLIF(artist,''), '')` and an album is that
expression plus `album_title`, recomputed by `GROUP BY` on every query
(`database/library.go`). Cover tables are keyed by the **strings** themselves
(`artist_images.artist_name`, `album_images(album_artist, album_title)`).

Consequence: identity is the text. Renaming an album rewrites the text on every
track and silently orphans the string-keyed cover row; merging two spellings of
one album is impossible; nothing can be attached to a stable "this album".

## Model: overlay, not full normalization

The raw tag text **stays** on `media_metadata` (`artist`, `album_artist`,
`album`, …) — it is the file's actual metadata and must survive re-imports. We
**add** surrogate entities and FK columns that resolve to them:

```sql
CREATE TABLE artists (
  id         INTEGER PRIMARY KEY,
  name       TEXT    NOT NULL,         -- canonical display name
  norm_name  TEXT    NOT NULL UNIQUE,  -- dedup key (see normalization)
  created_at INTEGER NOT NULL
);

CREATE TABLE albums (
  id         INTEGER PRIMARY KEY,
  artist_id  INTEGER NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
  title      TEXT    NOT NULL,         -- canonical display title
  norm_title TEXT    NOT NULL,         -- dedup key within the artist
  year       INTEGER,                  -- representative year (nullable)
  created_at INTEGER NOT NULL,
  UNIQUE (artist_id, norm_title)
);

-- Overlay FKs on the existing metadata (text columns untouched).
ALTER TABLE media_metadata ADD COLUMN artist_id INTEGER REFERENCES artists(id);
ALTER TABLE media_metadata ADD COLUMN album_id  INTEGER REFERENCES albums(id);
CREATE INDEX idx_meta_artist_id ON media_metadata(artist_id);
CREATE INDEX idx_meta_album_id  ON media_metadata(album_id);
```

The entity is an **editable overlay** on top of the raw tags: rename/merge edits
the entity row; the file's stored tags are never silently mutated.

### Identity rules (the actual hard part)

These must be applied **identically** at import time and in the backfill, in Go
(SQL can't do the normalization cleanly):

1. **Effective artist of a track** = first non-empty of `album_artist`, then
   `artist`, else the configured "unknown artist" placeholder. (Mirrors today's
   `COALESCE(NULLIF(album_artist,''), NULLIF(artist,''))`.) This is the
   *album-level* artist used for browse-by-artist grouping.
2. **`norm_name` / `norm_title`** = a deterministic normalization of the display
   string: Unicode NFC → trim → collapse internal whitespace → casefold
   (lowercase). **No** "the "-stripping or fuzzy matching in v0 — keep it
   predictable; aggressive folding causes wrong merges. Documented as the single
   `normalizeKey()` helper so import and backfill cannot drift.
3. **Album identity** = `(artist_id, norm_title)`. An empty album title resolves
   to the "unknown album" placeholder under that artist (so untagged tracks group
   sensibly rather than each becoming its own album). The placeholder strings are
   configurable later (see the deferred "default placeholder names" idea).
4. **`media_metadata.artist_id`** = the effective-artist entity (same one
   `albums.artist_id` points to for that track's album).
5. **Representative `year`** = the album row's year is set from the first track
   that supplies one; not recomputed on every insert (avoids churn). A later
   admin edit can override it.

### Compilations / "Various Artists"

An album whose tracks have differing performers but a shared `album_artist`
(e.g. literally `Various Artists`) resolves to a **single** "Various Artists"
artist entity via rule 1 (the `album_artist` text wins). Each track keeps its own
performer in the `artist` text column. v0 does **not** create a separate
track-level performer entity — `media_metadata.artist_id` is the album-artist
entity. A future `track_artist_id` can be added without breaking this (deferred).

### Cover re-keying

Both image tables move from string keys to entity IDs:

```sql
-- after backfill populates artists/albums:
artist_images(artist_id PK, object_key, mime_type, updated_at)
album_images (album_id  PK, object_key, mime_type, updated_at)
```

Covers now attach to a stable ID. The Phase-5 cover-rename dance (re-POST the
cover to the new string identity) disappears: a rename is a one-row update to
`artists.name` / `albums.title` and the cover follows automatically.

## Operations the model enables

- **Rename** — update `artists.name` or `albums.title` (and the `norm_*` key).
  Covers, year, and all tracks via the FK follow with no string rewriting. The
  file's raw tags are intentionally left as-is (overlay).
- **Merge** — "B is the same as A": `UPDATE media_metadata SET artist_id=A WHERE
  artist_id=B`, repoint/merge B's albums into A (collapsing albums that collide
  on `(A, norm_title)`), move covers if the target lacks one, then delete B.
  Admin-only action; design hook now, build later (own phase).
- **Stable cover/metadata attachment** — the original ask ("attach the cover to
  an album id") is now the default.

## Query shape after normalization

`ListArtists` / `ListAlbumsByArtist` / `ListTracksByAlbumArtist` / `search`
become straight JOINs on `artists`/`albums` instead of `GROUP BY` over COALESCE
expressions — e.g.:

```sql
SELECT a.id, a.name, COUNT(m.file_id) AS track_count,
       (ai.artist_id IS NOT NULL) AS has_image
FROM artists a
JOIN media_metadata m ON m.artist_id = a.id
JOIN files f ON f.id = m.file_id AND f.deleted_at IS NULL
LEFT JOIN artist_images ai ON ai.artist_id = a.id
GROUP BY a.id
ORDER BY LOWER(a.name);
```

Browse endpoints can move to `?artist_id=`/`?album_id=` params. v0 may keep the
name-based routes working by resolving name→id first, to avoid a simultaneous
front-end rewrite (decided in the plan).

## Backfill strategy

The migration adds the tables/columns and re-keys the image tables *structurally*
but does **not** populate entities (the normalization is Go logic). Population is
a **startup reconcile pass** (same pattern as the existing orphan-blob pass in
`madshare.go`): idempotent, processes only `media_metadata` rows with NULL
`artist_id`/`album_id`, resolves entities via the shared resolver, and migrates
existing `artist_images`/`album_images` rows onto the new IDs by re-running the
same name→entity resolution. New uploads resolve entities inline during import.

## Non-goals (v0)

- Track-level performer entities (`track_artist_id`).
- Fuzzy / "the "-stripping normalization, MusicBrainz IDs, external matching.
- Re-deriving access control from entities (Layer B is gone; if per-content
  restriction returns it will key off `album_id`/`artist_id`).
