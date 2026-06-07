-- Roles-only access model, step 2 (docs/plans/access-roles-only.md): remove
-- Layer B entirely. Access is now decided by role capabilities (content.access,
-- migration 011) for authenticated users and by the guest-playable / license
-- policy for anonymous requests; there are no per-content grants.
--
-- Dropped together with the code that queried these tables (the content_grants
-- branch of accessClause, the group/grant CRUD, and the admin Access UI).

DROP TABLE IF EXISTS content_grants;
DROP TABLE IF EXISTS access_group_members;
DROP TABLE IF EXISTS access_groups;
