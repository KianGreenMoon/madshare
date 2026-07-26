-- Federation F6 — the gossiped network graph (docs/architecture/federation.md
-- §"Friend-list gossip & the network graph").
--
-- Every node publishes one signed record about its own friendships; friends
-- relay records they did not write, so a node's view grows outward to the whole
-- connected network. What lands here is that store.
--
-- Two shapes per document type, for two different jobs:
--
--   *_records   the signed bytes EXACTLY as received. This is the verifiable
--               truth and the thing relayed onward — never re-serialized, since
--               a record may carry fields this build cannot parse and
--               re-encoding would drop them and break the author's signature
--               for every node downstream (federation/gossip.go).
--   edges/marks the same content denormalized into rows, so admission checks,
--               the map's reachability walk and "who marked this key" are
--               queries rather than a scan that JSON-decodes every payload.
--
-- The denormalized rows are derived data, rewritten atomically whenever their
-- record is replaced. The whole thing is a CACHE in the sense federation_catalog
-- already established: nothing local references it, and dropping it costs only
-- the time to re-sync.
--
-- Ageing is by expires_at (issued_at + the TTL), not by a hop count: a record
-- is dropped once its author stops refreshing it, so an abandoned key fades out
-- of every store on its own and a lifted block clears network-wide.
--
-- received_from is ATTRIBUTION, not the snipping mechanism — it records which
-- friend last delivered the record, for the per-branch admission quota and for
-- showing an admin how a stranger entered their view. Branch snipping is a
-- reachability question answered when the graph is walked (a node reachable
-- only through a blocked peer drops out; one also reachable via another friend
-- stays), so a peer's disappearance must not silently delete records that are
-- still reachable another way.

CREATE TABLE federation_graph_records (
    origin        TEXT    PRIMARY KEY,   -- lowercase-hex ed25519 node key of the author
    seq           INTEGER NOT NULL,      -- author's own counter; highest wins, duplicates are dropped
    issued_at     INTEGER NOT NULL,      -- unix seconds, from the signed payload
    expires_at    INTEGER NOT NULL,      -- issued_at + TTL; swept, and never served once past
    payload       TEXT    NOT NULL,      -- the signed record verbatim
    received_from INTEGER REFERENCES federation_peers(id) ON DELETE SET NULL,
    stored_at     INTEGER NOT NULL
);
CREATE INDEX idx_fedgraph_expiry ON federation_graph_records(expires_at);
CREATE INDEX idx_fedgraph_branch ON federation_graph_records(received_from);

-- One row per claimed friendship. `name` is the author's private label for that
-- node — hearsay about a stranger, never an identity (the key is), which is why
-- every surface that renders it also renders the key.
CREATE TABLE federation_graph_edges (
    origin TEXT    NOT NULL,             -- who claims the friendship
    peer   TEXT    NOT NULL,             -- whom they claim it with
    name   TEXT    NOT NULL DEFAULT '',
    since  INTEGER NOT NULL DEFAULT 0,   -- when the friendship was made (durability signal, F7 trust weighting)
    PRIMARY KEY (origin, peer)
);
-- The admission check ("is this origin named by anyone we already hold?") and
-- the reachability walk both look up by the named side.
CREATE INDEX idx_fedgraph_edge_peer ON federation_graph_edges(peer);

CREATE TABLE federation_mark_records (
    origin        TEXT    PRIMARY KEY,
    seq           INTEGER NOT NULL,
    issued_at     INTEGER NOT NULL,
    expires_at    INTEGER NOT NULL,
    payload       TEXT    NOT NULL,
    received_from INTEGER REFERENCES federation_peers(id) ON DELETE SET NULL,
    stored_at     INTEGER NOT NULL
);
CREATE INDEX idx_fedmark_expiry ON federation_mark_records(expires_at);

-- One row per published block: who blocked whom, when, and why. Displayed
-- branch-weighted (one branch is one voice), so a sybil farm marking the same
-- key renders as one entry rather than a thousand.
CREATE TABLE federation_marks (
    origin TEXT    NOT NULL,             -- who published the block
    target TEXT    NOT NULL,             -- whom they blocked
    at     INTEGER NOT NULL DEFAULT 0,
    reason TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (origin, target)
);
CREATE INDEX idx_fedmark_target ON federation_marks(target);
