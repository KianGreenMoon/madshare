-- "listener" means may listen to the whole library. Grant the listener and
-- uploader built-in roles content.all so an authenticated user holding either
-- can browse, play and download every file (the same Layer-B bypass admins and
-- moderators already have). Anonymous (not-logged-in) users are unaffected and
-- remain default-deny — they still see only guest-playable / free-licensed
-- files. Access groups + content grants stay available to constrain custom
-- roles that deliberately lack content.all.
--
-- role ids are seeded in 003_auth.sql: 3 = uploader, 4 = listener.
INSERT OR IGNORE INTO role_permissions (role_id, permission) VALUES
  (3, 'content.all'),
  (4, 'content.all');
