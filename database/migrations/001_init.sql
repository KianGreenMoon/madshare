CREATE TABLE files (
  id              INTEGER PRIMARY KEY,
  hash            TEXT    NOT NULL UNIQUE,
  byte_size       INTEGER NOT NULL,
  mime_type       TEXT    NOT NULL,
  storage_backend TEXT    NOT NULL DEFAULT 'local',
  object_key      TEXT    NOT NULL,
  created_at      INTEGER NOT NULL
);
CREATE INDEX idx_files_created ON files(created_at);

CREATE TABLE file_uploads (
  id          INTEGER PRIMARY KEY,
  file_id     INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  filename    TEXT    NOT NULL,
  uploaded_at INTEGER NOT NULL,
  UNIQUE(file_id, filename)
);

CREATE TABLE media_metadata (
  file_id          INTEGER PRIMARY KEY REFERENCES files(id) ON DELETE CASCADE,
  title            TEXT,
  artist           TEXT,
  album            TEXT,
  album_artist     TEXT,
  genre            TEXT,
  year             INTEGER,
  track_number     INTEGER,
  track_total      INTEGER,
  disc_number      INTEGER,
  composer         TEXT,
  comment          TEXT,
  duration_seconds REAL,
  bitrate          INTEGER,
  sample_rate      INTEGER,
  channels         INTEGER,
  codec            TEXT,
  tag_format       TEXT,
  extracted_at     INTEGER NOT NULL
);
CREATE INDEX idx_meta_artist ON media_metadata(artist);
CREATE INDEX idx_meta_album  ON media_metadata(album);
CREATE INDEX idx_meta_title  ON media_metadata(title);
