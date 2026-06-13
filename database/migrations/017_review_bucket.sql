-- Moderation review bucket (docs/architecture/moderation.md).
--
-- Uploads now stage before they reach the library: files carry a review_state
-- (draft -> submitted -> approved, with returned for "fix and resubmit").
-- Existing rows backfill to 'approved' via the column default, so a migrated
-- library is unchanged. review_state is orthogonal to deleted_at: discarding a
-- submission soft-deletes it (Trash) with its state intact, so a Trash restore
-- re-enters the queue rather than the library.

ALTER TABLE files ADD COLUMN review_state TEXT NOT NULL DEFAULT 'approved'
  CHECK (review_state IN ('draft','submitted','returned','approved'));
ALTER TABLE files ADD COLUMN review_note TEXT;
ALTER TABLE files ADD COLUMN submitted_at INTEGER;

-- The pending set is the rare case; listings filter on = 'approved' which the
-- partial index serves by exclusion (and the moderation queue scans it).
CREATE INDEX idx_files_review ON files(review_state)
  WHERE review_state <> 'approved';

-- New capability: act on submissions (approve / return / discard). Holders'
-- own submits self-approve. Role ids seeded in 003_auth.sql: 1 admin,
-- 2 moderator. There is deliberately no permission wildcard (Identity.Has is
-- a plain map lookup), so admin must be granted explicitly like always.
INSERT OR IGNORE INTO role_permissions (role_id, permission) VALUES
  (1, 'content.moderate'),
  (2, 'content.moderate');

-- Moderators are the trusted uploaders (owner decision 2026-06-11): give the
-- built-in moderator role upload capability, which it lacked.
INSERT OR IGNORE INTO role_permissions (role_id, permission) VALUES
  (2, 'file.upload');
