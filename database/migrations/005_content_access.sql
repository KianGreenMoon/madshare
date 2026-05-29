-- Phase 3 auth: Layer-B content access (see docs/architecture/auth.md §5).

-- Per-file access metadata.
--   guest_playable: may the anonymous public stream/download this file? default deny.
--   license:        controlled-vocabulary metadata (CC0-1.0, public-domain, ...),
--                   distinct from guest_playable; may drive an opt-in auto-publish policy.
ALTER TABLE files ADD COLUMN guest_playable INTEGER NOT NULL DEFAULT 0;
ALTER TABLE files ADD COLUMN license TEXT;

-- A named set of users granted access to a set of content scopes.
CREATE TABLE access_groups (
  id   INTEGER PRIMARY KEY,
  name TEXT    NOT NULL UNIQUE
);

CREATE TABLE access_group_members (
  group_id INTEGER NOT NULL REFERENCES access_groups(id) ON DELETE CASCADE,
  user_id  INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  PRIMARY KEY (group_id, user_id)
);

-- Grants attach a content scope to a group. scope_type:
--   all    -> whole library (scope_* columns ignored)
--   artist -> scope_artist matches the file's effective artist (album_artist|artist)
--   album  -> scope_artist + scope_album match the file's album
--   file   -> scope_file_id targets one file
CREATE TABLE content_grants (
  id            INTEGER PRIMARY KEY,
  group_id      INTEGER NOT NULL REFERENCES access_groups(id) ON DELETE CASCADE,
  scope_type    TEXT    NOT NULL,
  scope_artist  TEXT,
  scope_album   TEXT,
  scope_file_id INTEGER REFERENCES files(id) ON DELETE CASCADE,
  created_at    INTEGER NOT NULL
);
CREATE INDEX idx_content_grants_group ON content_grants(group_id);
