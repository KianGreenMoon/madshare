-- One node, one row (docs/architecture/federation-nodes.md, owner 2026-08-13).
--
-- Four tables described a node: federation_peers (an admin acted),
-- federation_catalog_sources (the sweep pulled), federation_home_nodes (a
-- device enrolled), swarm_peer_traffic (bytes moved). One fact lived in two
-- places — last_seen had two clocks folded by MAX(), heard_name had two
-- landing spots read by three label chains — and the catalog satellites keyed
-- on source ids the frontier rotation recycles.
--
-- This migration folds the first three into federation_nodes: one row per
-- public key, holding the observations every contact updates plus three
-- OPTIONAL column groups, each marked present by its own columns:
--
--   trust      non-NULL trust_state = an admin acted (the old peer row)
--   sync       sync_added_at > 0    = in the pull rotation (the old source row)
--   household  home_added_at > 0    = this listener node signs in there
--
-- swarm_peer_traffic deliberately stays its own table (counters, not
-- knowledge; history outlives standing), as does the gossip store (received
-- signed documents, a log). The three cached satellites re-key onto the
-- public key, which is the only identity that never recycles.
--
-- Peer ids are PRESERVED (step 1 inserts them verbatim) so admin URLs and
-- any in-flight admin page stay valid across the upgrade; id remains an
-- admin-surface handle only and is never federated.

CREATE TABLE federation_nodes (
    id          INTEGER PRIMARY KEY,          -- admin-surface handle only; never federated
    public_key  TEXT NOT NULL UNIQUE,         -- lowercase hex ed25519; THE identity

    -- Observations: any contact may update these. One clock, one name.
    heard_name  TEXT    NOT NULL DEFAULT '',  -- the node's own claim, never matched on
    first_seen  INTEGER NOT NULL DEFAULT 0,
    last_seen   INTEGER NOT NULL DEFAULT 0,
    hinted_at   INTEGER NOT NULL DEFAULT 0,   -- gossiped-freshness receipt (picks the window)

    -- Trust group (was federation_peers).
    trust_state  TEXT CHECK (trust_state IS NULL OR trust_state IN
                 ('pending_outgoing','pending_incoming','friend','blocked')),
    prev_state   TEXT    NOT NULL DEFAULT '',
    label        TEXT    NOT NULL DEFAULT '', -- the admin's local name (was peers.name)
    user_id      INTEGER REFERENCES users(id) ON DELETE SET NULL,
    trusted_at   INTEGER NOT NULL DEFAULT 0,  -- was peers.created_at
    block_reason TEXT    NOT NULL DEFAULT '',
    blocked_at   INTEGER NOT NULL DEFAULT 0,

    -- Sync group (was federation_catalog_sources). sync_added_at > 0 marks the
    -- row as IN THE PULL ROTATION — the fact the old schema encoded as row
    -- existence, which one table must carry explicitly or a pending peer (a
    -- trust row nothing ever pulled from) would join the rotation by accident.
    sync_added_at      INTEGER NOT NULL DEFAULT 0,
    catalog_serial     TEXT    NOT NULL DEFAULT '',
    catalog_synced_at  INTEGER NOT NULL DEFAULT 0,
    attempted_at       INTEGER NOT NULL DEFAULT 0,
    upgrade_scanned_at INTEGER NOT NULL DEFAULT 0,  -- the upgrade scan's watermark (was 039's ALTER)

    -- Household group (was federation_home_nodes).
    home_added_at INTEGER NOT NULL DEFAULT 0,
    home_base_url TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX idx_fednodes_rotation ON federation_nodes(attempted_at);

-- 1. Peers first, ids preserved.
INSERT INTO federation_nodes
    (id, public_key, heard_name, first_seen, last_seen,
     trust_state, prev_state, label, user_id, trusted_at, block_reason, blocked_at)
SELECT p.id, p.public_key, p.heard_name, p.created_at, p.last_seen,
       p.state, p.prev_state, p.name, p.user_id, p.created_at, p.block_reason, p.blocked_at
FROM federation_peers p;

-- 2. Fold each peer's source half in: the sync group, plus the merged
--    observations — the peer's heard_name wins when set (the friendship ping
--    refreshes it every minute; the source's only per pull), last_seen is the
--    later clock, first_seen the earliest non-zero.
UPDATE federation_nodes AS n SET
    heard_name        = CASE WHEN n.heard_name <> '' THEN n.heard_name ELSE s.heard_name END,
    first_seen        = CASE WHEN n.first_seen = 0 THEN s.first_seen
                             WHEN s.first_seen = 0 THEN n.first_seen
                             ELSE MIN(n.first_seen, s.first_seen) END,
    last_seen         = MAX(n.last_seen, s.last_seen),
    hinted_at         = s.hinted_at,
    sync_added_at      = CASE WHEN s.first_seen > 0 THEN s.first_seen
                              WHEN s.attempted_at > 0 THEN s.attempted_at
                              ELSE 1 END,
    catalog_serial     = s.catalog_serial,
    catalog_synced_at  = s.catalog_synced_at,
    attempted_at       = s.attempted_at,
    upgrade_scanned_at = s.upgrade_scanned_at
