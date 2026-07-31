-- Madnetwork page, discovery lanes — when we first saw a cached entry
-- (docs/ui/madnetwork-page.md §Settled, the "New on the network" lane).
--
-- A catalog snapshot is applied as an atomic replace (federation.md §Catalog):
-- every row for a source is deleted and re-inserted on every sync that changes
-- the serial. A naive timestamp column would therefore call the source's whole
-- library new after every single sync, which is the opposite of the fact the
-- lane wants.
--
-- So the value is carried ACROSS the replace, per (source_id, entry_key), by
-- ReplaceSourceCatalog: an entry that survives keeps the date it first arrived,
-- and only an entry_key we have never held from this source is stamped now.
-- What the column records is therefore "when this node first learned that this
-- source offers this entry" — new TO US, which is not the same as new to the
-- origin and must never be presented as it: reaching a node for the first time
-- makes its whole library new here, including records older than the project.
--
-- Existing rows get 0, meaning "we do not know" rather than "just now". The
-- lane reads a 0 as absent instead of as the epoch, so the first sync after this
-- migration dates the catalog honestly rather than announcing all of it at once.

ALTER TABLE federation_catalog ADD COLUMN first_seen INTEGER NOT NULL DEFAULT 0;

CREATE INDEX idx_fedcat_first_seen ON federation_catalog(first_seen);
