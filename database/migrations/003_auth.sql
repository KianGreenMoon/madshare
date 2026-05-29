-- Authentication & authorization core (see docs/architecture/auth.md).
-- Layer-A capabilities (roles/permissions) are created here alongside the
-- identity tables so the admin route group can be gated by a real permission;
-- enforcement on upload/delete/edit and the Layer-B content ACLs land later.

CREATE TABLE users (
  id                       INTEGER PRIMARY KEY,
  username                 TEXT    NOT NULL UNIQUE,
  password_hash            TEXT    NOT NULL,            -- argon2id encoded string
  password_change_required INTEGER NOT NULL DEFAULT 0,
  disabled                 INTEGER NOT NULL DEFAULT 0,
  created_at               INTEGER NOT NULL
);

CREATE TABLE sessions (
  token_hash   TEXT    NOT NULL PRIMARY KEY,            -- sha256 of cookie value
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at   INTEGER NOT NULL,
  expires_at   INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL
);
CREATE INDEX idx_sessions_user ON sessions(user_id);

CREATE TABLE api_tokens (
  id           INTEGER PRIMARY KEY,
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name         TEXT    NOT NULL,
  token_hash   TEXT    NOT NULL UNIQUE,                 -- sha256 of raw token
  created_at   INTEGER NOT NULL,
  last_used_at INTEGER,
  expires_at   INTEGER,
  revoked_at   INTEGER
);
CREATE INDEX idx_api_tokens_user ON api_tokens(user_id);

CREATE TABLE roles (
  id       INTEGER PRIMARY KEY,
  name     TEXT    NOT NULL UNIQUE,
  built_in INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE role_permissions (
  role_id    INTEGER NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
  permission TEXT    NOT NULL,
  PRIMARY KEY (role_id, permission)
);

CREATE TABLE user_roles (
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role_id INTEGER NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
  PRIMARY KEY (user_id, role_id)
);

-- Seed the built-in roles.
INSERT INTO roles (id, name, built_in) VALUES
  (1, 'admin',     1),
  (2, 'moderator', 1),
  (3, 'uploader',  1),
  (4, 'listener',  1);

-- admin: every permission.
INSERT INTO role_permissions (role_id, permission) VALUES
  (1, 'user.manage'),
  (1, 'role.manage'),
  (1, 'file.upload'),
  (1, 'file.delete'),
  (1, 'metadata.edit'),
  (1, 'library.share'),
  (1, 'federation.manage'),
  (1, 'content.play'),
  (1, 'content.download'),
  (1, 'content.all');

-- moderator: delete/edit + see-all + playback.
INSERT INTO role_permissions (role_id, permission) VALUES
  (2, 'file.delete'),
  (2, 'metadata.edit'),
  (2, 'content.play'),
  (2, 'content.download'),
  (2, 'content.all');

-- uploader: upload + playback.
INSERT INTO role_permissions (role_id, permission) VALUES
  (3, 'file.upload'),
  (3, 'content.play'),
  (3, 'content.download');

-- listener: playback only (constrained by Layer-B ACLs later).
INSERT INTO role_permissions (role_id, permission) VALUES
  (4, 'content.play'),
  (4, 'content.download');
