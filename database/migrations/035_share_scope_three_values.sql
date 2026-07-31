-- Federation F7 — the sharing scope collapses from a depth ladder to three
-- values (docs/architecture/federation.md §Sharing scope, "Why the ladder
-- collapsed"). The column, the constants and the SQL comparison are unchanged;
-- what changes is which values are legal and what they mean:
--
--     -1  Local          — published to nobody
--      0  Direct friends — the nodes this admin hand-picked, and nobody else
--      ∞  Madnetwork     — our whole community (federation.DepthUnlimited)
--
--   NULL  inherit the node default (madnetwork.default_share_depth, ∞)
--
-- The in-between hop counts are gone because every scope value is a claim about
-- OUR behaviour, while "3 hops" is a claim about other people's — unenforceable
-- the moment a friend holds the bytes. They were stored and compared from F5 but
-- never reachable: no requester was ever at a distance above 0.
--
-- Two rewrites, and they go in opposite directions on purpose:
--
--   1..n → 0. An admin who asked for "friends of friends" asked for more than
--   direct friends and less than everything. Only one of those two survives as
--   an honest promise, and it is the narrower one — an intent that can no longer
--   be expressed must not be resolved by widening it.
--
--   ∞ → NULL (inherit). Explicit ∞ used to mean "any reach we ever grow into",
--   which reached exactly the direct friends who were the only requesters there
--   were. It now means "serve every member of our community", which is a real
--   audience. When a value's meaning changes, consent does not carry over: the
--   recording falls back to whatever the node's own default says, so one
--   node-level decision governs instead of a pin nobody knowingly set.
--
-- The node default itself is deliberately NOT rewritten. It is ∞ by default and
-- stays ∞, now named Madnetwork — the posture is everything to our community,
-- and a node whose admin never touched this setting keeps sharing everything.
-- Only an explicitly chosen in-between value is snapped inward.
UPDATE recordings SET share_depth = 0    WHERE share_depth > 0 AND share_depth < 1048576;
UPDATE recordings SET share_depth = NULL WHERE share_depth >= 1048576;

UPDATE settings SET value = '0'
 WHERE key = 'madnetwork.default_share_depth'
   AND CAST(value AS INTEGER) > 0
   AND CAST(value AS INTEGER) < 1048576;
