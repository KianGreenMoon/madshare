-- A listener node's home servers (docs/architecture/federation.md
-- §"The household").
--
-- This is the one-sided half of the capability token. A madplayer already
-- accepts "this bearer is mine" FROM a home server in order to be served by
-- third parties; recording that server here is what lets it accept the same
-- statement, from the same signer, in order to serve them back. Nothing new is
-- trusted — what changes is which node does the believing.
--
-- Deliberately NOT a row in federation_peers, and that is the load-bearing part
-- rather than a schema preference. A peer row publishes a gossip edge, an edge
-- is how a node appears in everybody else's map, and being in nobody's map is
-- the premise the whole listener design rests on: it is why no walk can place a
-- device, which is why the token exists at all. Friending the home server
-- instead would make nearly all of this fall out of machinery that already
-- exists, at the price of an admin accept per phone and a phone in the community
-- graph. Refused 2026-08-09.
--
-- So: no state machine, no pending/blocked, no user mapping, no gossip, no
-- CASCADE from anywhere. A trust record, not a relationship.

CREATE TABLE federation_home_nodes (
    -- The home server's ed25519 node key, lowercase hex. Learned from the
    -- `issuer` field of the token the device was already asking for, so the
    -- record costs no extra exchange and no extra round trip.
    public_key TEXT PRIMARY KEY,

    -- Where the device signs in over HTTP. Not used to decide anything — a mesh
    -- request is placed by deriving the address from the key above — but it is
    -- what tells a person which of their servers a row is, and what a client
    -- matches against its own sign-in list when the account goes away.
    base_url TEXT NOT NULL DEFAULT '',

    -- What the server calls itself, for display. A claim like every other heard
    -- name: never an identity, and never matched on.
    name TEXT NOT NULL DEFAULT '',

    added_at INTEGER NOT NULL
);
