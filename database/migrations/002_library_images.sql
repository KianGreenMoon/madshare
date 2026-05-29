CREATE TABLE artist_images (
  artist_name TEXT    NOT NULL PRIMARY KEY,
  object_key  TEXT    NOT NULL,
  mime_type   TEXT    NOT NULL,
  updated_at  INTEGER NOT NULL
);

CREATE TABLE album_images (
  album_artist TEXT    NOT NULL,
  album_title  TEXT    NOT NULL,
  object_key   TEXT    NOT NULL,
  mime_type    TEXT    NOT NULL,
  updated_at   INTEGER NOT NULL,
  PRIMARY KEY (album_artist, album_title)
);
