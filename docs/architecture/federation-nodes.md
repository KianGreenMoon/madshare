# One node, one row — the federation node store

Decided 2026-08-13 (owner). Status: **designed, not built**. Companion to
`federation.md` and its three splits; this doc owns the STORAGE shape for
"what does this server know about node X".

## The problem

Four tables describe a node, all keyed by (or joined on) its public key:
`federation_peers` (an admin acted), `federation_catalog_sources` (the sweep
pulled), `federation_home_nodes` (a device enrolled), `swarm_peer_traffic`
(bytes moved). Each split was justified alone; together, one fact lives in
two places and every question needs a join:

- `last_seen` exists on peers AND sources → freshness reads
  `MAX(s.last_seen, COALESCE(p.last_seen, 0))` (`srcLastSeen`).
- `heard_name` lands on the peer row via the friendship ping but on the
  source row via the discovery pull → three label-chain expressions
  (`peerLabelExpr`, `sourceHeardExpr`, `swarmPeerIdentityCols`), one of which
  exists because friends once displayed as bare key prefixes.
- "What is this node to us" is a CASE over two tables on the swarm page, a
  `notBlocked` join in the browse, and an in-memory walk in membership.
- The catalog satellites key on `source_id`, whose rows the frontier rotation
  EVICTS — so ids recycle, and the standing rule "a node is addressed by its
  public key, never the source id" is a gotcha every query must remember.

Bugs already charged to this: devices folded into one provider entry
(source-id keying), the freshness-window pick needing both tables, friends
losing their names on the cache page.

## The decision

**One `federation_nodes` table, column groups instead of tables.** A row is
an identity plus up to three optional relationships, each marked present by
its own columns:

```sql
CREATE TABLE federation_nodes (
    id          INTEGER PRIMARY KEY,          -- admin-surface handle only; never federated
    public_key  TEXT NOT NULL UNIQUE,         -- lowercase hex ed25519; THE identity

    -- Observations: any contact may update these. ONE clock, ONE name now.
    heard_name  TEXT    NOT NULL DEFAULT '',  -- the node's own claim, never matched on
    first_seen  INTEGER NOT NULL DEFAULT 0,
    last_seen   INTEGER NOT NULL DEFAULT 0,
    hinted_at   INTEGER NOT NULL DEFAULT 0,   -- gossiped-freshness receipt (window pick)

    -- Trust group: non-NULL trust_state = an admin acted (the old peer row).
    trust_state  TEXT CHECK (trust_state IS NULL OR trust_state IN
                 ('pending_outgoing','pending_incoming','friend','blocked')),
    prev_state   TEXT    NOT NULL DEFAULT '',
    label        TEXT    NOT NULL DEFAULT '', -- the admin's local name (was peers.name)
    user_id      INTEGER REFERENCES users(id) ON DELETE SET NULL,
    trusted_at   INTEGER NOT NULL DEFAULT 0,  -- was peers.created_at
    block_reason TEXT    NOT NULL DEFAULT '',
    blocked_at   INTEGER NOT NULL DEFAULT 0,

    -- Sync group: sync_added_at > 0 = in the pull rotation (the old source
    -- row's EXISTENCE, which one table must carry explicitly — a pending peer
    -- is a trust row the sweep must not pull from by accident).
    sync_added_at     INTEGER NOT NULL DEFAULT 0,
    catalog_serial    TEXT    NOT NULL DEFAULT '',
    catalog_synced_at INTEGER NOT NULL DEFAULT 0,
    attempted_at      INTEGER NOT NULL DEFAULT 0,

    -- Household group: home_added_at > 0 = this (listener) node signs in there.
    home_added_at INTEGER NOT NULL DEFAULT 0,
    home_base_url TEXT    NOT NULL DEFAULT ''
);
```

The satellites re-key onto the public key and CASCADE off the node row:
`federation_catalog(node_key, entry_key)`, `federation_holdings(node_key,
hash)`, `federation_claim_reports.node_key`. The recycled-id hazard class
ends; the storage cost (64-char key per row instead of an int) was weighed
and accepted. `id` stays only so the admin peer endpoints keep their URLs —
it is never sent to, or accepted from, the mesh.

**`swarm_peer_traffic` stays its own table** (owner): different write cadence
(the 30 s flusher), the `''` unplaced bucket is not a node, and "history
outlives standing" stays structural — it now LEFT JOINs one table instead of
two. The gossip store also stays: graph records are signed documents
received, a log — not knowledge about a node.

## The five properties the split existed for, preserved

1. *"The peer table reads as admin decisions"* → the admin UI filters
   `trust_state IS NOT NULL`; strangers have NULL there.
2. *"A home node must not look like a peer"* → a ROW publishes nothing; only
   gossip publishes edges. The household stays out of every map exactly as
   before.
3. *"Traffic survives unfriending"* → unfriending clears the trust group;
   the row (and the separate counters) remain.
4. *"Blocked = kept-hidden, instant"* → `trust_state = 'blocked'` is now a
   direct column read where `notBlocked` joined.
5. *"Eviction must not orphan"* → retention keeps a row while ANY group is
   live (trust, home, community member) or a catalog is cached; past
   `discovery_cap` the coldest member-only rows are deleted and CASCADE
   clears their satellites. Same rule as today, one table.

## What the merge deleted (built 2026-08-13)

The MAX()-of-two-clocks (`srcLastSeen` is now just `s.last_seen`), the
two-landing-spot heard-name chain (`sourceHeardExpr` is just `s.heard_name`),
the second join and peer-table COALESCEs in `swarmPeerIdentityCols`,
`sourceJoin`'s LEFT JOIN onto peers, and the two-table freshness-window pick
(`sourcePingedExpr` reads one row). Class is still a CASE — "what is this
node to us" is genuinely computed — but over one row's columns. Two
behaviours quietly improved: a member the sweep cached becomes a friend
WITHOUT losing its catalog (the old flow inserted a fresh peer row beside
the source), and a brand-new friend appears on the /madnetwork strip
immediately instead of after its first sweep.

## Migration (046, one shot)

1. Create `federation_nodes`; fold peers ∪ sources ∪ home_nodes by key:
   `heard_name` = peer's claim if set else source's (the ping refreshes
   more often); `last_seen` = MAX of both; `first_seen` = earliest non-zero.
2. Rebuild the three satellites keyed by `node_key`, joining their old
   `source_id` across.
3. Drop the three old tables.

Known breakage, priced in: `database_test.go` version/table assertions,
`api.fakeRepo`, `federation.PeerStore` (reshaped around keys — breaks the
mesh lab's `emptyStore`/`memStore`; run `go vet -tags tests ./tests/mesh/...`),
and every query in `database/madnetwork*.go` that spells `p.`/`s.`.
Zero mesh-visible change: the wire protocol, gossip, and token formats do
not move.

## The Go surface (decided 2026-08-13)

`Peer` (the Go struct) SURVIVES as the trust-group view over a node row: the
schema merges now, the Go types do not — a smaller diff, and the admin
surface keeps its shape. **Deliberate follow-up, not yet scheduled: fold
`Peer` (and `CatalogSource`) into one `Node` struct** once the merged table
has settled; until then the two structs are two VIEWS of one row, which is
honest, but a reader of the Go code alone cannot see that the storage is
unified — that gap is the debt this paragraph exists to remember.
