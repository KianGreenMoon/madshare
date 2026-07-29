-- Federation F6 — separate what a peer calls itself from what we call it
-- (docs/architecture/federation.md §"names are a convenience, the key is the
-- identity").
--
-- Three different names were collapsed into federation_peers.name: our own
-- self-name on the wire (config, never stored here), the HEARD name a peer
-- claims for itself, and the LOCAL LABEL this admin chose for it. The column was
-- seeded from a card or pair request, then overwritten by a rename — which
-- destroyed the claim — and afterwards never refreshed, so a node that renamed
-- itself kept its old name here forever and an admin who renamed a peer could no
-- longer see what that peer calls itself.
--
-- After this migration the two have separate owners and never overwrite each
-- other: heard_name is the peer's claim, refreshed on every successful contact
-- (ping, pairing), while name is written by an admin rename and nothing else.
-- Display resolves label ?? heard name ?? short key.
--
-- Existing rows move their name into heard_name and start with no local label.
-- The column cannot be classified retroactively — a value seeded from a card is
-- indistinguishable from one an admin typed — so this picks the direction whose
-- failure is visible and recoverable: a peer that was renamed reverts to its own
-- name on the next contact and can be renamed again in two clicks, whereas
-- keeping the old value as a label would pin a name the admin never chose and
-- reproduce exactly the bug this fixes.
ALTER TABLE federation_peers ADD COLUMN heard_name TEXT NOT NULL DEFAULT '';
UPDATE federation_peers SET heard_name = name;
UPDATE federation_peers SET name = '';
