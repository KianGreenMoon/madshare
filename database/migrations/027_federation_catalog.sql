-- Federation F2 — catalog (docs/architecture/federation.md §Catalog).
--
-- Browsing the merged madnetwork catalog is a new capability,
-- madnetwork.access: admin holds it by default, and the stand-alone
-- 'madnetwork' role makes it grantable to any trusted local user by stacking
-- a role (the roles-only model has no per-user grants).
INSERT INTO role_permissions (role_id, permission) VALUES (1, 'madnetwork.access');
INSERT INTO roles (id, name, built_in) VALUES (5, 'madnetwork', 1);
INSERT INTO role_permissions (role_id, permission) VALUES (5, 'madnetwork.access');

-- Pull-and-cache sync state per peer: the serial (content hash) of the last
-- snapshot applied from this friend, and when the catalog was last confirmed
-- fresh (a not-modified check also bumps synced_at).
ALTER TABLE federation_peers ADD COLUMN catalog_serial    TEXT    NOT NULL DEFAULT '';
ALTER TABLE federation_peers ADD COLUMN catalog_synced_at INTEGER NOT NULL DEFAULT 0;

-- The local cached copy of each friend's published catalog: one row per remote
-- appearance (their tagset), denormalized to display text — remote claims are
-- hints, so nothing references local entities. entry_key / recording_key are
-- the peer's own stable ids (opaque here); renditions is a JSON array of the
-- recording's advertised blobs ({hash,size,codec,bitrate,sample_rate,
-- bit_depth,duration}) — the holders/tracker data the swarm (F4) reads.
-- Rows vanish with their peer (CASCADE); a blocked peer's rows are kept but
-- hidden from every browse query (unblock restores the view without a resync).
CREATE TABLE federation_catalog (
    peer_id        INTEGER NOT NULL REFERENCES federation_peers(id) ON DELETE CASCADE,
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
    PRIMARY KEY (peer_id, entry_key)
);
CREATE INDEX idx_fedcat_browse ON federation_catalog(album_artist, album);
