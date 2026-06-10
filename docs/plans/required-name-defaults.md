# Required name defaults: no NULL / no empty `artists.name`, `albums.title`, `media_metadata.title`

## Goal

Three display columns must never be NULL **or** empty going forward, with a
canonical default supplied when the source data has nothing:

| Column                  | Today                                  | Default when missing                         |
| ----------------------- | -------------------------------------- | -------------------------------------------- |
| `artists.name`          | `NOT NULL`, but `''` for the bucket    | `Unknown artist`                             |
| `albums.title`          | `NOT NULL`, but `''` for the bucket    | `Other`                                      |
| `media_metadata.title`  | **nullable** (NULL when no title tag)  | file name with its extension stripped        |

This moves the fallback labels out of the (inconsistent) display layer
(`artist || 'Unknown artist'`, `album || 'Untitled album'`, `title || filename`
scattered through the JS) into the data, with one canonical value each.

## Decisions (agreed up front)

1. **Fold the defaults into the dedup keys, not just the display columns.**
   The "unknown" buckets are deduped by `norm_name` / `norm_title`, currently
   `''`. We give them real keys: `normalizeKey("Unknown artist") = "unknown
   artist"` and `normalizeKey("Other") = "other"`. Consequence (accepted): a
   track tagged literally "Unknown Artist" / "Other" now **merges into the
   bucket** instead of forming a separate entity, and the migration must resolve
   the rare case where such a literally-named entity already exists (handled as a
   merge — see §3).

2. **Enforce non-empty at the DB level, not only NOT NULL.** SQLite *can* reject
   `''` — the earlier "just not null" assumption was wrong. How we add the
   enforcement differs per table because of a foreign-key constraint (see §2):
   - `media_metadata.title` → rebuilt with `TEXT NOT NULL CHECK(title <> '')`.
   - `artists.name` / `albums.title` → already `NOT NULL`; add `BEFORE
     INSERT/UPDATE` **triggers** that `RAISE(ABORT)` on `''`. (Triggers because
     these tables can't be rebuilt in-transaction — §2. Same guarantee as a
     column CHECK: the DB rejects an empty write.)

3. **`media_metadata.title` is stored at write time**, per the request: on upload
   (and in the one-time backfill) we write the filename-derived title when the
   file carries no title tag. On a PATCH that clears the title we re-derive from
   the filename so the column is never emptied. Trade-off accepted: the value is
   frozen — a later re-tag/extraction won't auto-update it.

`media_metadata.artist`, `.album`, `.album_artist` are **out of scope** — those
are the file's raw tags (deliberately left untouched by the entity overlay,
migration 013); they stay nullable. Only the three columns above change.

### Invariant: album identity is per-artist, never title-only

An album is identified by the composite key `(artist_id, norm_title)` —
`albums.UNIQUE(artist_id, norm_title)` (migration 013) plus the resolver's
`INSERT ... ON CONFLICT(artist_id, norm_title)`. Two **different** artists each
having an album of the same title (`Other`, `Greatest Hits`, …) are two distinct
album rows and must never unite. This applies to the `Other` bucket too: every
artist gets its **own** `Other` album (distinct `artist_id`), each with its own
cover slot (`album_images.album_id`); only the single `Unknown artist` entity
(globally unique `norm_name`) has the one `Other` album. Nothing in this change
keys an album by bare title: §3a relabels only the display `title`; the §4 fold
and `MergeAlbums` operate strictly within one `artist_id`. Albums collapse only
when they share *both* artist and normalized title — i.e. an explicit same-artist
merge.

## 2. Why triggers for `artists`/`albums` instead of a column CHECK

Adding a column-level `CHECK` to an existing column requires SQLite's
table-rebuild procedure (`CREATE new → copy → DROP old → RENAME`). The official
procedure mandates `PRAGMA foreign_keys=OFF` around it, because dropping a table
that is referenced by other tables' foreign keys would otherwise fail or cascade.

- `artists` is referenced by `albums.artist_id` (ON DELETE CASCADE),
  `media_metadata.artist_id` (RESTRICT) and `artist_images.artist_id`.
- `albums` is referenced by `media_metadata.album_id` (RESTRICT) and
  `album_images.album_id`.

`PRAGMA foreign_keys` is a **no-op inside a transaction**, and our migration
runner (`database/migrate.go` `applyMigration`) wraps every migration in one
`tx`. So we cannot safely rebuild these two tables. Dropping `artists` with FKs
on would hit the `media_metadata.artist_id` RESTRICT and fail.

