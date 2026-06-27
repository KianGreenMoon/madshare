-- Per-source file attribution (docs/architecture/data-sources.md, P7).
--
-- v0 deliberately tracked no per-file attribution: with a shared `links` dir,
-- sources carried only a scan summary. Removing a source therefore could not
-- tell which records it owned exclusively. This table records which files each
-- source references so Remove can drop a record only when NO source (and no
-- local upload) still relies on it, and so Refresh can re-affirm attribution.
--
-- Many-to-many on purpose: the same content hash found under two source roots is
-- attributed to BOTH, so removing one source leaves the record, owned by the
-- other. ON DELETE CASCADE on both sides keeps it self-cleaning — a hard-deleted
-- file or a removed source takes its attribution rows with it (foreign_keys is
-- ON per-connection; see database/database.go).
CREATE TABLE source_files (
    source_id TEXT    NOT NULL REFERENCES data_sources(id) ON DELETE CASCADE,
    file_id   INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    PRIMARY KEY (source_id, file_id)
);

-- The PRIMARY KEY indexes (source_id, file_id) for a source's file lookup; this
-- secondary index serves the reverse "does any OTHER source reference this file?"
-- probe (the exclusive-set NOT EXISTS) and the file-side cascade.
CREATE INDEX idx_source_files_file ON source_files(file_id);
