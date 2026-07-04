-- Recording tagsets P0 (docs/architecture/recording-tagsets.md).
--
-- A tagset is one metadata *appearance* of a recording (hardlink model): the
-- descriptive tags move off media_metadata into per-appearance rows owned by
-- the recording, the review/trash lifecycle moves off files onto the tagset
-- (the reviewable, trashable catalog unit), and license/guest access moves onto
-- the recording (one audio identity, one license — decision 9). media_metadata
-- keeps only the blob-owned tech columns. Every file now belongs to a recording
-- (singleton recordings are seeded for unresolved files; "NULL = implicit
-- recording" dies here).
--
-- The backfill maps each file 1:1 onto a tagset read from its old row
-- (origin_file_id = the file), so post-migration behavior is identical: a
-- trashed file becomes a trashed tagset with the file row kept live, preserving
-- today's restore semantics exactly.

-- ── 1. recordings gain access/license (must run before the files columns are
--       dropped). Conflicting per-file values collapse to the best rendition's,
--       chosen by a SQL approximation of the quality ladder (RankRenditions):
--       preferred file first, then lossless > lossy > unknown codec, then
--       sample-rate / bit-depth / bitrate / size. best_file_id is scaffolding,
--       dropped at the end of this step.
ALTER TABLE recordings ADD COLUMN license TEXT;
ALTER TABLE recordings ADD COLUMN guest_playable INTEGER NOT NULL DEFAULT 0;
ALTER TABLE recordings ADD COLUMN guest_playable_manual INTEGER NOT NULL DEFAULT 0;

-- Recordings that lost every file (e.g. an interrupted split) are invalid;
-- clear them before they could pick up singleton seeds or tagsets.
DELETE FROM recordings WHERE NOT EXISTS (
  SELECT 1 FROM files f WHERE f.recording_id = recordings.id);

-- ── 2. Singleton recordings for every file still unresolved (no fingerprint /
--       fpcalc absent / trashed at the old backfill). seed_file_id is
--       scaffolding for the map-back, dropped below.
ALTER TABLE recordings ADD COLUMN seed_file_id INTEGER;
CREATE INDEX idx_recordings_seed ON recordings(seed_file_id);

INSERT INTO recordings (created_at, seed_file_id)
SELECT f.created_at, f.id FROM files f WHERE f.recording_id IS NULL;

UPDATE files SET recording_id = (
  SELECT r.id FROM recordings r WHERE r.seed_file_id = files.id)
WHERE recording_id IS NULL;

DROP INDEX idx_recordings_seed;
ALTER TABLE recordings DROP COLUMN seed_file_id;

-- ── 3. Pick each recording's best rendition (the ladder proxy) and collapse
--       the per-file license/guest values onto the recording.
ALTER TABLE recordings ADD COLUMN best_file_id INTEGER;

UPDATE recordings SET best_file_id = (
  SELECT f.id
  FROM files f
  LEFT JOIN media_metadata mm ON mm.file_id = f.id
  WHERE f.recording_id = recordings.id
  ORDER BY
    (f.id = COALESCE(recordings.preferred_file_id, -1)) DESC,
    CASE
      WHEN lower(COALESCE(mm.codec, '')) IN ('flac', 'alac') THEN 0
      WHEN lower(COALESCE(mm.codec, '')) IN ('mp3', 'aac', 'vorbis', 'opus', 'wmav2', 'ac3', 'mp2') THEN 1
      ELSE 2
    END ASC,
    COALESCE(mm.sample_rate, 0) DESC,
    COALESCE(mm.bit_depth, 0) DESC,
    COALESCE(mm.bitrate, 0) DESC,
    f.byte_size DESC,
    f.id ASC
  LIMIT 1);

UPDATE recordings SET
  license               = (SELECT license FROM files WHERE id = recordings.best_file_id),
  guest_playable        = COALESCE((SELECT guest_playable        FROM files WHERE id = recordings.best_file_id), 0),
  guest_playable_manual = COALESCE((SELECT guest_playable_manual FROM files WHERE id = recordings.best_file_id), 0);

-- ── 4. The tagsets table. Note on identity: the design's appearance key
--       (recording_id, album_id, album_artist_id, disc_number, track_number) is
--       deliberately NOT a UNIQUE index — the review flow requires a draft
--       appearance to coexist with an identical approved one until a moderator
--       denies it, SQLite UNIQUE treats the legitimately-NULL disc/track as
--       distinct anyway, and the backfill would abort on two same-tag renditions
--       of one recording. Identity dedup is enforced by the attach/absorb
--       operations (IS-NOT-DISTINCT-style check in their transaction); the
--       plain index below is their fast path.
CREATE TABLE tagsets (
  id               INTEGER PRIMARY KEY,
  recording_id     INTEGER NOT NULL REFERENCES recordings(id) ON DELETE CASCADE,

  -- Raw tag text (overlay — never silently mutated), exactly the descriptive
  -- columns moved off media_metadata:
  title            TEXT    NOT NULL CHECK (title <> ''),
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

  -- Resolved overlay FKs (the same resolver media_metadata used:
  -- effectiveArtist / effectiveTrackArtist / album identity,
  -- docs/architecture/artist-album-model.md):
  artist_id        INTEGER REFERENCES artists(id),   -- performer
  album_artist_id  INTEGER REFERENCES artists(id),   -- album-grouping artist
  album_id         INTEGER REFERENCES albums(id),

  -- Catalog lifecycle (moved from files — the tagset is the reviewable,
  -- trashable unit):
  review_state     TEXT    NOT NULL DEFAULT 'draft'
                     CHECK (review_state IN ('draft','submitted','returned','approved')),
  review_note      TEXT,
  submitted_at     INTEGER,
  created_by       INTEGER REFERENCES users(id),  -- who offered this appearance
  deleted_at       INTEGER,                       -- tagset-level Trash

  -- Provenance: the file this appearance's tags were read from. Kept for
  -- audit / federation attribution; SET NULL when that blob is purged. A
  -- future origin_node column (federation) sits beside it.
  origin_file_id   INTEGER REFERENCES files(id) ON DELETE SET NULL,

  is_primary       INTEGER NOT NULL DEFAULT 0,  -- the recording's default appearance
  created_at       INTEGER NOT NULL
);

-- Appearance-identity fast path (recording_id is its prefix, so no separate
-- recording index is needed).
CREATE INDEX idx_tagsets_identity ON tagsets(recording_id, album_id, album_artist_id, disc_number, track_number);
CREATE INDEX idx_tagsets_album_id        ON tagsets(album_id);
CREATE INDEX idx_tagsets_artist_id       ON tagsets(artist_id);
CREATE INDEX idx_tagsets_album_artist_id ON tagsets(album_artist_id);
CREATE INDEX idx_tagsets_origin          ON tagsets(origin_file_id);
CREATE INDEX idx_tagsets_deleted         ON tagsets(deleted_at);
-- The pending set is the rare case; listings filter on = 'approved' which the
-- partial index serves by exclusion (same shape as the old idx_files_review).
CREATE INDEX idx_tagsets_review ON tagsets(review_state)
  WHERE review_state <> 'approved';

-- ── 5. Backfill: one tagset per file from its media_metadata row + the files
--       review/trash columns. media_metadata.title is required non-empty since
--       migration 016, so the filename fallback below only fires for the edge
--       case of a file with no metadata row at all (same CASE as 016).
INSERT INTO tagsets (
  recording_id, title, artist, album_artist, album, genre, year,
  track_number, track_total, disc_number, composer, comment,
  artist_id, album_artist_id, album_id,
  review_state, review_note, submitted_at, created_by, deleted_at,
  origin_file_id, is_primary, created_at
)
SELECT
  f.recording_id,
  COALESCE(
    NULLIF(TRIM(COALESCE(m.title, '')), ''),
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
  m.artist, m.album_artist, m.album, m.genre, m.year,
  m.track_number, m.track_total, m.disc_number, m.composer, m.comment,
  m.artist_id, m.album_artist_id, m.album_id,
  f.review_state, f.review_note, f.submitted_at, f.uploaded_by, f.deleted_at,
  f.id, 0, f.created_at
FROM files f
LEFT JOIN media_metadata m ON m.file_id = f.id
LEFT JOIN (
  SELECT file_id, MIN(filename) AS filename
  FROM file_uploads
  GROUP BY file_id
) fn ON fn.file_id = f.id;

-- The recording's primary (default) appearance is its best rendition's tagset —
-- the same file whose license values the recording adopted above.
UPDATE tagsets SET is_primary = 1 WHERE id IN (
  SELECT t.id FROM tagsets t
  JOIN recordings r ON r.id = t.recording_id AND r.best_file_id = t.origin_file_id);

ALTER TABLE recordings DROP COLUMN best_file_id;

-- ── 6. files: the user-facing Trash moved to the tagset (copied above), so the
--       file rows go live again; files.deleted_at now means *rendition removal*
--       only (nothing sets it until that feature lands). Then drop the moved
--       lifecycle/access columns.
UPDATE files SET deleted_at = NULL WHERE deleted_at IS NOT NULL;

DROP INDEX idx_files_review;
ALTER TABLE files DROP COLUMN review_state;
ALTER TABLE files DROP COLUMN review_note;
ALTER TABLE files DROP COLUMN submitted_at;
ALTER TABLE files DROP COLUMN guest_playable;
ALTER TABLE files DROP COLUMN guest_playable_manual;
ALTER TABLE files DROP COLUMN license;

-- ── 7. media_metadata shrinks to the blob-owned tech columns (filled by
--       ffprobe) + tag_format/extracted_at bookkeeping. Rebuilt in place — it
--       has no inbound FKs (the 016 precedent). The old descriptive-text
--       indexes die with the table and are not recreated: search matches via
--       unicode_lower() LIKE (never index-served) and the library browses by
--       the entity FKs, which live on tagsets now.
CREATE TABLE media_metadata_new (
  file_id          INTEGER PRIMARY KEY REFERENCES files(id) ON DELETE CASCADE,
  duration_seconds REAL,
  bitrate          INTEGER,
  sample_rate      INTEGER,
  channels         INTEGER,
  codec            TEXT,
  bit_depth        INTEGER,
  tag_format       TEXT,
  extracted_at     INTEGER NOT NULL
);

INSERT INTO media_metadata_new (
  file_id, duration_seconds, bitrate, sample_rate, channels, codec,
  bit_depth, tag_format, extracted_at
)
SELECT file_id, duration_seconds, bitrate, sample_rate, channels, codec,
       bit_depth, tag_format, extracted_at
FROM media_metadata;

DROP TABLE media_metadata;
ALTER TABLE media_metadata_new RENAME TO media_metadata;

-- ── 8. Invariant: every file belongs to a recording. files cannot be rebuilt
--       with a column-level NOT NULL (inbound FKs + in-transaction PRAGMA
--       limits — the 016 trigger precedent), so triggers reject the NULL on
--       future writes; the backfill above fixed every existing row.
CREATE TRIGGER files_recording_required_ins BEFORE INSERT ON files
  WHEN NEW.recording_id IS NULL
  BEGIN SELECT RAISE(ABORT, 'files.recording_id is required'); END;
CREATE TRIGGER files_recording_required_upd BEFORE UPDATE OF recording_id ON files
  WHEN NEW.recording_id IS NULL
  BEGIN SELECT RAISE(ABORT, 'files.recording_id is required'); END;
