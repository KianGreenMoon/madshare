-- The user mapping becomes a plain per-peer guest-only flag (owner 2026-08-13;
-- docs/architecture/federation-access.md §Principals & access).
--
-- federation_nodes.user_id let an admin bind a friend node's key to a local
-- account so the node was answered with that account's rights. The binding
-- came from misreading "authorize the node as a user" — the real requirement
-- was the listener node, a person with credentials on a device — and since
-- the local model grants either content.access or nothing beyond the
-- guest-playable policy, the whole mapping ever expressed exactly one bit:
-- does this friend see the full published set, or guest-accessible content
-- only. That bit is now a column of its own, set directly by the admin.
--
-- The backfill freezes each mapped peer's CURRENT effective audience: mapped
-- to an active account holding content.access = full (0), mapped to anything
-- else — no such permission, or a disabled account — = guest-only (1),
-- matching what PeerAudience resolved at request time. A peer narrowed only
-- by its account's disabled state stays narrowed after the account recovers;
-- the flag is an admin decision now, not a live lookup, and the admin page
-- shows it.

ALTER TABLE federation_nodes ADD COLUMN guest_only INTEGER NOT NULL DEFAULT 0;

UPDATE federation_nodes SET guest_only = 1
 WHERE user_id IS NOT NULL
   AND NOT EXISTS (
        SELECT 1 FROM users u
        JOIN user_roles ur       ON ur.user_id = u.id
        JOIN role_permissions rp ON rp.role_id = ur.role_id
        WHERE u.id = federation_nodes.user_id
          AND u.disabled = 0
          AND rp.permission = 'content.access');

ALTER TABLE federation_nodes DROP COLUMN user_id;
