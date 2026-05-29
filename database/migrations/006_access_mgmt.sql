-- Phase 3c auth: content-access management (see docs/architecture/auth.md §5, §10).

-- Server settings as a key/value store. Used here for the opt-in
-- license->guest_playable auto-derivation policy:
--   access.autoderive.enabled  = "1" | "0"
--   access.autoderive.licenses = comma-separated license allow-list
-- The store is generic so later admin-tunable settings can reuse it.
CREATE TABLE settings (
  key   TEXT NOT NULL PRIMARY KEY,
  value TEXT NOT NULL
);

-- Tracks whether guest_playable was set by an explicit human decision.
-- Auto-derivation only ever *grants* and never touches a manually set row, so a
-- manual override always wins (auth.md §5.1).
ALTER TABLE files ADD COLUMN guest_playable_manual INTEGER NOT NULL DEFAULT 0;