FROM federation_catalog_sources AS s
WHERE s.public_key = n.public_key;

-- 3. Sources that are not peers (members the frontier reached).
INSERT INTO federation_nodes
    (public_key, heard_name, first_seen, last_seen, hinted_at, sync_added_at,
     catalog_serial, catalog_synced_at, attempted_at, upgrade_scanned_at)
SELECT s.public_key, s.heard_name, s.first_seen, s.last_seen, s.hinted_at,
       CASE WHEN s.first_seen > 0 THEN s.first_seen
            WHEN s.attempted_at > 0 THEN s.attempted_at
            ELSE 1 END,
       s.catalog_serial, s.catalog_synced_at, s.attempted_at, s.upgrade_scanned_at
FROM federation_catalog_sources s
WHERE s.public_key NOT IN (SELECT public_key FROM federation_nodes);

-- 4. Home nodes: fold into existing rows, then the rest. A home server's
--    stored name is a claim like any heard name, so it fills the same column
--    when nothing fresher is known.
UPDATE federation_nodes AS n SET
    home_added_at = h.added_at,
    home_base_url = h.base_url,
    heard_name    = CASE WHEN n.heard_name <> '' THEN n.heard_name ELSE h.name END
FROM federation_home_nodes AS h
WHERE h.public_key = n.public_key;

INSERT INTO federation_nodes (public_key, heard_name, home_added_at, home_base_url)
SELECT h.public_key, h.name, h.added_at, h.base_url
FROM federation_home_nodes h
WHERE h.public_key NOT IN (SELECT public_key FROM federation_nodes);

-- ── Re-key the three cached satellites onto the public key ──────────────────
-- Same columns, same meanings; the root changes from a recyclable source id to
-- the identity itself. ON DELETE CASCADE now hangs off the node row.

CREATE TABLE federation_catalog_new (
    node_key       TEXT    NOT NULL REFERENCES federation_nodes(public_key) ON DELETE CASCADE,
    entry_key      TEXT    NOT NULL,
    recording_key  TEXT    NOT NULL DEFAULT '',
    title          TEXT    NOT NULL,
    artist         TEXT    NOT NULL DEFAULT '',
    album_artist   TEXT    NOT NULL DEFAULT '',
    album          TEXT    NOT NULL DEFAULT '',
    genre          TEXT,
    year           INTEGER,
    track_number   INTEGER,
    disc_number    INTEGER,
    duration       REAL,
    license        TEXT,
    guest_playable INTEGER NOT NULL DEFAULT 0,
    renditions     TEXT    NOT NULL DEFAULT '[]',
    first_seen     INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (node_key, entry_key)
);
INSERT INTO federation_catalog_new
SELECT s.public_key, c.entry_key, c.recording_key, c.title, c.artist,
       c.album_artist, c.album, c.genre, c.year, c.track_number, c.disc_number,
       c.duration, c.license, c.guest_playable, c.renditions, c.first_seen
FROM federation_catalog c
JOIN federation_catalog_sources s ON s.id = c.source_id;
DROP TABLE federation_catalog;
ALTER TABLE federation_catalog_new RENAME TO federation_catalog;
CREATE INDEX idx_fedcat_browse ON federation_catalog(album_artist, album);
CREATE INDEX idx_fedcat_first_seen ON federation_catalog(first_seen);

CREATE TABLE federation_holdings_new (
    node_key TEXT NOT NULL REFERENCES federation_nodes(public_key) ON DELETE CASCADE,
    hash     TEXT NOT NULL,
    PRIMARY KEY (node_key, hash)
);
INSERT INTO federation_holdings_new
SELECT s.public_key, h.hash
FROM federation_holdings h
JOIN federation_catalog_sources s ON s.id = h.source_id;
DROP TABLE federation_holdings;
ALTER TABLE federation_holdings_new RENAME TO federation_holdings;
CREATE INDEX idx_fedhold_hash ON federation_holdings(hash);

CREATE TABLE federation_claim_reports_new (
    id            INTEGER PRIMARY KEY,
    node_key      TEXT    NOT NULL REFERENCES federation_nodes(public_key) ON DELETE CASCADE,
    kind          TEXT    NOT NULL CHECK (kind IN ('held_blob','grouping')),
    hash          TEXT    NOT NULL,
    other_hash    TEXT    NOT NULL DEFAULT '',
    ber           REAL    NOT NULL DEFAULT 0,
    words         INTEGER NOT NULL DEFAULT 0,
    our_head      TEXT    NOT NULL DEFAULT '',
    their_head    TEXT    NOT NULL DEFAULT '',
    our_version   TEXT    NOT NULL DEFAULT '',
    their_version TEXT    NOT NULL DEFAULT '',
    disposition   TEXT    NOT NULL DEFAULT 'new'
                  CHECK (disposition IN ('new','dismissed','acted')),
    first_seen    INTEGER NOT NULL,
    last_seen     INTEGER NOT NULL,
    UNIQUE (node_key, kind, hash, other_hash)
);
INSERT INTO federation_claim_reports_new
    (id, node_key, kind, hash, other_hash, ber, words, our_head, their_head,
     our_version, their_version, disposition, first_seen, last_seen)