`media_metadata` has **no inbound FKs** (nothing references it; `file_id` is its
own PK pointing *at* `files`), so rebuilding *it* in-transaction with FKs on is
safe — the implicit row-delete on `DROP` violates nothing.

Triggers add the same "reject empty" guarantee to `artists`/`albums` with a plain
`CREATE TRIGGER`, no rebuild, no FK toggle. (If we later decide we want a literal
column CHECK on those two, that's a separate change to the migration runner to
support a "no outer transaction / foreign_keys off" migration — noted, not done
here.)

## 3. Migration `016_required_name_defaults.sql`

Runs in the runner's single transaction with FKs on. Steps, in order:

### 3a. Backfill the entity display columns (cheap, in SQL)

```sql
UPDATE artists SET name  = 'Unknown artist' WHERE name  = '';
UPDATE albums  SET title = 'Other'          WHERE title = '';
```

`norm_name` / `norm_title` are left at `''` here — the *key* fold + collision
merge is done in Go afterward (§4), where we can reuse the tested merge logic.
(Doing the display label now keeps the new triggers from tripping on the very
rows we are about to write.)

### 3b. Rebuild `media_metadata` with `title NOT NULL CHECK(title <> '')`

Standard rebuild (safe — no inbound FKs). The new table is identical to the
current schema (migration 001 columns + the `artist_id`/`album_id` columns from
013) except `title` becomes `TEXT NOT NULL CHECK(title <> '')`.

```sql
CREATE TABLE media_metadata_new (
  file_id  INTEGER PRIMARY KEY REFERENCES files(id) ON DELETE CASCADE,
  title    TEXT NOT NULL CHECK(title <> ''),
  artist   TEXT, album TEXT, album_artist TEXT, genre TEXT, year INTEGER,
  track_number INTEGER, track_total INTEGER, disc_number INTEGER,
  composer TEXT, comment TEXT, duration_seconds REAL, bitrate INTEGER,
  sample_rate INTEGER, channels INTEGER, codec TEXT, tag_format TEXT,
  extracted_at INTEGER NOT NULL,
  artist_id INTEGER REFERENCES artists(id),
  album_id  INTEGER REFERENCES albums(id)
);

INSERT INTO media_metadata_new
SELECT
  m.file_id,
  COALESCE(
    NULLIF(TRIM(m.title), ''),          -- a real title tag wins
    NULLIF(<stripext(first filename)>, ''),
    'Untitled'                          -- last-resort: no tag, no upload row
  ),
  m.artist, m.album, m.album_artist, m.genre, m.year,
  m.track_number, m.track_total, m.disc_number, m.composer, m.comment,
  m.duration_seconds, m.bitrate, m.sample_rate, m.channels, m.codec,
  m.tag_format, m.extracted_at, m.artist_id, m.album_id
FROM media_metadata m
LEFT JOIN (SELECT file_id, MIN(filename) AS filename
           FROM file_uploads GROUP BY file_id) fu ON fu.file_id = m.file_id;

DROP TABLE media_metadata;
ALTER TABLE media_metadata_new RENAME TO media_metadata;

-- recreate the five indexes dropped with the old table
CREATE INDEX idx_meta_artist    ON media_metadata(artist);
CREATE INDEX idx_meta_album     ON media_metadata(album);
CREATE INDEX idx_meta_title     ON media_metadata(title);
CREATE INDEX idx_meta_artist_id ON media_metadata(artist_id);
CREATE INDEX idx_meta_album_id  ON media_metadata(album_id);
```

`<stripext(first filename)>` strips a trailing **known audio extension**
(`.mp3 .ogg .flac .wav .mp4 .m4a .aac .opus` — the set the uploader accepts,
`api.allowedExtensions`) via a case-insensitive `CASE`/`substr`, leaving the name
intact otherwise. (SQLite has no `reverse()`; a generic last-dot strip would need
a recursive CTE. The known-extension `CASE` is exact for our accepted formats and
matches `filepath.Ext` on real audio filenames; the rare divergence on a name
with a non-audio dot, e.g. `Vol.2`, is acceptable and arguably safer.)

### 3c. Empty-string guard triggers on `artists` / `albums`

