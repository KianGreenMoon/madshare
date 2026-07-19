-- Federation F4 — swarm holdings tracker (docs/architecture/federation.md
-- §Distribution). A friend's LIBRARY is already advertised in the catalog
-- (federation_catalog, migration 027). This table caches what a friend holds in
-- its download CACHE and is willing to seed — pulled per-friend from
-- GET /madnetwork/v0/holdings on the same refresh cadence as the catalog. The
-- union of catalog holders and holdings holders is the swarm tracker
-- ("who has hash H"), which makes a downloaded blob a discoverable seeder.
--
-- Rows vanish with their peer (CASCADE); an atomic replace rewrites a friend's
-- whole set each sync round. hash is a content hash (opaque here).
CREATE TABLE federation_holdings (
    peer_id INTEGER NOT NULL REFERENCES federation_peers(id) ON DELETE CASCADE,
    hash    TEXT    NOT NULL,
    PRIMARY KEY (peer_id, hash)
);
CREATE INDEX idx_fedhold_hash ON federation_holdings(hash);
