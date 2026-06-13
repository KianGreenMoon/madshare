-- Required, non-empty display names: artists.name, albums.title and
-- media_metadata.title may no longer be NULL or empty. Missing values fall back
-- to canonical defaults: "Unknown artist" / "Other" / the upload filename with
-- its extension stripped.
-- Design: docs/architecture/artist-album-model.md (Required name defaults)
--
-- Two enforcement mechanisms, chosen per table because of foreign keys:
--   * media_metadata has NO inbound FKs, so it is rebuilt in place to add
--     `title TEXT NOT NULL CHECK(title <> '')` and backfill the column.
--   * artists/albums ARE referenced by other tables' FKs. The SQLite
--     table-rebuild needed to add a column CHECK requires PRAGMA
--     foreign_keys=OFF, which is a no-op inside a transaction (and the migration
--     runner wraps every migration in one). They are already NOT NULL, so we add
--     BEFORE INSERT/UPDATE triggers that reject '' instead — the same guarantee
--     without a rebuild.
--
-- The norm_name/norm_title dedup keys of the "unknown" buckets are left at '' by
-- this migration; folding them onto the real keys ("unknown artist" / "other")
-- and merging any pre-existing literal collisions is done by the Go startup pass
-- db.FoldUnknownBuckets (it reuses the tested MergeArtists/MergeAlbums logic,
-- which is impractical to reimplement in SQL).

-- 1. Relabel the unknown-bucket display columns (keys folded later, in Go).
UPDATE artists SET name  = 'Unknown artist' WHERE name  = '';
UPDATE albums  SET title = 'Other'          WHERE title = '';

-- 2. Rebuild media_metadata with a required, non-empty title. The new table is
--    identical to migrations 001 + 013 except `title` gains NOT NULL + CHECK.
--    Safe with foreign_keys on: media_metadata has no inbound references, so the
--    implicit row-delete on DROP violates nothing.
CREATE TABLE media_metadata_new (
  file_id          INTEGER PRIMARY KEY REFERENCES files(id) ON DELETE CASCADE,
  title            TEXT    NOT NULL CHECK(title <> ''),
  artist           TEXT,
  album            TEXT,
  album_artist     TEXT,
  genre            TEXT,
  year             INTEGER,
  track_number     INTEGER,
  track_total      INTEGER,
  disc_number      INTEGER,
  composer         TEXT,
  comment          TEXT,
  duration_seconds REAL,
  bitrate          INTEGER,
  sample_rate      INTEGER,
  channels         INTEGER,
  codec            TEXT,
  tag_format       TEXT,
  extracted_at     INTEGER NOT NULL,
  artist_id        INTEGER REFERENCES artists(id),
  album_id         INTEGER REFERENCES albums(id)
);

-- Backfill title: a real title tag wins; else the first upload filename with a
-- known audio extension stripped (the set api.allowedExtensions accepts); else
-- "Untitled" (a row with no upload record at all).
INSERT INTO media_metadata_new (
  file_id, title, artist, album, album_artist, genre, year,
  track_number, track_total, disc_number, composer, comment,
  duration_seconds, bitrate, sample_rate, channels, codec,
  tag_format, extracted_at, artist_id, album_id
)
SELECT
  m.file_id,
  COALESCE(
    NULLIF(TRIM(m.title), ''),
    NULLIF(TRIM(
      CASE
        WHEN lower(fn.filename) LIKE '%.mp3'  THEN substr(fn.filename, 1, length(fn.filename) - 4)
        WHEN lower(fn.filename) LIKE '%.ogg'  THEN substr(fn.filename, 1, length(fn.filename) - 4)
        WHEN lower(fn.filename) LIKE '%.wav'  THEN substr(fn.filename, 1, length(fn.filename) - 4)
        WHEN lower(fn.filename) LIKE '%.mp4'  THEN substr(fn.filename, 1, length(fn.filename) - 4)
        WHEN lower(fn.filename) LIKE '%.m4a'  THEN substr(fn.filename, 1, length(fn.filename) - 4)
        WHEN lower(fn.filename) LIKE '%.aac'  THEN substr(fn.filename, 1, length(fn.filename) - 4)
        WHEN lower(fn.filename) LIKE '%.opus' THEN substr(fn.filename, 1, length(fn.filename) - 5)
        WHEN lower(fn.filename) LIKE '%.flac' THEN substr(fn.filename, 1, length(fn.filename) - 5)
        ELSE fn.filename
      END
    ), ''),
    'Untitled'
  ),
  m.artist, m.album, m.album_artist, m.genre, m.year,
  m.track_number, m.track_total, m.disc_number, m.composer, m.comment,
  m.duration_seconds, m.bitrate, m.sample_rate, m.channels, m.codec,
  m.tag_format, m.extracted_at, m.artist_id, m.album_id
FROM media_metadata m
LEFT JOIN (
  SELECT file_id, MIN(filename) AS filename
  FROM file_uploads
  GROUP BY file_id
) fn ON fn.file_id = m.file_id;

DROP TABLE media_metadata;
ALTER TABLE media_metadata_new RENAME TO media_metadata;

-- Recreate the indexes dropped with the old table (migrations 001 + 013).
CREATE INDEX idx_meta_artist    ON media_metadata(artist);
CREATE INDEX idx_meta_album     ON media_metadata(album);
CREATE INDEX idx_meta_title     ON media_metadata(title);
CREATE INDEX idx_meta_artist_id ON media_metadata(artist_id);
CREATE INDEX idx_meta_album_id  ON media_metadata(album_id);

-- 3. Reject empty artists.name / albums.title on future writes (the column-CHECK
--    equivalent we cannot add via rebuild). Created after the relabel above so
--    they do not fire on the rows just fixed.
CREATE TRIGGER artists_name_nonempty_ins BEFORE INSERT ON artists
  WHEN NEW.name = ''
  BEGIN SELECT RAISE(ABORT, 'artists.name must not be empty'); END;
CREATE TRIGGER artists_name_nonempty_upd BEFORE UPDATE OF name ON artists
  WHEN NEW.name = ''
  BEGIN SELECT RAISE(ABORT, 'artists.name must not be empty'); END;
CREATE TRIGGER albums_title_nonempty_ins BEFORE INSERT ON albums
  WHEN NEW.title = ''
  BEGIN SELECT RAISE(ABORT, 'albums.title must not be empty'); END;
CREATE TRIGGER albums_title_nonempty_upd BEFORE UPDATE OF title ON albums
  WHEN NEW.title = ''
  BEGIN SELECT RAISE(ABORT, 'albums.title must not be empty'); END;
