-- Recording overlay (docs/architecture/recordings.md, P1).
--
-- A recording groups files that are the same audio (by acoustic fingerprint);
-- each member file is a rendition (a specific encoding). The overlay never
-- mutates a file's bytes, hash, or tags — it only adds a grouping FK, exactly
-- like the artist/album overlay. Most recordings have a single rendition.

CREATE TABLE recordings (
    id                INTEGER PRIMARY KEY,
    created_at        INTEGER NOT NULL,
    -- Nullable manual override of the auto-ranked "best" rendition (the quality
    -- ladder's escape hatch for lossy-sourced upscales). Unsurfaced in v0;
    -- NULL = use the ladder. ON DELETE SET NULL so pruning the chosen file
    -- falls back to the ladder rather than dangling.
    preferred_file_id INTEGER REFERENCES files(id) ON DELETE SET NULL
);

-- Recording identity is content-derived, so the FK lives on files (next to
-- hash), not on media_metadata (which carries tag-derived ids). NULL = not yet
-- resolved / no fingerprint = the file is its own implicit recording.
ALTER TABLE files ADD COLUMN recording_id INTEGER REFERENCES recordings(id);

-- A file a moderator manually split/pinned into its own recording: the resolver
-- must never re-merge it on a later pass (a tag edit alone would not re-group
-- it — the pin is what makes "this is actually the live version" stick).
ALTER TABLE files ADD COLUMN recording_pinned INTEGER NOT NULL DEFAULT 0;

CREATE INDEX idx_files_recording ON files(recording_id);
