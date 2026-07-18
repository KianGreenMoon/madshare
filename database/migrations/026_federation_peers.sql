-- Federation F1 — friendship (docs/architecture/federation.md).
--
-- The trusted-peer table: one row per remote madnetwork node this node knows.
-- public_key (lowercase hex ed25519) is the peer's durable identity; its mesh
-- address derives from the key at runtime, so no address is stored.
--
-- State machine:
--   pending_outgoing  we imported their node card and are waiting for their node
--                     to confirm (their admin must import ours)
--   pending_incoming  their node introduced itself over the mesh; awaiting our
--                     admin's explicit accept (deliberate friending)
--   friend            mutual — both sides confirmed
--   blocked           all application-layer service refused; prev_state remembers
--                     what an unblock returns the peer to
--
-- user_id maps a personal (madplayer) node to a LOCAL user account, so every
-- existing ACL applies to its owner unchanged — federation adds no parallel
-- permission system (federation.md §Principals & access).
CREATE TABLE federation_peers (
    id          INTEGER PRIMARY KEY,
    public_key  TEXT    NOT NULL UNIQUE,
    name        TEXT    NOT NULL DEFAULT '',
    state       TEXT    NOT NULL DEFAULT 'pending_outgoing'
                CHECK (state IN ('pending_outgoing','pending_incoming','friend','blocked')),
    prev_state  TEXT    NOT NULL DEFAULT '',
    user_id     INTEGER REFERENCES users(id) ON DELETE SET NULL,
    created_at  INTEGER NOT NULL,
    last_seen   INTEGER NOT NULL DEFAULT 0   -- unix seconds; 0 = never seen on the mesh
);
