-- Federation F8 item 3 — quality upgrades found in the synced catalogs
-- (docs/architecture/federation.md §Quality upgrades, "The upgrade scan").
--
-- The scan compares every local recording the network can be tied to against the
-- renditions other nodes advertise for it, and records the ones that would beat
-- what we hold. It runs beside checkClaims on every catalog sync, for the same
-- reason that one does: a peer's catalog stands still while OUR library moves,
-- so every upload and every materialized download is new material.
--
-- Findings are STORED rather than recomputed per view, and the disposition
-- column is why. A finding is a comparison made at a moment; dismissing one has
-- to survive the next scan, or an admin would be asked the same question every
-- fifteen minutes forever. This is the rule federation_claim_reports already
-- follows: detection writes measurements, never dispositions.
--
-- Nothing here is authoritative about audio. Every tech column is the ORIGIN'S
-- CLAIM about bytes we have not seen; they become facts only when the bytes
-- arrive and the analysis pipeline re-derives them locally. The column comments
-- say "claimed" for exactly that reason.

CREATE TABLE library_upgrades (
    id           INTEGER PRIMARY KEY,
    -- The local recording that could be upgraded. CASCADE: a recording that is
    -- gone has no upgrade.
    recording_id INTEGER NOT NULL REFERENCES recordings(id) ON DELETE CASCADE,
    -- The remote blob. Not a foreign key to anything — we do not hold it, which
    -- is the entire point of the row.
    remote_hash  TEXT    NOT NULL,
    -- A source currently advertising it, for the "who has it" column. SET NULL
    -- rather than CASCADE: losing track of who offered an upgrade does not make
    -- the upgrade untrue, and the swarm finds holders by hash anyway.
    source_id    INTEGER REFERENCES federation_catalog_sources(id) ON DELETE SET NULL,
    entry_key    TEXT    NOT NULL DEFAULT '',   -- their catalog entry, for provenance

    -- How the recording was tied to their entry, and the measurement when that
    -- was a fingerprint compare. Evidence is stored with the finding so the page
    -- never has to ask the reader to take the match on trust.
    match        TEXT    NOT NULL,              -- 'hash' | 'fingerprint'
    ber          REAL    NOT NULL DEFAULT 0,

    -- The rendition of ours it beats, and its claimed tech facts. our_file_id
    -- SET NULL so a pruned blob leaves the finding readable.
    our_file_id  INTEGER REFERENCES files(id) ON DELETE SET NULL,
    codec        TEXT    NOT NULL DEFAULT '',   -- claimed
    bitrate      INTEGER NOT NULL DEFAULT 0,    -- claimed
    sample_rate  INTEGER NOT NULL DEFAULT 0,    -- claimed
    bit_depth    INTEGER NOT NULL DEFAULT 0,    -- claimed
    byte_size    INTEGER NOT NULL DEFAULT 0,    -- claimed

    -- new: awaiting a decision. dismissed: an admin said no, and a rescan must
    -- not ask again. materialized: we fetched it (the row is kept so the page can
    -- show what was done, and swept once the bytes land and stop being an upgrade).
    disposition  TEXT    NOT NULL DEFAULT 'new'
                 CHECK (disposition IN ('new', 'dismissed', 'materialized')),
    first_seen   INTEGER NOT NULL DEFAULT 0,
    last_seen    INTEGER NOT NULL DEFAULT 0,

    -- One finding per (recording, remote blob) however many nodes advertise it:
    -- the upgrade is a fact about the bytes, not about who is offering them.
    UNIQUE (recording_id, remote_hash)
);

CREATE INDEX idx_upgrades_open ON library_upgrades(disposition, last_seen DESC);
CREATE INDEX idx_upgrades_hash ON library_upgrades(remote_hash);

-- The incremental bound. Stage 1 of the join (shared hash) is cheap enough to
-- re-run whole every sync; stage 2 (fingerprint compare) is not, so it runs only
-- over material newer than this watermark on either side — cached rows whose
-- first_seen is newer, and local recordings fingerprinted since. The first scan
-- of a source pays the full pass once, and only once.
ALTER TABLE federation_catalog_sources ADD COLUMN upgrade_scanned_at INTEGER NOT NULL DEFAULT 0;
