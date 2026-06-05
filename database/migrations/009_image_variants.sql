-- Extend album_images / artist_images with variant tracking columns.
-- object_key retains its original meaning (original image path) for backward
-- compatibility with rows written before variants existed.
-- base_key is the 16-char SHA-256 prefix used to derive all variant paths.
-- source_ext is ".jpg" or ".png" — the extension of the original file (WebP not
-- accepted in v0). variants_ready is 0 until the async worker has generated all
-- variants for the row.
ALTER TABLE album_images ADD COLUMN base_key TEXT;
ALTER TABLE album_images ADD COLUMN source_ext TEXT;
ALTER TABLE album_images ADD COLUMN variants_ready INTEGER NOT NULL DEFAULT 0;

-- Artist images reserved for future use (no worker implementation in this plan).
ALTER TABLE artist_images ADD COLUMN base_key TEXT;
ALTER TABLE artist_images ADD COLUMN source_ext TEXT;
ALTER TABLE artist_images ADD COLUMN variants_ready INTEGER NOT NULL DEFAULT 0;

-- Async job queue for image variant generation.
CREATE TABLE image_processing_jobs (
    id          INTEGER PRIMARY KEY,
    cover_type  TEXT    NOT NULL,  -- "album" (artist deferred)
    -- For album: "<album_artist>\x1f<album_title>" (unit separator, not slash,
    -- to avoid ambiguity with names containing a slash).
    subject_key TEXT    NOT NULL,
    base_key    TEXT    NOT NULL,
    status      TEXT    NOT NULL DEFAULT 'pending', -- pending | running | done | failed
    error       TEXT,              -- last error message if status=failed
    retry_count INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL,
    started_at  INTEGER,
    finished_at INTEGER
);

CREATE INDEX idx_imgproc_status ON image_processing_jobs(status, created_at);

-- Enforce enqueue idempotency at the DB level: at most one active (pending or
-- running) job per base_key. EnqueueImageJob relies on this with
-- INSERT ... ON CONFLICT DO NOTHING so concurrent uploads of the same cover
-- cannot double-queue.
CREATE UNIQUE INDEX idx_imgproc_active ON image_processing_jobs(base_key)
    WHERE status IN ('pending', 'running');
