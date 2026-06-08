-- Artist & album entities (overlay): stable surrogate IDs that the raw tag text
-- on media_metadata resolves to, so albums/artists can be renamed and (later)
-- merged without orphaning covers or rewriting file tags.
-- Design: docs/architecture/artist-album-model.md
-- Plan:   docs/plans/artist-album-normalization.md (Phase 1)
--
-- This migration is structure only. The entities are populated by the Go-side
-- resolver (normalizeKey/resolveAlbumArtist) via a startup backfill pass and the
-- import path; SQLite cannot do the normalization cleanly. The artist_id/album_id
-- columns are dormant after this migration — nothing reads them until Phase 3.
--
-- The image-table re-key (artist_images/album_images → entity-id keys) is
-- deferred to Phase 4, where it lands in lockstep with the cover-accessor
-- rewrite (see the plan's Phase 1 decision note).

CREATE TABLE artists (
  id         INTEGER PRIMARY KEY,
  name       TEXT    NOT NULL,         -- canonical display name (first spelling wins)
  norm_name  TEXT    NOT NULL UNIQUE,  -- dedup key: normalizeKey(name)
  created_at INTEGER NOT NULL
);

CREATE TABLE albums (
  id         INTEGER PRIMARY KEY,
  artist_id  INTEGER NOT NULL REFERENCES artists(id) ON DELETE CASCADE,
  title      TEXT    NOT NULL,         -- canonical display title (first spelling wins)
  norm_title TEXT    NOT NULL,         -- dedup key within the artist: normalizeKey(title)
  year       INTEGER,                  -- representative year, set by the first track that has one
  created_at INTEGER NOT NULL,
  UNIQUE (artist_id, norm_title)
);

-- Overlay FK columns on the existing metadata. The text columns (artist,
-- album_artist, album) are left untouched — they are the file's actual tags.
ALTER TABLE media_metadata ADD COLUMN artist_id INTEGER REFERENCES artists(id);
ALTER TABLE media_metadata ADD COLUMN album_id  INTEGER REFERENCES albums(id);

CREATE INDEX idx_meta_artist_id ON media_metadata(artist_id);
CREATE INDEX idx_meta_album_id  ON media_metadata(album_id);
