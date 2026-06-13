-- Split the two artist roles on media_metadata. Until now media_metadata.artist_id
-- held the *album* artist (the album_artist -> artist grouping). We rename it to
-- album_artist_id and add a new artist_id for the track's *performer*, resolved
-- from the raw `artist` tag (falling back to album_artist, then the Unknown
-- bucket). Both columns FK into the shared artists table; for a normal
-- single-artist release the two ids are the same entity, so no extra rows appear.
-- Design: docs/architecture/artist-album-model.md ("Two artist roles, one entity
-- table").
--
-- The rename is a real column rename (the physical column name changes in the DB),
-- not a query-time alias. The new artist_id starts NULL and is populated by the
-- Go-side resolver: new uploads inline, pre-existing rows by the startup
-- BackfillEntities pass.

ALTER TABLE media_metadata RENAME COLUMN artist_id TO album_artist_id;

-- RENAME COLUMN rewrites the old index's definition onto album_artist_id but keeps
-- its name (idx_meta_artist_id). Drop it and recreate under a name that matches the
-- column it now covers, then index the new performer column.
DROP INDEX IF EXISTS idx_meta_artist_id;
CREATE INDEX idx_meta_album_artist_id ON media_metadata(album_artist_id);

ALTER TABLE media_metadata ADD COLUMN artist_id INTEGER REFERENCES artists(id);
CREATE INDEX idx_meta_artist_id ON media_metadata(artist_id);
