-- PENDING / PARKED — NOT APPLIED.
--
-- This file is intentionally NOT under database/migrations/*.sql, so the embed
-- glob (//go:embed migrations/*.sql) and the migration runner never pick it up.
-- It is a draft kept under version control, not an active migration.
--
-- It adds storage-awareness to the cover-image entities, needed ONLY if we adopt
-- option (B) "persist linked original" for sidecar covers (see the image-original
-- persistence decision in docs/architecture/variants.md and the Cover images row
-- in docs/architecture/data-sources.md). The current decision is (A)
-- read-once-derive: covers decode once into owned/local variants and no linked
-- original is kept, so these columns are unnecessary and were left out of
-- migration 021.
--
-- To ACTIVATE: move this file to database/migrations/NNN_image_storage_columns.sql
-- with NNN = the next free migration number at that time, and flip the relevant
-- variants.md / data-sources.md sections from (A) to (B).

-- Which storage the linked ORIGINAL of a cover came from ('local' | 'links');
-- resized variants always stay owned/local regardless. link_target is the abs
-- path of the external original, NULL for a local-origin image.
ALTER TABLE album_images  ADD COLUMN storage_backend TEXT NOT NULL DEFAULT 'local';
ALTER TABLE album_images  ADD COLUMN link_target      TEXT;
ALTER TABLE artist_images ADD COLUMN storage_backend TEXT NOT NULL DEFAULT 'local';
ALTER TABLE artist_images ADD COLUMN link_target      TEXT;