SELECT cr.id, s.public_key, cr.kind, cr.hash, cr.other_hash, cr.ber, cr.words,
       cr.our_head, cr.their_head, cr.our_version, cr.their_version,
       cr.disposition, cr.first_seen, cr.last_seen
FROM federation_claim_reports cr
JOIN federation_catalog_sources s ON s.id = cr.source_id;
DROP TABLE federation_claim_reports;
ALTER TABLE federation_claim_reports_new RENAME TO federation_claim_reports;

-- ── Re-point the tables that referenced the old roots ────────────────────────

-- library_upgrades (039) held a SET NULL provenance FK onto the source table.
-- Same meaning, new root: node_id references the unified row. Source-only
-- nodes got FRESH ids above, so the values are remapped through the key, not
-- copied.
CREATE TABLE library_upgrades_new (
    id           INTEGER PRIMARY KEY,
    recording_id INTEGER NOT NULL REFERENCES recordings(id) ON DELETE CASCADE,
    remote_hash  TEXT    NOT NULL,
    node_id      INTEGER REFERENCES federation_nodes(id) ON DELETE SET NULL,
    entry_key    TEXT    NOT NULL DEFAULT '',
    match        TEXT    NOT NULL,
    ber          REAL    NOT NULL DEFAULT 0,
    our_file_id  INTEGER REFERENCES files(id) ON DELETE SET NULL,
    codec        TEXT    NOT NULL DEFAULT '',
    bitrate      INTEGER NOT NULL DEFAULT 0,
    sample_rate  INTEGER NOT NULL DEFAULT 0,
    bit_depth    INTEGER NOT NULL DEFAULT 0,
    byte_size    INTEGER NOT NULL DEFAULT 0,
    disposition  TEXT    NOT NULL DEFAULT 'new'
                 CHECK (disposition IN ('new', 'dismissed', 'materialized')),
    first_seen   INTEGER NOT NULL DEFAULT 0,
    last_seen    INTEGER NOT NULL DEFAULT 0,
    UNIQUE (recording_id, remote_hash)
);
INSERT INTO library_upgrades_new
    (id, recording_id, remote_hash, node_id, entry_key, match, ber, our_file_id,
     codec, bitrate, sample_rate, bit_depth, byte_size, disposition, first_seen, last_seen)
SELECT u.id, u.recording_id, u.remote_hash, n.id, u.entry_key, u.match, u.ber, u.our_file_id,
       u.codec, u.bitrate, u.sample_rate, u.bit_depth, u.byte_size, u.disposition,
       u.first_seen, u.last_seen
FROM library_upgrades u
LEFT JOIN federation_catalog_sources s ON s.id = u.source_id
LEFT JOIN federation_nodes n ON n.public_key = s.public_key;
DROP TABLE library_upgrades;
ALTER TABLE library_upgrades_new RENAME TO library_upgrades;
CREATE INDEX idx_upgrades_open ON library_upgrades(disposition, last_seen DESC);
CREATE INDEX idx_upgrades_hash ON library_upgrades(remote_hash);

-- The gossip stores (031) held SET NULL branch FKs onto federation_peers.
-- Peer ids were preserved verbatim in step 1, so the VALUES copy unchanged;
-- only the FK target moves.
CREATE TABLE federation_graph_records_new (
    origin        TEXT    PRIMARY KEY,
    seq           INTEGER NOT NULL,
    issued_at     INTEGER NOT NULL,
    expires_at    INTEGER NOT NULL,
    payload       TEXT    NOT NULL,
    received_from INTEGER REFERENCES federation_nodes(id) ON DELETE SET NULL,
    stored_at     INTEGER NOT NULL
);
INSERT INTO federation_graph_records_new SELECT * FROM federation_graph_records;
DROP TABLE federation_graph_records;
ALTER TABLE federation_graph_records_new RENAME TO federation_graph_records;
CREATE INDEX idx_fedgraph_expiry ON federation_graph_records(expires_at);
CREATE INDEX idx_fedgraph_branch ON federation_graph_records(received_from);

CREATE TABLE federation_mark_records_new (
    origin        TEXT    PRIMARY KEY,
    seq           INTEGER NOT NULL,
    issued_at     INTEGER NOT NULL,
    expires_at    INTEGER NOT NULL,
    payload       TEXT    NOT NULL,
    received_from INTEGER REFERENCES federation_nodes(id) ON DELETE SET NULL,
    stored_at     INTEGER NOT NULL
);
INSERT INTO federation_mark_records_new SELECT * FROM federation_mark_records;
DROP TABLE federation_mark_records;
ALTER TABLE federation_mark_records_new RENAME TO federation_mark_records;
CREATE INDEX idx_fedmark_expiry ON federation_mark_records(expires_at);

DROP TABLE federation_peers;
DROP TABLE federation_catalog_sources;
DROP TABLE federation_home_nodes;
