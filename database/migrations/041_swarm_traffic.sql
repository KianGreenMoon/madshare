-- Per-blob swarm traffic accounting (docs/architecture/swarm-admin.md).
--
-- Until now this node moved bytes and remembered none of it. TransferStats
-- describes a fetch WHILE IT RUNS and dies with the transfer; the seeding side
-- counted nothing at all, ever. So "what has my node actually contributed" had
-- no answer, and neither did "what did this file cost me".
--
-- One row per content hash this node has ever moved, in either direction.
-- Counters only: there are no per-file settings here, because there are no
-- per-file settings at all (owner, 2026-08-06 — a rate cap protects the shared
-- LINE, so only the node-wide sum means anything, and fairness between peers is
-- already the member quotas' job).
--
-- Properties worth stating, because the flusher depends on all three:
--
--   * A hash with no row is the normal case — never transferred, zero traffic.
--     Every listing LEFT JOINs, so nothing has to pre-create rows for a library
--     of 40 000 files.
--
--   * Every write is an UPSERT adding a delta, from ONE place (api's flusher,
--     draining the node's in-memory counters). Nothing else writes here, which
--     is what lets accounting be batched off the transfer path entirely.
--
--   * Rows OUTLIVE the blobs they describe. Removing a cached blob, trashing a
--     recording or hard-deleting a file leaves the row standing: the bytes
--     really did move, and a node's contribution history must not be erasable as
--     a side effect of housekeeping. Only an explicit "forget stats" deletes one.
--
-- The node's all-time totals are SUM() over this table rather than separate
-- counters kept beside it — two stores of one number eventually disagree, and
-- the aggregate is indexed and instant.

CREATE TABLE swarm_traffic (
    -- Lowercase sha256 hex: the same content hash the swarm addresses blobs by,
    -- which is why this table needs no foreign key. It describes bytes, not a
    -- library row, and it must survive the disappearance of both.
    hash         TEXT PRIMARY KEY,

    -- Served to the mesh (seeding) and pulled off it (fetching), all time.
    up_bytes     INTEGER NOT NULL DEFAULT 0,
    down_bytes   INTEGER NOT NULL DEFAULT 0,

    -- Received and thrown away: a chunk that failed its sha256, or a whole
    -- attempt abandoned when the swarm fell back. Computed as received minus
    -- delivered at the end of each transfer, so it needs no per-failure
    -- bookkeeping. It is the only visible symptom of a holder serving corrupt
    -- bytes, and down_bytes without it would understate what the mesh cost us.
    wasted_bytes INTEGER NOT NULL DEFAULT 0,

    first_at     INTEGER NOT NULL,
    -- Last byte moved in either direction. Distinct from the cache index's
    -- last_used_at, which counts LOCAL READS only and belongs to retention;
    -- this one counts mesh activity and belongs to the swarm view.
    last_at      INTEGER NOT NULL
);

-- The three orders the swarm listing sorts by beyond the library's own.
CREATE INDEX idx_swarm_up   ON swarm_traffic(up_bytes);
CREATE INDEX idx_swarm_down ON swarm_traffic(down_bytes);
CREATE INDEX idx_swarm_last ON swarm_traffic(last_at);
