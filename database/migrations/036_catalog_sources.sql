-- Federation F7 item 5 — discovery beyond the friend ring
-- (docs/architecture/federation.md §Discovery beyond the friend ring).
--
-- Until now every cached catalog row hung off federation_peers, and every
-- browse query joined `state = 'friend'`. That was exactly right while a
-- friend was the only node we ever pulled from. F7 serves our whole community,
-- so this node must also be able to CACHE a member's catalog — and a member is
-- a node no admin here has decided anything about.
--
-- Those are two different facts about a node, and they now live in two tables:
--
--   federation_peers            a local trust decision — we asked, they asked,
--                               we accepted, we blocked. Rows appear because an
--                               ADMIN acted. Unchanged by this migration except
--                               that the catalog sync state moves out of it.
--
--   federation_catalog_sources  a node whose published catalog we hold a cached
--                               copy of. Rows appear because the SWEEP pulled
--                               from it. Every friend is a source; so is every
--                               member the frontier rotation has reached.
--
-- Keeping them apart is what stops the peer table — whose rows an admin reads
-- as "someone decided something here" — from filling with hundreds of nodes
-- nobody chose. It also puts the cache's retention rule on the cache's own
-- table: a source is kept while it is one of our peers or a member of our
-- community, and evicted oldest-first past the configured cap.
--
-- A peer row per member was considered and refused. It looks cheaper than a new
-- table and is not: SQLite cannot alter a CHECK constraint, so admitting a
-- 'member' state would mean rebuilding federation_peers anyway — the same
-- migration weight, in exchange for merging two meanings that want to stay
-- apart.
--
-- Blocking still hides a cached catalog without deleting it (unblock restores
-- the view with no resync), but it is now expressed as a join to the peer table
-- rather than a CASCADE: the row is hidden because a peer row says 'blocked',
-- not because the cache is rooted in one.

CREATE TABLE federation_catalog_sources (
    id                INTEGER PRIMARY KEY,
    public_key        TEXT    NOT NULL UNIQUE,   -- lowercase hex ed25519; the mesh address derives from it
    heard_name        TEXT    NOT NULL DEFAULT '', -- what the node calls itself (never an admin's label)
    catalog_serial    TEXT    NOT NULL DEFAULT '', -- serial of the snapshot we hold; the `since=` we send
    catalog_synced_at INTEGER NOT NULL DEFAULT 0,  -- last round that confirmed the copy fresh
    attempted_at      INTEGER NOT NULL DEFAULT 0,  -- last pull ATTEMPT — the frontier rotates on this,
                                                   -- so an unreachable node cannot starve the others
    first_seen        INTEGER NOT NULL DEFAULT 0,  -- when we started caching this node
    last_seen         INTEGER NOT NULL DEFAULT 0   -- last successful contact; feeds the freshness window
);

CREATE INDEX idx_fedsrc_rotation ON federation_catalog_sources(attempted_at);

-- Every existing peer becomes a source, carrying the sync state it owned. All
-- states, not just friends: a blocked peer's cached rows are deliberately kept
-- (hidden, so an unblock restores them), and a pending peer becomes a friend
-- often enough that discarding the row would only cost a resync.
INSERT INTO federation_catalog_sources
    (public_key, heard_name, catalog_serial, catalog_synced_at, attempted_at, first_seen, last_seen)
SELECT public_key, heard_name, catalog_serial, catalog_synced_at, catalog_synced_at, created_at, last_seen
FROM federation_peers;

-- ── Re-key the three cached tables from peer_id to source_id ────────────────
-- Same columns, same meanings; only the root changes. ON DELETE CASCADE still
-- holds, now against the source: dropping a source is how the cache is evicted.

CREATE TABLE federation_catalog_new (
    source_id      INTEGER NOT NULL REFERENCES federation_catalog_sources(id) ON DELETE CASCADE,
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
    PRIMARY KEY (source_id, entry_key)
);
INSERT INTO federation_catalog_new
SELECT s.id, c.entry_key, c.recording_key, c.title, c.artist, c.album_artist,
       c.album, c.genre, c.year, c.track_number, c.disc_number, c.duration,
       c.license, c.guest_playable, c.renditions
FROM federation_catalog c
JOIN federation_peers p           ON p.id = c.peer_id
JOIN federation_catalog_sources s ON s.public_key = p.public_key;
DROP TABLE federation_catalog;
ALTER TABLE federation_catalog_new RENAME TO federation_catalog;
CREATE INDEX idx_fedcat_browse ON federation_catalog(album_artist, album);

CREATE TABLE federation_holdings_new (
    source_id INTEGER NOT NULL REFERENCES federation_catalog_sources(id) ON DELETE CASCADE,
    hash      TEXT    NOT NULL,
    PRIMARY KEY (source_id, hash)
);
INSERT INTO federation_holdings_new
SELECT s.id, h.hash
FROM federation_holdings h
JOIN federation_peers p           ON p.id = h.peer_id
JOIN federation_catalog_sources s ON s.public_key = p.public_key;
DROP TABLE federation_holdings;
ALTER TABLE federation_holdings_new RENAME TO federation_holdings;
CREATE INDEX idx_fedhold_hash ON federation_holdings(hash);

-- Contradicted-claim reports (F6, migration 034) follow their evidence. The
-- checks read the cached catalog, so a report is a finding about a SOURCE's
-- claims; a member can contradict itself exactly as a friend can, and the admin
-- surface can still offer a block — by key, which is all a block ever needed.
CREATE TABLE federation_claim_reports_new (
    id            INTEGER PRIMARY KEY,
    source_id     INTEGER NOT NULL REFERENCES federation_catalog_sources(id) ON DELETE CASCADE,
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
    UNIQUE (source_id, kind, hash, other_hash)
);
INSERT INTO federation_claim_reports_new
    (id, source_id, kind, hash, other_hash, ber, words, our_head, their_head,
     our_version, their_version, disposition, first_seen, last_seen)
SELECT cr.id, s.id, cr.kind, cr.hash, cr.other_hash, cr.ber, cr.words,
       cr.our_head, cr.their_head, cr.our_version, cr.their_version,
       cr.disposition, cr.first_seen, cr.last_seen
FROM federation_claim_reports cr
JOIN federation_peers p           ON p.id = cr.peer_id
JOIN federation_catalog_sources s ON s.public_key = p.public_key;
DROP TABLE federation_claim_reports;
ALTER TABLE federation_claim_reports_new RENAME TO federation_claim_reports;
CREATE INDEX idx_claim_reports_open ON federation_claim_reports(disposition, last_seen DESC);

-- The sync state has moved to the source row that now owns it. Leaving these
-- behind would give the same fact two homes, and the stale one would be the one
-- an admin's peer card reads.
ALTER TABLE federation_peers DROP COLUMN catalog_serial;
ALTER TABLE federation_peers DROP COLUMN catalog_synced_at;
