-- Federation F6 — contradicted identity claims (docs/architecture/federation.md
-- §Trust graph, "report contradicted identity claims").
--
-- A peer's catalog makes claims this node can CHECK: it advertises a content hash
-- together with the head of its own acoustic fingerprint. When we hold the same
-- bytes we know the true fingerprint, so a materially different claim contradicts
-- something we can hash ourselves. This table is where such a finding waits for a
-- human.
--
-- Nothing here is automatic. Blocking stays a manual act — an automatic
-- reputation score is a weapon in intra-network wars — so a row is evidence put
-- in front of an admin beside the Block action, never an input to a score. The
-- wording follows the same rule: a CONTRADICTION, not a lie. Innocent
-- explanations are more common than malice (a different chromaprint build, a peer
-- that grouped a rendition wrongly through its own sloppiness, or an honest relay
-- repeating someone else's claim).
--
-- disposition is what stops a repeating sync from re-alarming anybody: the
-- (peer, kind, hash, other_hash) key makes detection idempotent, so re-finding a
-- contradiction refreshes last_seen and leaves the admin's decision alone.
--
-- Rows are a cache in the sense federation_catalog established — rebuildable by
-- re-checking, referenced by nothing local, and CASCADE-deleted with the peer, so
-- forgetting a node forgets what we found about it.
CREATE TABLE federation_claim_reports (
    id          INTEGER PRIMARY KEY,
    peer_id     INTEGER NOT NULL REFERENCES federation_peers(id) ON DELETE CASCADE,
    -- kind is which check found it:
    --   held_blob  the peer advertises a hash we hold, with a fingerprint that
    --              does not match our own copy of those exact bytes. Airtight:
    --              identical bytes cannot fingerprint differently (modulo the
    --              algo_version caveat, which is why both versions are stored).
    --   grouping   the peer asserts two renditions are the same recording, we
    --              hold both, and our own fingerprints of them disagree. Needs no
    --              wire claim at all — the assertion is testable locally.
    kind        TEXT    NOT NULL CHECK (kind IN ('held_blob','grouping')),
    hash        TEXT    NOT NULL,
    other_hash  TEXT    NOT NULL DEFAULT '',  -- the second blob, for 'grouping'
    -- The evidence, so the peer card can show what was compared and how each side
    -- was obtained rather than asserting a verdict.
    ber          REAL    NOT NULL DEFAULT 0,  -- measured bit-error rate
    words        INTEGER NOT NULL DEFAULT 0,  -- fingerprint words actually compared
    our_head     TEXT    NOT NULL DEFAULT '', -- base64, our own fingerprint head
    their_head   TEXT    NOT NULL DEFAULT '', -- base64, as advertised
    our_version  TEXT    NOT NULL DEFAULT '',
    their_version TEXT   NOT NULL DEFAULT '',
    disposition TEXT    NOT NULL DEFAULT 'new'
                CHECK (disposition IN ('new','dismissed','acted')),
    first_seen  INTEGER NOT NULL,
    last_seen   INTEGER NOT NULL,
    UNIQUE (peer_id, kind, hash, other_hash)
);

CREATE INDEX idx_claim_reports_open ON federation_claim_reports(disposition, last_seen DESC);
