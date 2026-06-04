-- License-based guest access is now evaluated live at query time via
-- accessClause rather than written to guest_playable. Clear any flags that were
-- set by the old auto-derive mechanism (guest_playable_manual = 0 means the
-- flag was never explicitly set by an admin).
UPDATE files SET guest_playable = 0 WHERE guest_playable_manual = 0 AND guest_playable = 1;