```sql
CREATE TRIGGER artists_name_nonempty_ins BEFORE INSERT ON artists
  WHEN NEW.name = '' BEGIN SELECT RAISE(ABORT,'artists.name must not be empty'); END;
CREATE TRIGGER artists_name_nonempty_upd BEFORE UPDATE OF name ON artists
  WHEN NEW.name = '' BEGIN SELECT RAISE(ABORT,'artists.name must not be empty'); END;
CREATE TRIGGER albums_title_nonempty_ins BEFORE INSERT ON albums
  WHEN NEW.title = '' BEGIN SELECT RAISE(ABORT,'albums.title must not be empty'); END;
CREATE TRIGGER albums_title_nonempty_upd BEFORE UPDATE OF title ON albums
  WHEN NEW.title = '' BEGIN SELECT RAISE(ABORT,'albums.title must not be empty'); END;
```

Created **after** 3a so they don't fire on the rows we just relabeled. They fire
only on the `name`/`title` columns, so the Go norm-key fold (which updates
`norm_name`/`norm_title`) and `MergeArtists`/`MergeAlbums` are unaffected.

## 4. Go reconcile pass: fold the unknown buckets onto their new keys

New method `db.FoldUnknownBuckets(ctx)` in `database/entities.go`, run at startup
**after** `BackfillEntities` and `BackfillCoverEntities` (so entities + covers
exist before any merge), before `startListeners` — slotted into `madshare.go`
next to the other backfills. Idempotent (after folding, no `''` key remains, so a
re-run is a no-op).

```
artists: find the bucket A where norm_name = '' (display already 'Unknown artist')
  target = 'unknown artist'
  if some R != A already has norm_name = 'unknown artist':
        MergeArtists(from=R, into=A)        -- collapse the literal onto the bucket,
                                            -- reusing tested cover/album-collision logic
  UPDATE artists SET norm_name='unknown artist' WHERE id = A

albums: for each bucket B where norm_title = '' (display already 'Other'):
  target = 'other' within B.artist_id
  if some R != B under the same artist has norm_title = 'other':
        MergeAlbums(from=R, into=B)
  UPDATE albums SET norm_title='other' WHERE id = B
```

Merging the *literal* entity **into the bucket** keeps the canonical display
(`Unknown artist` / `Other`) on the survivor. No collision → a plain `UPDATE`
(the common case). This is the only place that needs `MergeArtists`/`MergeAlbums`
reuse; everything else is the relabel UPDATE.

## 5. Resolver changes (`database/entities.go`)

Introduce two shared constants and apply them so **new** uploads land on the
folded buckets (not `''`):

```go
const DefaultArtistName = "Unknown artist"
const DefaultAlbumTitle = "Other"
```

- `effectiveArtist(albumArtist, artist)` → return `DefaultArtistName` instead of
  `""` when both tags normalize empty. Then `resolveArtistTx` stores
  `name='Unknown artist'`, `norm_name='unknown artist'`.
- `resolveAlbumArtistTx` album branch → when `normalizeKey(t.Album) == ""`,
  resolve against `DefaultAlbumTitle` (`title='Other'`, `norm_title='other'`)
  instead of `''`.
