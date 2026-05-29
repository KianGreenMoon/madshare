-- Phase 2 auth: file ownership + audit log (see docs/architecture/auth.md).

-- Track who uploaded each file. Null for pre-auth or federated files.
ALTER TABLE files ADD COLUMN uploaded_by INTEGER REFERENCES users(id);

-- Append-only record of privileged actions. actor_user_id is nullable and set
-- null if the acting user is later deleted, so history survives.
CREATE TABLE audit_log (
  id            INTEGER PRIMARY KEY,
  actor_user_id INTEGER REFERENCES users(id) ON DELETE SET NULL,
  action        TEXT    NOT NULL,
  target        TEXT,
  detail        TEXT,
  created_at    INTEGER NOT NULL
);
CREATE INDEX idx_audit_created ON audit_log(created_at);
