-- Data sources & storages (docs/architecture/data-sources.md, P1).
--
-- Separates WHERE bytes physically live (a "storage") from WHERE library
-- content was imported from (a "source"). v0 ships two storages — `local`
-- (owned blobs, today) and `links` (shared dir of symlinks to externals) — and
-- one source kind, `symlink`. This migration adds only schema; nothing yet
-- populates `links` or `data_sources` (that is P3), so there is no behaviour
-- change. Storage precedence is the fixed constant local > links, so no
-- storage_order / storage_default settings exist.

-- A logical origin that populates the `links` storage. Many symlink sources
-- share the one links dir; per-file attribution is intentionally not tracked in
-- v0 (sources carry their scan-time summary; link health is reported over the
-- whole links storage). 's3' joins the kind CHECK when that storage lands.
CREATE TABLE data_sources (
    id           TEXT PRIMARY KEY,
    kind         TEXT NOT NULL CHECK (kind IN ('symlink')),
    name         TEXT NOT NULL,
    root_path    TEXT NOT NULL,                               -- the external dir referenced
    status       TEXT NOT NULL DEFAULT 'active'
                 CHECK (status IN ('active','scanning','error')),
    summary_json TEXT,                                        -- last scan: linked/skipped/failed counts
    created_at   INTEGER NOT NULL,
    scanned_at   INTEGER
);

-- files.storage_backend already exists (migration 001, default 'local'); it now
-- becomes an ORIGIN HINT ('local' | 'links'), NOT authoritative for serving —
-- the resolver decides by probing storages. Add the original-path pointer for
-- link rows (abs path of the external original; NULL for local).
ALTER TABLE files ADD COLUMN link_target TEXT;

-- No cover-image storage columns: covers are read-once-derived into owned/local
-- variants (decision in docs/architecture/variants.md), so every
-- album_images/artist_images row is implicitly local — there is no linked
-- original to track. The storage-aware image columns (storage_backend /
-- link_target on album_images + artist_images) are parked, unapplied, in
-- database/migrations/pending/ in case the "persist linked original" option (B)
-- is ever adopted.
