# Artist & album entities

**Status:** implemented (entities P1–P5; grouping model reaffirmed). The
**track-performer split** (`media_metadata.album_artist_id` + a new
`media_metadata.artist_id`, with unified album-artist/performer browse) is
designed here and built in migration `018`. Assumes the roles-only access model
(`docs/architecture/auth.md`) — artist/album strings carry **no** access-control
meaning, so this is a pure library/cover/grouping change.

## Problem

Today there is no artist or album entity. An artist is the SQL expression
`COALESCE(NULLIF(album_artist,''), NULLIF(artist,''), '')` and an album is that
expression plus `album_title`, recomputed by `GROUP BY` on every query
(`database/library.go`). Cover tables are keyed by the **strings** themselves
(`artist_images.artist_name`, `album_images(album_artist, album_title)`).

Consequence: identity is the text. Renaming an album rewrites the text on every
track and silently orphans the string-keyed cover row; merging two spellings of
one album is impossible; nothing can be attached to a stable "this album".

A second consequence, addressed by the track-performer split below: the only
artist a track was attached to was its *album* artist. A performer who appears
only on a compilation (`album_artist = "Various Artists"`, each track a different
`artist`) had no entity at all — unbrowsable and unsearchable.

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
```

The entity is an **editable overlay** on top of the raw tags: rename/merge edits
the entity row; the file's stored tags are never silently mutated.

### Two artist roles, one entity table

`artists` is a **single** entity table. A given artist row is referenced in two
roles, both FKs from `media_metadata` into `artists`:

```sql
-- migration 013 added a single artist_id (the album artist). Migration 018
-- splits the two roles on media_metadata. The rename is a real ALTER on the
-- table (the physical column is renamed in the DB), not a query-time alias —
-- the schema itself names each role:
ALTER TABLE media_metadata RENAME COLUMN artist_id TO album_artist_id;  -- album-grouping artist
ALTER TABLE media_metadata ADD  COLUMN artist_id INTEGER REFERENCES artists(id);  -- track performer

