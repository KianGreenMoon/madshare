-- Remote playlist items (madnetwork-page parity phase 3,
-- docs/ui/madnetwork-page.md §Remote tracks in favorites & playlists).
--
-- A playlist item is now EITHER a local appearance (tagset_id) OR a remote
-- madnetwork track (remote_hash = the default rendition's content hash, plus
-- the display text captured at add time — the friend's catalog row may vanish
-- later). Exactly one identity per row (CHECK). When a matching blob later
-- lands approved locally, the repoint sweep (RepointRemotePlaylistItems)
-- silently turns the remote row into a normal local one.
--
-- playlist_items has no inbound FKs, so it is rebuilt in place (the 025
-- precedent).
CREATE TABLE playlist_items_new (
  id            INTEGER PRIMARY KEY,
  playlist_id   INTEGER NOT NULL REFERENCES playlists(id) ON DELETE CASCADE,
  tagset_id     INTEGER REFERENCES tagsets(id) ON DELETE CASCADE,
  remote_hash   TEXT,
  remote_title  TEXT,
  remote_artist TEXT,
  remote_album  TEXT,
  position      INTEGER NOT NULL,
  added_at      INTEGER NOT NULL,
  CHECK ((tagset_id IS NULL) <> (remote_hash IS NULL))
);

INSERT INTO playlist_items_new (id, playlist_id, tagset_id, position, added_at)
SELECT id, playlist_id, tagset_id, position, added_at FROM playlist_items;

DROP TABLE playlist_items;
ALTER TABLE playlist_items_new RENAME TO playlist_items;

CREATE INDEX idx_playlist_items_list   ON playlist_items(playlist_id, position);
-- Serves the tagset-side FK cascade and "is this appearance in a playlist".
CREATE INDEX idx_playlist_items_tagset ON playlist_items(tagset_id);
-- Serves the repoint sweep's hash lookup.
CREATE INDEX idx_playlist_items_remote ON playlist_items(remote_hash)
  WHERE remote_hash IS NOT NULL;
