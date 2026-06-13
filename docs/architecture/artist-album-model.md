# Artist & album entities

**Status:** implemented (entities P1–P5; grouping model reaffirmed). Assumes the
roles-only access model (`docs/architecture/auth.md`) — artist/album strings carry
**no** access-control meaning, so this is a pure library/cover/grouping change.

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
   sensibly rather than each becoming its own album). The placeholder strings
   (`Unknown artist` / `Other`, plus filename-derived track titles) are
   **required and non-empty** at both the DB level and in the resolver — see
   [Required name defaults](#required-name-defaults).
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

### Required name defaults

Three display columns are **never NULL and never empty**, with a canonical default
when the source data has nothing:

| Column | Default when missing |
|--------|----------------------|
| `artists.name` | `Unknown artist` |
| `albums.title` | `Other` |
| `media_metadata.title` | the filename with its audio extension stripped |

The defaults are folded into the dedup keys, not just the display text:
`normalizeKey("Unknown artist")` / `normalizeKey("Other")` are the buckets'
reserved keys (`DefaultArtistName` / `DefaultAlbumTitle` in
`database/entities.go`), so a track literally tagged `Unknown Artist` / `Other`
merges **into** the bucket rather than forming a separate entity (the startup
`FoldUnknownBuckets` pass collapses any pre-existing literal). Enforcement:
`media_metadata.title` is `NOT NULL CHECK(title <> '')` and is written at
upload/edit time (filename-derived when untagged, re-derived if a PATCH clears
it); `artists.name` / `albums.title` keep their `NOT NULL` plus empty-string-guard
triggers. This moves the fallback labels out of the (inconsistent) display layer
into the data, with one canonical value each.

Because identity is per-artist (rule 3), **every** artist gets its own `Other`
album — two different artists' `Other` albums never unite; only the single
`Unknown artist` entity has the one shared `Other`.

### Where the grouping decision lives (settled 2026-06-10)

The album-artist-vs-track-artist choice is made **server-side**, in the entity
resolver (`effectiveArtist`, `database/entities.go`) — *not* in the clients. Two
alternatives were considered and rejected:

- **A separate `album_artist` table** — unnecessary. "Album artist" is just *the
  artist an album belongs to*, already modeled by `albums.artist_id`. The
  per-track performer survives as the raw `media_metadata.artist` tag; nothing is
  lost. Adding a table would duplicate a relationship the FK already expresses.
- **Moving the grouping into each UI** ("if album_artist present, the client uses
  it as the artist") — rejected. The artist is a stable backend **entity** (id,
  cover, dedup, rename/merge). Deriving the grouping ad-hoc per client would lose
  the stable entity + cover and force the library view, `/cmus`, admin, and
  future federation to re-implement and stay in sync on the same rule.

Browsing by a *performing/featured* artist who is never an `album_artist` is the
one capability this model intentionally does not provide; it would need a
track↔artist credits model (the deferred `track_artist_id` non-goal) and its own
plan — independent of the library grouping above.

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
ORDER BY a.norm_name = ? ASC, LOWER(a.name);  -- ? = normalizeKey(DefaultArtistName)
```

Browse endpoints can move to `?artist_id=`/`?album_id=` params. v0 may keep the
name-based routes working by resolving name→id first, to avoid a simultaneous
front-end rewrite (decided in the plan).

### Unknown buckets sort last

The browse listings pin the two placeholder buckets to the **bottom** of their
lists, after the named entities: `ListArtists` sends the "Unknown artist" bucket
last, and `ListAlbumsByArtist` sends "Other" last (before that, named albums sort
by year then title). Implemented as a leading `ORDER BY` key — a boolean
comparing the row's dedup key to the bucket's reserved key
(`norm_name = normalizeKey(DefaultArtistName)` / `norm_title =
normalizeKey(DefaultAlbumTitle)`), passed as a bound parameter so the SQL carries
no magic strings. It is a second key folded into the sort the query already runs
(the listing sorts on `LOWER(name)` regardless), so it adds no scan, join, or
extra pass.

The bucket is identified by its **reserved normalized key**, not by an empty
string or by matching the display text. Per the identity rules above, an untagged
track and one literally tagged `Other`/`Unknown artist` resolve to the *same*
entity (`effectiveAlbum`/`effectiveArtist` map empty tags onto the placeholder,
and `FoldUnknownBuckets` merges any pre-existing literal into it at startup). So
keying off the reserved key is exact: there is no separate "real `Other`" entity
to confuse it with — by design they are one and the same.

## Backfill strategy

The migration adds the tables/columns and re-keys the image tables *structurally*
but does **not** populate entities (the normalization is Go logic). Population is
a **startup reconcile pass** (same pattern as the existing orphan-blob pass in
`madshare.go`): idempotent, processes only `media_metadata` rows with NULL
`artist_id`/`album_id`, resolves entities via the shared resolver, and migrates
existing `artist_images`/`album_images` rows onto the new IDs by re-running the
same name→entity resolution. New uploads resolve entities inline during import.

## Browse by entity id

The read path originally addressed entities by **name** (`/api/albums?artist=`,
`/api/tracks?artist=&album=`, and the cover GETs by name), re-running
`normalizeKey` → entity on every request even though the listing DTOs already
carry the stable `id`. The browse + cover-read endpoints move to id addressing:

| Endpoint | Before | After |
|----------|--------|-------|
| albums of an artist | `GET /api/albums?artist=<name>` | `GET /api/albums?artist_id=<id>` |
| tracks of an album | `GET /api/tracks?artist=<name>&album=<title>` | `GET /api/tracks?album_id=<id>` |
| album cover (read) | `GET /api/albums/{name}/image?artist=<name>` | `GET /api/albums/{album_id}/image` |
| album cover status | `GET /api/albums/{name}/image/status?artist=<name>` | `GET /api/albums/{album_id}/image/status` |
| artist cover (read) | `GET /api/artists/{name}/image` | `GET /api/artists/{artist_id}/image` |

`artist_id` / `album_id` are **required** and must parse to a positive integer
(`400` otherwise); the old empty-`artist` "list every album" mode and the
empty-`artist` "tracks → nil" guard are dropped (no client used them). The
unknown-bucket sorting and guest-filtering are unchanged — only the WHERE key
moves from `norm_name`/`norm_title` to `id`. A valid-but-unknown id yields an
empty list / `404` image / empty status body (same contract as a missing entity
today).

### Covers: read by id, write by name

The cover **write** path stays name-addressed —
`POST /api/albums/{name}/image?artist=<name>` and
`POST /api/artists/{name}/image` — because it *resolve-or-creates* the entity
(`ResolveAlbumID`/`ResolveArtistID`) when a cover is attached at upload time,
before the entity has a browsable id. chi routes a name-param `POST` and an
id-param `GET` on the same path node independently (verified), so the two
coexist without a separate path. `LookupArtistID`/`LookupAlbumID` therefore stay
(used by the write, rename, and name-based merge endpoints); only the
browse/cover-read handlers stop calling them.

### What this removes

The name-keyed browse DB methods (`ListAlbumsByArtist[Guest]`,
`ListTracksByAlbumArtist[Guest]`) and their `Repository` entries are deleted, not
kept as back-compat shims (pre-release, and every caller is migrated in the same
change). New id-keyed siblings replace them:
`ListAlbumsByArtistID[Guest](artistID)`, `ListTracksByAlbumID[Guest](albumID)`.

### Front-end migration

Three browse clients move to ids: the library drill-down (`app.js`), the admin
By-entity drill-down (`admin/files.js`), and the standalone cmus view
(`cmus.js`) — the last only because it shares the now-removed name routes, not
because it gains anything. Each already holds the entity `id` in its rendered
DTOs; the drill state carries `{id, name}` so the breadcrumb still shows the
display name while fetches use the id. **Search** result artist/album items gain
an `id` field (the DB already selects it into `ArtistEntry`/`AlbumEntry`; only
the search JSON omitted it) so a search hit can drill in and render its cover by
id. The cover **edit** component (`cover-edit.js`) and the upload-page cover
attach (`upload.js`) are unchanged — they only POST, which stays by name.

## Merge dry-run

Merge is destructive and immediate. A non-mutating **preview** lets the UI show
exactly what a merge will do before the user commits:

- `POST /api/artists/merge/preview` and `POST /api/albums/merge/preview`, same
  `{from_id, into_id}` body and `metadata.edit` gate as the real merges. A
  separate route (not a `dry_run` flag on the merge endpoint) keeps any chance of
  an accidental real merge off the table.
- Backed by `MergeArtistsPreview` / `MergeAlbumsPreview` in `database/entities.go`
  — read-only queries that reuse the exact collision/cover rules of
  `MergeArtists`/`MergeAlbums` (so the preview can't drift from the act). They
  return `ErrMergeSelf` / `ErrEntityNotFound` identically.

The preview payload (`MergePreview` in `models.go`):

| Field | Artist merge | Album merge |
|-------|-------------|-------------|
| `tracks_moved` | tracks repointed off the source | same |
| `albums_moved` | source albums with no title collision | — (0) |
| `albums_collapsed` + `collapsed_titles` | source albums folding into an existing target album | — |
| `source_has_cover` / `target_has_cover` | drives "cover moves" vs "target's cover kept" | same |
| `source_artist_orphaned` | — | source's artist left with nothing |

The admin merge modal (`admin/files.js`) already posts the id-addressed merge;
its static warning ("Moves all tracks into X, then deletes Y") is replaced by the
live preview numbers, fetched when the merge target changes (falling back to the
static text on a preview error).

## Non-goals (v0)

- Track-level performer entities (`track_artist_id`) — still deferred; needs its
  own track↔artist credits design.
- A merge preview is **read-only** advisory, not a lock: a concurrent edit
  between preview and commit is not guarded against (single-admin assumption).
- Fuzzy / "the "-stripping normalization, MusicBrainz IDs, external matching.
- Re-deriving access control from entities (Layer B is gone; if per-content
  restriction returns it will key off `album_id`/`artist_id`).
