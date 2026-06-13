-- Re-key the cover tables from string identities to the artist/album entity ids
-- (docs/architecture/artist-album-model.md (cover re-keying)). A cover now attaches to
-- a stable id, so a rename is a one-row update to the entity and the cover
-- follows automatically — no string rewrite, no orphaned cover row.
--
-- This migration is structural only. The existing rows are keyed by strings and
-- can only be mapped to entity ids by the Go-side normalizeKey resolver, which
-- runs in a startup backfill (BackfillCoverEntities) — and only after the entity
-- backfill has populated artists/albums. So the old tables are renamed aside
-- (not dropped); the backfill drains them into the new id-keyed tables and then
-- drops the *_old leftovers. New uploads write the id-keyed tables directly.
--
-- Columns preserved from migration 009: object_key, mime_type, updated_at,
-- base_key, source_ext, variants_ready. image_processing_jobs is unaffected
-- (keyed by base_key, not by album identity).

ALTER TABLE album_images  RENAME TO album_images_old;
ALTER TABLE artist_images RENAME TO artist_images_old;

CREATE TABLE album_images (
  album_id       INTEGER PRIMARY KEY REFERENCES albums(id) ON DELETE CASCADE,
  object_key     TEXT    NOT NULL,
  mime_type      TEXT    NOT NULL,
  updated_at     INTEGER NOT NULL,
  base_key       TEXT,
  source_ext     TEXT,
  variants_ready INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE artist_images (
  artist_id      INTEGER PRIMARY KEY REFERENCES artists(id) ON DELETE CASCADE,
  object_key     TEXT    NOT NULL,
  mime_type      TEXT    NOT NULL,
  updated_at     INTEGER NOT NULL,
  base_key       TEXT,
  source_ext     TEXT,
  variants_ready INTEGER NOT NULL DEFAULT 0
);
