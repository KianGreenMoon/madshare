-- Ingest-time media analysis (docs/architecture/recordings.md, P0).
--
-- Two optional decode passes run asynchronously after upload, mirroring the
-- image_processing_jobs queue (migration 009): ffprobe fills the reserved tech
-- columns on media_metadata; fpcalc computes an acoustic fingerprint. Both
-- tools are optional — when a tool is absent its output is left NULL/missing and
-- the job still completes (graceful degradation, never a hard dependency).

-- bit_depth distinguishes two lossless renditions at the same sample rate (the
-- quality ladder's lossless tiebreak). The other tech columns
-- (duration_seconds / bitrate / sample_rate / channels / codec) already exist on
-- media_metadata, reserved NULL since v0 (migration 001).
ALTER TABLE media_metadata ADD COLUMN bit_depth INTEGER;

-- Acoustic fingerprint, one row per file (recording identity). Kept in its own
-- table: a few KB, cold relative to the hot files row, and federation will share
-- it. fingerprint stores the raw fpcalc sub-fingerprints packed little-endian
-- (uint32 each) so the resolver can Hamming-compare without re-decoding base64.
CREATE TABLE audio_fingerprints (
    file_id      INTEGER PRIMARY KEY REFERENCES files(id) ON DELETE CASCADE,
    algo         TEXT    NOT NULL,         -- e.g. 'chromaprint'
    algo_version TEXT,                     -- fpcalc / chromaprint version string
    duration     REAL,                     -- fingerprinted duration (fpcalc reports it)
    fingerprint  BLOB    NOT NULL,         -- raw sub-fingerprints, little-endian uint32
    created_at   INTEGER NOT NULL
);

-- Async job queue for ingest media analysis (ffprobe + fpcalc), modeled on
-- image_processing_jobs (migration 009).
CREATE TABLE media_analysis_jobs (
    id          INTEGER PRIMARY KEY,
    file_id     INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    status      TEXT    NOT NULL DEFAULT 'pending', -- pending | running | done | failed
    error       TEXT,              -- last error message if status=failed
    retry_count INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL,
    started_at  INTEGER,
    finished_at INTEGER
);

CREATE INDEX idx_media_analysis_status ON media_analysis_jobs(status, created_at);

-- At most one active (pending or running) job per file: enqueue idempotency,
-- same pattern as idx_imgproc_active. EnqueueAnalysisJob relies on this with
-- INSERT ... ON CONFLICT DO NOTHING.
CREATE UNIQUE INDEX idx_media_analysis_active ON media_analysis_jobs(file_id)
    WHERE status IN ('pending', 'running');
