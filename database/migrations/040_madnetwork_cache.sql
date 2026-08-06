-- The madnetwork download cache index
-- (docs/architecture/madnetwork-cache.md).
--
-- Until now the cache was a flat directory of files named after their content
-- hash and nothing else: no record of when a blob arrived, when anyone last
-- read it, or what it even is. That was enough while nothing ever removed one.
-- It is not enough to CONTROL the cache, and it is not enough to forget by
-- itself later — a retention policy keyed on last use has nowhere to read that
-- from, since a directory walk cannot answer it and atime is unusable on a
-- normally-mounted filesystem.
--
-- The relationship between this table and the directory is one-directional and
-- deliberate:
--
--     the FILES ON DISK are authoritative; this table DESCRIBES them.
--
-- Every existing cache path — EnsureBlob, seedableBlob, cacheHoldings, the
-- startup eviction sweep — keeps reading the directory and knows nothing about
-- this table. So a row that outlives its file is a stale row that
-- ReconcileMadnetworkCache drops, never a phantom entry the swarm could
-- advertise. Believing the disk is what keeps that guarantee cheap.
--
-- What is NOT here is as considered as what is:
--
--   * No provenance. Nothing records which node served the bytes, or what that
--     node called them. Who holds what belongs to the swarm/transfer admin
--     surface, which will own it for both directions of transfer; a partial
--     version here would be something that surface later has to contradict.
--
--   * No pin. Removal is manual today, and when the retention daemon lands it
--     evicts by policy alone with nothing exempt from it.
--
-- The descriptive columns come from the FILE'S OWN TAGS (media.ExtractTags, the
-- same call the upload ingest makes), read once when the row is created. They
-- are not any node's claim about the bytes; they are what the bytes say about
-- themselves. That is what lets a cached blob still name itself — and stay
-- searchable — long after whoever we fetched it from has left the network.

CREATE TABLE madnetwork_cache (
    -- Lowercase sha256 hex. Also the file's name on disk, which is what makes
    -- reconciliation a plain set difference against os.ReadDir.
    hash         TEXT    PRIMARY KEY,
    byte_size    INTEGER NOT NULL DEFAULT 0,
    -- The blob's own filename, needed to write it into storage with a real
    -- extension when it is materialized, and to save it under a sane name when
    -- it is downloaded. Empty for a row adopted from a pre-existing cache.
    filename     TEXT    NOT NULL DEFAULT '',

    -- The file's own embedded tags. Empty is normal and not an error: an
    -- untagged blob falls back to its filename, then to its hash.
    title        TEXT    NOT NULL DEFAULT '',
    artist       TEXT    NOT NULL DEFAULT '',
    album        TEXT    NOT NULL DEFAULT '',

    fetched_at   INTEGER NOT NULL,
    -- Last read BY A LOCAL USER. A member of the community pulling this blob out
    -- of our cache over the mesh deliberately does NOT count: seeding is a
    -- service we render with bytes we happen to hold, not a reason to go on
    -- holding them, and a disk kept full purely by other people's traffic is the
    -- outcome the retention policy exists to avoid.
    last_used_at INTEGER NOT NULL
);

-- The two orders the retention daemon and the control page both need: evict
-- (and sort by) least-recently-used, and find the largest entries first.
CREATE INDEX idx_mncache_lru  ON madnetwork_cache(last_used_at);
CREATE INDEX idx_mncache_size ON madnetwork_cache(byte_size);
