-- Covers over the madnetwork, M1 (docs/plans/covers-federation.md).
--
-- A catalog entry now advertises its ALBUM cover: the full sha256 of the
-- cover's source original (fetchable as a mesh blob, self-verifying) and the
-- original's extension. Cached foreign catalogs store both so the download
-- path can attach a remote album's cover without re-asking the origin.
-- Empty on rows from nodes that predate the field — additive, no backfill.
ALTER TABLE federation_catalog ADD COLUMN cover_hash TEXT NOT NULL DEFAULT '';
ALTER TABLE federation_catalog ADD COLUMN cover_ext  TEXT NOT NULL DEFAULT '';
