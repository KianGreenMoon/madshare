-- Playlists & favorites (docs/api/playlists.md). Per-user, private-only in
-- v1 (no visibility column — sharing later is an additive migration).
-- Favorites is a per-user system playlist (kind='favorites').
CREATE TABLE playlists (
  id         INTEGER PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name       TEXT    NOT NULL,
  kind       TEXT    NOT NULL DEFAULT 'regular',   -- 'regular' | 'favorites'
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX idx_playlists_user ON playlists(user_id);
-- exactly one favorites playlist per user
CREATE UNIQUE INDEX idx_playlists_favorites ON playlists(user_id) WHERE kind = 'favorites';

-- Items reference files.id: trashed files keep their row (item renders grayed),
-- a hard delete cascades the item away. Duplicates are allowed in regular
-- playlists, hence the surrogate item id for removal/reorder addressing.
CREATE TABLE playlist_items (
  id          INTEGER PRIMARY KEY,
  playlist_id INTEGER NOT NULL REFERENCES playlists(id) ON DELETE CASCADE,
  file_id     INTEGER NOT NULL REFERENCES files(id)     ON DELETE CASCADE,
  position    INTEGER NOT NULL,
  added_at    INTEGER NOT NULL
);
CREATE INDEX idx_playlist_items_list ON playlist_items(playlist_id, position);