-- index follows the rename for clarity; a fresh index covers the new column:
DROP   INDEX IF EXISTS idx_meta_artist_id;
CREATE INDEX idx_meta_album_artist_id ON media_metadata(album_artist_id);
CREATE INDEX idx_meta_artist_id       ON media_metadata(artist_id);
-- (album_id + idx_meta_album_id are unchanged.)
```

| Column | Role | Resolved from |
|--------|------|---------------|
| `media_metadata.album_artist_id` | the album-grouping artist (drives browse-by-artist + the album's `albums.artist_id`) | `effectiveArtist` = `album_artist` → `artist` → Unknown |
| `media_metadata.artist_id` *(new)* | the track's **performer** | `effectiveTrackArtist` = `artist` → `album_artist` → Unknown |
| `albums.artist_id` | the album's (album-)artist | same as the track's `album_artist_id` |

The same `artists` row serves both roles whenever the performer name equals the
album-artist name — i.e. **the normal case**: a single-artist release has
`artist == album_artist` for every track, so `artist_id == album_artist_id` and
no extra entity is created. Only a compilation (differing performers under one
`album_artist`) materializes distinct performer entities.

**Why `albums` keeps `artist_id` but `media_metadata` gets `album_artist_id`:**
`media_metadata` now carries *two* artist columns, so the bare name `artist_id`
would be ambiguous (performer? album artist?). `albums` carries exactly one — an
album has one artist by definition — so `albums.artist_id` is already
unambiguous and `albums.album_artist_id` would just be redundant.

### Identity rules (the actual hard part)

These must be applied **identically** at import time and in the backfill, in Go
(SQL can't do the normalization cleanly):

1. **Effective album-artist of a track** = first non-empty of `album_artist`,
   then `artist`, else the configured "unknown artist" placeholder. (Mirrors
   today's `COALESCE(NULLIF(album_artist,''), NULLIF(artist,''))`.) This is the
   *album-level* artist used for browse-by-artist grouping → `album_artist_id`
   (and the album's `albums.artist_id`). Helper: `effectiveArtist(albumArtist, artist)`.
2. **Effective performer of a track** = first non-empty of `artist`, then
   `album_artist`, else the unknown-artist placeholder → `artist_id`. Note the
   **reversed precedence**: the track's own `artist` tag wins; it falls back to
   the album artist when the track has no performer tag, then to the Unknown
   bucket. The column is therefore **never NULL**, and for a normal release it
   equals `album_artist_id`. Helper: `effectiveTrackArtist(artist, albumArtist)`.
3. **`norm_name` / `norm_title`** = a deterministic normalization of the display
   string: Unicode NFC → trim → collapse internal whitespace → casefold
   (lowercase). **No** "the "-stripping or fuzzy matching in v0 — keep it
   predictable; aggressive folding causes wrong merges. Documented as the single
   `normalizeKey()` helper so import and backfill cannot drift. Both artist roles
   resolve through the same `resolveArtistTx` / `artists` dedup, so identical
   names share one entity row regardless of role.
4. **Album identity** = `(artist_id, norm_title)`, where `artist_id` is the
   album-artist (rule 1). An empty album title resolves to the "unknown album"
   placeholder under that artist (so untagged tracks group sensibly rather than
   each becoming its own album). The placeholder strings (`Unknown artist` /
   `Other`, plus filename-derived track titles) are **required and non-empty** at
   both the DB level and in the resolver — see
   [Required name defaults](#required-name-defaults).
5. **Representative `year`** = the album row's year is set from the first track
   that supplies one; not recomputed on every insert (avoids churn). A later
   admin edit can override it.

### Compilations / "Various Artists"

An album whose tracks have differing performers but a shared `album_artist`
(e.g. literally `Various Artists`) resolves its **album-artist** entity to the
single "Various Artists" artist via rule 1 (the `album_artist` text wins), so the
album still groups as one album under one album-artist. Each track *additionally*
resolves a **performer** entity from its own `artist` tag (rule 2) →
`media_metadata.artist_id`. Those performers are now first-class `artists` rows:
browsable, searchable, cover-bearing, and mergeable like any artist. The raw
`artist` text still survives on `media_metadata` (overlay).

A track has **exactly one** performer entity, derived from the single `artist`
tag. Splitting `"A feat. B"` into multiple credits is **not** done in v0 (see
[Non-goals](#non-goals-v0)).

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

Because identity is per-artist (rule 4), **every** artist gets its own `Other`
album — two different artists' `Other` albums never unite; only the single
`Unknown artist` entity has the one shared `Other`. Both artist roles map empty
tags onto the same `Unknown artist` entity (rules 1 & 2 share the placeholder).

### Where the grouping decision lives (settled 2026-06-10)

The album-artist grouping choice is made **server-side**, in the entity resolver
(`effectiveArtist`, `database/entities.go`) — *not* in the clients. Two
alternatives were considered and rejected:

- **A separate `album_artist` table** — unnecessary. "Album artist" is just *the
  artist an album belongs to*, already modeled by `albums.artist_id`. Adding a
  table would duplicate a relationship the FK already expresses.
- **Moving the grouping into each UI** ("if album_artist present, the client uses
  it as the artist") — rejected. The artist is a stable backend **entity** (id,
  cover, dedup, rename/merge). Deriving the grouping ad-hoc per client would lose
  the stable entity + cover and force the library view, admin, and
  future federation to re-implement and stay in sync on the same rule.

Browsing by a *performing/featured* artist who is never an `album_artist` is
**now provided** by the track-performer split (`artist_id`) and the unified
browse below — it is no longer a non-goal.

### Browse by album-artist, search across both roles

The **library is grouped by album-artist**: a performer who only ever appears on
a compilation (its `album_artist` owned by someone else) does **not** clutter the
library artist list. But such a performer is still findable in search and
browsable by its id, so a search hit is never a dead end:

- **Library artist list** (`ListArtists`): one row per artist that is the
  **album-artist** of ≥1 visible (and, when guest, reachable) track
  (`m.album_artist_id = a.id`). Pure performers are excluded here — the album the
  performer's track sits on is grouped under *its* album-artist (e.g. "Various
  Artists"), not under the performer.
- **Artist search** (search-artists): matches **both** roles
  (`m.album_artist_id = a.id OR m.artist_id = a.id`), so searching a performer
  name returns the performer as a clickable artist entity even when it owns no
  album. `track_count` counts the matching rows (a row whose two FKs point at the
  same artist still counts once).
- **Albums of an artist** (`ListAlbumsByArtistID`): the **union** of (a) albums
  the artist owns as album-artist (`albums.artist_id = a.id`) and (b) albums
  containing a track they perform (`m.artist_id = a.id`). Done in one pass via the
  join condition `al.artist_id = ? OR m.artist_id = ?`, which yields a useful
  hybrid `track_count`: an owned album counts all its tracks, while a comp the
  artist only performs on counts just their tracks on it. So a performer reached
  from search drills into the comps they appear on (not a dead end), and an
  album-artist additionally surfaces any comp they are individually featured on.
- **Tracks of an album** (`ListTracksByAlbumID`): the whole album's track list,
  regardless of which artist the user entered from, ordered
  `COALESCE(disc_number, 1), track_number, title` so a multi-disc album groups by
  disc instead of interleaving (the row carries `disc_number` for the UI's
  "Disc N" grouping). The track row shows the **performer** name
  (`media_metadata.artist_id → artists.name`), matching the playlists page (which
  shows each track's own `artist`).

The guest-filtered variants apply the same `OR` over the access clause. The
unknown-bucket sorting is unchanged.

### Cover re-keying

Both image tables key on entity IDs:

```sql
artist_images(artist_id PK, object_key, mime_type, updated_at, …)
album_images (album_id  PK, object_key, mime_type, updated_at, …)
```

Covers attach to a stable ID. `artist_images` is keyed by the artist entity id, so
a performer entity can carry a cover exactly like an album artist; performer
covers are reachable through the existing id-addressed artist-cover read endpoint
with no change. The album-artist cover shown next to a *file* (the admin file
list, review queue) joins `artist_images` on `media_metadata.album_artist_id` (the
album artist), which is the cover a track is filed under.

## Operations the model enables

- **Rename** — update `artists.name` or `albums.title` (and the `norm_*` key).
  Covers, year, and all tracks via the FKs follow with no string rewriting. The
  file's raw tags are intentionally left as-is (overlay). A rename of a shared
  entity updates both its album-artist and performer appearances at once (it is
  one row).
- **Merge** — "B is the same as A". See the merge semantics below; an artist
  merge now repoints **both** artist roles, an album merge repoints neither
  track's performer.
- **Stable cover/metadata attachment** — the original ask ("attach the cover to
  an album id") is the default.

### Merge semantics with two roles

- **`MergeArtists(from → into)`** repoints every reference to the source artist in
  *both* roles: `albums.artist_id`, `media_metadata.album_artist_id`, **and**
  `media_metadata.artist_id` (a merged-away artist may be an album artist on one
  release and a performer on another). Colliding albums collapse on
  `(into, norm_title)`; the source cover moves only if the target lacks one; the
  source row is deleted. The merge preview's `tracks_moved` counts rows matching
  the source in either role.
- **`MergeAlbums(from → into)`** repoints the moved tracks' `album_id` and
  `album_artist_id` (to the target album's artist) — but **leaves `artist_id`
  (the performer) untouched**. Moving a track to a different album never changes
  who performed it.
- **`FoldUnknownBuckets`** folds literals onto the canonical Unknown bucket in
  *both* `album_artist_id` and `artist_id`.

## Query shape after normalization

`ListArtists` / `ListAlbumsByArtistID` / `ListTracksByAlbumID` / `search` are
straight JOINs on `artists`/`albums` instead of `GROUP BY` over COALESCE
expressions. The library artist listing joins the **album-artist** role only
(search swaps in the both-roles `OR` from the section above) — e.g.:

```sql
SELECT a.id, a.name, COUNT(*) AS track_count,
       (ai.artist_id IS NOT NULL) AS has_image
