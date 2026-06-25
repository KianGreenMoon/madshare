-- Cover source/derivative split: re-key covers by the FULL image hash.
--
-- The cover pipeline moves to a content-addressed source/derivative split that
-- mirrors the audio model (docs/architecture/variants.md):
--   * the source ORIGINAL lives at <files_dir>/images/<image_hash>/original<ext>
--     (a regenerate seed, never served), and
--   * its derived VARIANTS at <variants_dir>/images/<image_hash>/<recipe><ext>
--     (served at /images),
-- both addressed by the full sha256 of the original bytes — superseding the
-- 16-char base_key.
--
-- This migration is the SCHEMA half: rename the key column base_key -> image_hash
-- on every cover table. The DATA half — recomputing the full hash from the stored
-- bytes and relocating the original out of the variants tree — cannot be done in
-- SQL (it must read the image files), so it runs as an idempotent Go startup pass
-- (db.SplitImageSources) which rewrites the still-16-char values left here to the
-- full hash and re-enqueues variant regeneration. Until that pass runs, an
-- album_images.image_hash may transiently still hold a 16-char value.

ALTER TABLE album_images  RENAME COLUMN base_key TO image_hash;
ALTER TABLE artist_images RENAME COLUMN base_key TO image_hash;

-- image_processing_jobs is keyed by the same value. Drop its partial unique index
-- (one active job per key) before the rename and recreate it on the new column so
-- it never dangles on the old name.
DROP INDEX IF EXISTS idx_imgproc_active;
ALTER TABLE image_processing_jobs RENAME COLUMN base_key TO image_hash;
CREATE UNIQUE INDEX idx_imgproc_active ON image_processing_jobs(image_hash)
    WHERE status IN ('pending', 'running');
