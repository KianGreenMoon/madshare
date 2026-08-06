-- Per-counterparty swarm traffic accounting (docs/architecture/swarm-admin.md).
--
-- The companion to the F7 member quotas. Those bound what a member MAY cost us
-- (per_member_rate_kib, member_max_transfers, and the class ceiling above them);
-- nothing said what one HAS cost us, because the per-peer figures lived only in
-- the running node's memory and died with the process.
--
-- One row per counterparty, NOT per (blob, counterparty). That distinction is
-- the whole reason this table can exist while the per-pair one cannot: this is
-- bounded by the size of the community, which the friendship graph already
-- bounds, while hashes × peers grows with the library forever and answers a
-- question that is almost always about right now.

CREATE TABLE swarm_peer_traffic (
    -- The node's ed25519 public key, lowercase hex — the identity every other
    -- federation surface addresses a node by, because heard names are claims
    -- that change and the frontier rotation recycles source ids.
    --
    -- The EMPTY STRING is the unplaced bucket: one row for every requester we
    -- could not name. A friend or a member arrives with a key (serveAudienceKey
    -- establishes it while resolving the audience); a guest served under
    -- serve_guests, or a listener device carrying a capability token, arrives
    -- with only a mesh address. Those are folded together rather than given a
    -- row each: a keyed row set is bounded by the community, while an
    -- address-keyed one is sized by whoever chooses to talk to us — N forged
    -- keys would be N rows, which is the sybil shape the class quotas exist to
    -- answer. Dropping them entirely was the worse option: then the panel's
    -- figures quietly fail to add up and nothing on screen says why.
    public_key TEXT PRIMARY KEY,

    -- Served to that node (seeding) and pulled from it (fetching), all time.
    --
    -- No wasted_bytes here, unlike swarm_traffic: waste is received minus kept,
    -- computed once at the end of a transfer that may have drawn on several
    -- holders, so charging it to one of them would be a guess.
    up_bytes   INTEGER NOT NULL DEFAULT 0,
    down_bytes INTEGER NOT NULL DEFAULT 0,

    first_at   INTEGER NOT NULL,
    last_at    INTEGER NOT NULL
);

-- No name column and no foreign key to federation_peers or
-- federation_catalog_sources. The page joins those at read time for the current
-- name and the node's class (friend / member / gone), so a node we have since
-- unfriended — or one the discovery rotation has evicted — keeps its bytes and
-- shows as its key. What a node cost us does not stop being true when we forget
-- who it was, and CASCADE would have deleted exactly that history.

-- Ordering the panel: most recently active first when the byte counts tie.
CREATE INDEX idx_swarm_peer_last ON swarm_peer_traffic(last_at);
