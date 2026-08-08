-- Cover-variant byte index (docs/architecture/storage.md).
--
-- The storage panel sizes five categories. Four of them — audio, review, trash,
-- cache — are indexed DB sums that answer instantly and stay fresh. Images alone
-- was a full DirSize walk of the variants tree, run inline on every dashboard
-- load: eight variants per cover times every album, so a 50k-album library paid
-- ~400k stat() calls to draw one bar. The hybrid design accepted that on the
-- rationale that "the image set is small (few files)", which is exactly the
-- assumption that stops holding at the scale the other four were built for.
--
-- This table makes images the fifth indexed sum.
--
-- The DIRECTORY REMAINS AUTHORITATIVE and this index merely describes it — the
-- same rule madnetwork_cache (migration 040) follows, and for the same reason:
-- the variant files are written by the imageproc pool and removed by the orphan
-- sweep, neither of which should have to be transactional with a byte counter to
-- stay correct. A stale row is therefore a reconciliation problem, never a
-- phantom: ReconcileImageVariants re-walks the tree at startup, which is the one
-- place the expensive walk is still paid, once per process instead of once per
-- page load.
--
-- Keyed by image_hash, not by album. album_images and artist_images are keyed by
-- entity id and several of them can share one image_hash (identical embedded art
-- collapses to a single variant directory and a single job — see the partial
-- unique index idx_imgproc_active). A byte column on those tables would
-- therefore double-count every shared cover on every sum. One row per directory
-- is the only shape that adds up.

CREATE TABLE image_variants (
    -- Full sha256 of the source original, lowercase hex. Keys the variants
    -- directory <variants_dir>/images/<image_hash>/ whose bytes this row totals,
    -- and matches album_images.image_hash / artist_images.image_hash /
    -- image_processing_jobs.image_hash (all renamed from base_key in 022).
    image_hash TEXT PRIMARY KEY,

    -- Total bytes of the DERIVED variants in that directory. Not the source
    -- original, which lives in the separate tree under <files_dir>/images and is
    -- not what the panel's "images" category has ever measured.
    bytes      INTEGER NOT NULL DEFAULT 0,

    updated_at INTEGER NOT NULL
);
