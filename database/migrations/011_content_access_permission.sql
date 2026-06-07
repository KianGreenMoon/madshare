-- Roles-only access model, step 1 (docs/plans/access-roles-only.md):
-- collapse the three content permissions (content.play / content.download /
-- content.all) into a single content.access capability — "may reach the whole
-- library". content.play/download were never enforced as gates (only
-- content.all was, at the file-access bypass); there is no per-content scoping
-- left to bypass once Layer B is removed, so one permission suffices.
--
-- Layer B (access_groups / access_group_members / content_grants) and its admin
-- UI are removed in a later migration; this step only retunes the role grants so
-- the built-in roles keep full-library access under the new permission name.

DELETE FROM role_permissions
  WHERE permission IN ('content.play', 'content.download', 'content.all');

-- role ids seeded in 003_auth.sql: 1 admin, 2 moderator, 3 uploader, 4 listener.
INSERT OR IGNORE INTO role_permissions (role_id, permission) VALUES
  (1, 'content.access'),
  (2, 'content.access'),
  (3, 'content.access'),
  (4, 'content.access');