- `RenameArtist`/`RenameAlbum`: the `if newNorm == "" { return ErrNameConflict }`
  guard stays valid (can't rename to empty). Update the now-stale comments that
  call `''` "the unknown bucket" — the bucket key is now `unknown artist`/`other`,
  and renaming *to* those collides with the bucket → `ErrNameConflict` (a merge),
  which is the desired behavior. No logic change.
- `BackfillEntities` needs no change (it routes through the resolver, which now
  applies the defaults); it only touches rows with NULL FKs, already a near no-op.

## 6. `media_metadata.title` at write time

- **Model** (`database/models.go`): `MediaMetadata.Title` `sql.NullString` →
  `string` (it's now non-null). Ripples to `getMetadataByFileID` scan and
  `InsertFile` insert (drop the NullString handling for title only).
- **Upload path**: add a Go helper `titleFromFilename(name)` =
  `strings.TrimSuffix(name, filepath.Ext(name))`. In `api/upload_handlers.go`
  (or in `tagsToMetadata`, threading the sanitized `filename`), set
  `meta.Title = tag-title if non-empty else titleFromFilename(filename)`.
  `tagsToMetadata` currently `nullString(t.Title)` — change so title is never
  empty. (`tagsToMetadata` gains a `filename string` parameter.)
- **PATCH** (`database/metadata.go` `UpdateFileMetadata`): when `p.Title != nil`
  and the trimmed value is empty, re-derive from the file's first filename
  instead of storing NULL (look it up in the same tx). `metaNullString` stays for
  the other (still-nullable) fields. Title can no longer be cleared to NULL.

## 7. Read-query simplifications (now that the columns are guaranteed non-empty)

- `database/library.go`
  - `listTracksByAlbumArtist` and `Search` tracks: `COALESCE(NULLIF(m.title,''),
    fu.filename,'')` → just `m.title` (in SELECT and ORDER BY). The `fu` join for
    title fallback can go where it was only used for that.
  - `searchAlbums`: drop the `AND al.title != ''` filter — it existed to hide the
    empty bucket; the bucket is now a real `Other` album. (`Other`/`Unknown
    artist` will appear in search when the query matches; accepted under the
    fold-into-keys decision.)
  - Empty-**parameter** semantics are unchanged: `listAlbumsByArtist`'s
    `(? = '' OR ar.norm_name = ?)` and `listTracksByAlbumArtist`'s
    `if artist == "" { return nil }` key off the *request argument*, not the
    stored value. Note one behavioral gain: the unknown-artist bucket is now
    drillable, because the UI passes its real name `Unknown artist`
    (→ `norm 'unknown artist'`) rather than `''`.
- `database/files.go` `listFiles`/`ListTrashedFiles`: `COALESCE(m.title,'')` is
  now redundant (title non-null) but harmless — leave or simplify. The flat file
  list will now show filename-derived titles for previously-untitled files
  (an improvement, consistent with the rest).

## 8. Frontend (minimal)

The JS fallbacks (`title || 'Unknown'`, `album || 'Untitled album'`,
`artist || 'Unknown artist'`, `t.title || baseName(...)`) become mostly dead
because the data now carries canonical values. Leaving them is harmless
defense-in-depth; not changing them keeps this change backend-focused. (One real
inconsistency to note: the JS used `Untitled album`, we standardize on `Other`.)
No required frontend change; optional cleanup deferred.

## 9. Test impact (per the migration/repo gotchas)

- `database/database_test.go`: migration version `15 → 16` (two assertions); the
  idempotent-migrations row count `15 → 16`. **Table-name set is unchanged** (the
  rebuild renames `media_metadata_new` back to `media_metadata`; triggers aren't
  tables), so that assertion stands.
- `database/entities_test.go`: `TestResolveAlbumArtist_EmptyBuckets` and the
  untagged-bucket assertions flip from `name=''`/`norm=''` to
  `Unknown artist`/`unknown artist` and `Other`/`other`. The "titled album
  distinct from the unknown bucket" case still holds. Add a case for the
  literal-"Unknown Artist"-merges-into-bucket behavior and for `FoldUnknownBuckets`
  collision handling.
- `database/files_test.go` / `search_test.go`: any seed that inserts a NULL/empty
  title or relies on the empty bucket needs updating; `InsertFile` now writes a
  filename-derived title.
- `api` `fakeRepo`/handler tests: `FileListEntry.Title` is already a plain
  `string` (no change); the new `FoldUnknownBuckets` method on the repo interface
  (if added to `database.Repository`) will need a `fakeRepo` stub — or keep
  `FoldUnknownBuckets` a `*DB` method not on the interface (it's startup-only,
  like the other backfills, which are **not** on `Repository`). Prefer the latter.
- New: a migration test that an empty `INSERT INTO artists(name,...)` /
  `UPDATE ... SET name=''` is rejected by the triggers, and that
  `media_metadata.title` rejects `''`/NULL.

## 10. Order of work

1. Migration `016` (3a relabel → 3b rebuild → 3c triggers).
2. `database/entities.go`: constants + resolver defaults + `FoldUnknownBuckets`;
   wire into `madshare.go` after the existing backfills.
3. `database/models.go` + `database/files.go` + `database/metadata.go`: `Title`
   to `string`; PATCH re-derive.
4. `api`: `titleFromFilename`, thread filename into `tagsToMetadata`.
5. Read-query simplifications (`library.go`).
6. Update/extend tests; `go test ./...` + `go build ./...`.

## Risks / notes

- The fold pass uses `MergeArtists`/`MergeAlbums`, which mutate covers — they run
  *after* `BackfillCoverEntities`, so covers are already on the entity tables.
- Collisions (a literal `Unknown artist`/`Other` already present) are rare but
  handled; without the merge they'd be a UNIQUE violation on the relabel UPDATE.
- The migration rebuild rewrites the whole `media_metadata` table once; fine at
  current scale, O(rows). It runs inside the migration transaction, so a failure
  rolls back cleanly.
