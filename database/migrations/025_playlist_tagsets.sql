-- Playlists & favorites reference the tagset (recording-tagsets P1,
-- decision 4): a playlist item names the specific *appearance* the user
-- picked, not the blob — an absorbed appearance has no blob of its own, and
-- the serving rendition is resolved per play (recording → ladder best).
-- Existing rows migrate 1:1 through the file's offered tagset; an item whose
-- file somehow lacks a tagset (invariant violation) is dropped rather than
-- carried as an unplayable ghost.
--
-- playlist_items has no inbound FKs, so it is rebuilt in place (the 016
-- precedent).
CREATE TABLE playlist_items_new (
  id          INTEGER PRIMARY KEY,
  playlist_id INTEGER NOT NULL REFERENCES playlists(id) ON DELETE CASCADE,
  tagset_id   INTEGER NOT NULL REFERENCES tagsets(id)   ON DELETE CASCADE,
  position    INTEGER NOT NULL,
  added_at    INTEGER NOT NULL
);

INSERT INTO playlist_items_new (id, playlist_id, tagset_id, position, added_at)
SELECT i.id, i.playlist_id,
       (SELECT MIN(t.id) FROM tagsets t WHERE t.origin_file_id = i.file_id),
       i.position, i.added_at
FROM playlist_items i
WHERE EXISTS (SELECT 1 FROM tagsets t WHERE t.origin_file_id = i.file_id);

DROP TABLE playlist_items;
ALTER TABLE playlist_items_new RENAME TO playlist_items;

CREATE INDEX idx_playlist_items_list   ON playlist_items(playlist_id, position);
-- Serves the tagset-side FK cascade and "is this appearance in a playlist".
CREATE INDEX idx_playlist_items_tagset ON playlist_items(tagset_id);
