-- Federation F6 — what a block records, so it can be published as a distrust
-- mark (docs/architecture/federation.md §Friend-list gossip, "Distrust marks").
--
-- Every block publishes a mark: there are no private blocks. A mark carries the
-- blocked key, when it happened, and a short reason — a bare key is an
-- anonymous downvote that forces the reader to ask out-of-band what happened,
-- while a reason is what lets them judge whether it applies to them.
--
-- Both columns describe the CURRENT block only. Unblocking leaves them behind
-- rather than clearing them: nothing reads them while the peer is not blocked,
-- and the next block overwrites both. The mark itself is rebuilt from the set of
-- blocked peers on every publish, so lifting a block drops it from this node's
-- record and — as that record propagates — from every store holding it.
ALTER TABLE federation_peers ADD COLUMN block_reason TEXT    NOT NULL DEFAULT '';
ALTER TABLE federation_peers ADD COLUMN blocked_at   INTEGER NOT NULL DEFAULT 0;