FROM artists a
JOIN media_metadata m ON m.album_artist_id = a.id   -- search: (= a.id OR m.artist_id = a.id)
JOIN files f ON f.id = m.file_id AND f.deleted_at IS NULL
LEFT JOIN artist_images ai ON ai.artist_id = a.id
GROUP BY a.id
ORDER BY a.norm_name = ? ASC, LOWER(a.name);  -- ? = normalizeKey(DefaultArtistName)
```

Browse endpoints address entities by `?artist_id=` / `?album_id=`.

### Unknown buckets sort last

The browse listings pin the two placeholder buckets to the **bottom** of their
lists, after the named entities: `ListArtists` sends the "Unknown artist" bucket
last, and `ListAlbumsByArtistID` sends "Other" last (before that, named albums
sort by year then title). Implemented as a leading `ORDER BY` key — a boolean
comparing the row's dedup key to the bucket's reserved key
(`norm_name = normalizeKey(DefaultArtistName)` / `norm_title =
normalizeKey(DefaultAlbumTitle)`), passed as a bound parameter so the SQL carries
no magic strings.

The bucket is identified by its **reserved normalized key**, not by an empty
string or by matching the display text. Per the identity rules above, an untagged
track and one literally tagged `Other`/`Unknown artist` resolve to the *same*
entity, and `FoldUnknownBuckets` merges any pre-existing literal into it at
startup. So keying off the reserved key is exact.

## Backfill strategy

The migration renames `artist_id → album_artist_id` (existing data preserved
in place — every pre-existing row keeps its album-artist id) and adds the new,
NULL `artist_id`. Entity *population* of the new column is a **startup reconcile
pass** (same pattern as the existing orphan-blob pass and the original
`BackfillEntities`): idempotent, processes only `media_metadata` rows with NULL
`artist_id` (or NULL `album_artist_id`/`album_id`), resolves the performer via the
shared `effectiveTrackArtist` resolver, and sets the FK. New uploads resolve all
three FKs inline during import; metadata edits re-resolve them when an
identity-affecting tag changes.

## Browse by entity id

The browse + cover-read endpoints address entities by id:

| Endpoint | Shape |
|----------|-------|
| albums of an artist | `GET /api/albums?artist_id=<id>` |
| tracks of an album | `GET /api/tracks?album_id=<id>` |
| album cover (read) | `GET /api/albums/{album_id}/image` |
| album cover status | `GET /api/albums/{album_id}/image/status` |
| artist cover (read) | `GET /api/artists/{artist_id}/image` |

`artist_id` / `album_id` are **required** and must parse to a positive integer
(`400` otherwise). A valid-but-unknown id yields an empty list / `404` image /
empty status body. The unknown-bucket sorting and guest-filtering are unchanged.

### Covers: read by id, write by name

The cover **write** path stays name-addressed —
`POST /api/albums/{name}/image?artist=<name>` and
`POST /api/artists/{name}/image` — because it *resolve-or-creates* the entity
(`ResolveAlbumID`/`ResolveArtistID`) when a cover is attached at upload time,
before the entity has a browsable id. `LookupArtistID`/`LookupAlbumID` stay (used
by the write, rename, and name-based merge endpoints).

### Front-end migration

The library drill-down (`app.js`) and the admin By-entity drill-down
(`admin/files.js`) browse by id. The
drill state carries `{id, name}` so the breadcrumb shows the display name while
fetches use the id. **Search** result artist/album items carry an `id` so a hit
can drill in and render its cover by id. The track-list rows show the per-track
**performer** (`artist_name` from the new `artist_id`).

## Merge dry-run

Merge is destructive and immediate. A non-mutating **preview** lets the UI show
exactly what a merge will do before the user commits:

- `POST /api/artists/merge/preview` and `POST /api/albums/merge/preview`, same
  `{from_id, into_id}` body and `metadata.edit` gate as the real merges. A
  separate route (not a `dry_run` flag) keeps any chance of an accidental real
  merge off the table.
- Backed by `MergeArtistsPreview` / `MergeAlbumsPreview` in `database/entities.go`
  — read-only queries that reuse the exact collision/cover rules of
  `MergeArtists`/`MergeAlbums` (so the preview can't drift from the act),
  including the both-roles `tracks_moved` count for artist merges.

The preview payload (`MergePreview` in `models.go`):

| Field | Artist merge | Album merge |
|-------|-------------|-------------|
| `tracks_moved` | tracks repointed off the source (either role) | tracks moved to the target album |
| `albums_moved` | source albums with no title collision | — (0) |
| `albums_collapsed` + `collapsed_titles` | source albums folding into an existing target album | — |
| `source_has_cover` / `target_has_cover` | drives "cover moves" vs "target's cover kept" | same |
| `source_artist_orphaned` | — | source's album-artist left with nothing |

## Non-goals (v0)

- **Multi-credit tracks** — a track resolves to exactly one performer entity from
  its single `artist` tag; parsing `"A feat. B"` / `"A & B"` into multiple
  credits (a track↔artist join table) is deferred and would need its own design.
- A merge preview is **read-only** advisory, not a lock: a concurrent edit
  between preview and commit is not guarded against (single-admin assumption).
- Fuzzy / "the "-stripping normalization, MusicBrainz IDs, external matching.
- Re-deriving access control from entities (Layer B is gone; if per-content
  restriction returns it will key off `album_id`/`artist_id`).
