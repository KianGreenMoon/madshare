-- What this server's own listener devices hold (docs/architecture/federation.md
-- §"The household", "Being found").
--
-- A madplayer fetches into a cache and can seed it back, but nothing could ever
-- ask it to: it appears in no catalog and no federation_holdings, because both
-- of those are filled by pulling from friends and members, and a listener node
-- is neither. So it pushes instead — POST /api/madnetwork/holdings, an ordinary
-- authenticated call — and this server answers hash queries with those devices
-- alongside its catalog holders.
--
-- The home server is the tracker for its OWN devices, and only for them. These
-- rows are deliberately NOT re-published: they never enter this node's mesh
-- catalog and never appear in its GET /madnetwork/v0/holdings, which reads
-- federation_holdings — a different table, on purpose. A device serves only what
-- this server vouches for, so advertising it to the wider community would be
-- promising something we cannot make good: a member following it gets a 404 and
-- the swarm reads a refusing holder as a broken one.

-- One row per device, because a device is a thing with an owner and a heartbeat.
CREATE TABLE federation_listener_devices (
    -- The device's ed25519 node key, lowercase hex — the same key its capability
    -- token names as bearer, and the one a mesh address derives from.
    public_key TEXT PRIMARY KEY,

    -- The account that pushed. CASCADE because the vouch IS the account: a token
    -- is issued to a user, so deleting that user withdraws every advertisement
    -- made on its behalf, rather than leaving rows pointing at a device nobody
    -- can vouch for any more.
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,

    -- What the device calls itself. A claim, like every other heard name: shown,
    -- never matched on.
    name TEXT NOT NULL DEFAULT '',

    -- When it last pushed. This is the whole retention mechanism: a device that
    -- goes away stops pushing and stops being advertised, which is the same
    -- freshness idea the availability windows use, sized from the observer's
    -- cadence rather than guessed (federation.ListenerHoldingsTTL).
    updated_at INTEGER NOT NULL
);

CREATE INDEX idx_listener_devices_fresh ON federation_listener_devices(updated_at);

-- The set each device holds, replaced wholesale on every push — the same shape
-- ReplaceSourceHoldings has, for the same reason: a push is a complete statement
-- about what is in a cache right now, not a delta anybody could reconcile.
CREATE TABLE federation_listener_holdings (
    device_key TEXT NOT NULL
        REFERENCES federation_listener_devices(public_key) ON DELETE CASCADE,
    hash TEXT NOT NULL,
    PRIMARY KEY (device_key, hash)
);

-- The lookup that matters: who holds this blob.
CREATE INDEX idx_listener_holdings_hash ON federation_listener_holdings(hash);
