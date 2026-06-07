# Admin Panel Rework

Status: **planned** (not started)
Branch: aidev

## Why

The admin panel is one monolithic page doing seven unrelated jobs at once:

- `webui/html/admin.html` — 207 lines, 7 stacked `<section>`s.
- `webui/static/js/admin.js` — **1646 lines**, one module wiring all of them.
- `webui/static/css/admin.css` — 896 lines.

Everything loads on every visit, the file is too big to reason about, and the
seven concerns (upload, prune, files, trash, users, access groups, auto-publish)
have nothing to do with one another. We are rewriting it as **separate routed
pages**, one per function, all sharing the existing header bar and auth layer.

This is a pure front-end / routing rework. **No data-model or business-logic
change.** One endpoint we thought we'd need (metadata edit) already exists.

## Goals

1. Split the panel into discrete pages, one per function (decided: separate
   routes, not client-side tabs).
2. Reuse the already-factored header partial (`{{define "header"}}`) and
   `auth.js` (`initAuth` / `gatePage` / `PAGE_PERMS`) unchanged.
3. Preserve **all** current functionality (nothing regresses).
4. Add per-file **metadata editing** (title / artist / album / album-artist) to
   the Files page — the backend endpoint already exists, only UI is missing.
5. Smaller, single-purpose JS modules + per-page CSS; extract shared helpers.
6. **Reuse existing CSS and JS wherever it's meaningful** rather than rewriting.
   In particular, extract a **shared, reusable audio player** (today's player
   logic is duplicated and divergent across the library and cmus pages) and use
   it on the Files / music-editing page so an admin can preview a track while
   editing it.

## Guiding principle: reuse, don't reinvent

Before writing new markup/styles/scripts for the admin pages, prefer the assets
that already exist:

- **CSS** — `app.css` carries the design tokens (`:root[data-theme]`), header /
  nav / theme-switcher, and shared button / modal / table styling. Admin pages
  should pull these in (as `upload.html` already does) and add only what's
  genuinely admin-specific on top. Don't re-declare tokens or re-style buttons.
- **JS** — `auth.js` (`initAuth`, `gatePage`, `PAGE_PERMS`, theme handling) is
  already shared. Helpers currently duplicated inside `admin.js` (`toast`,
  `fmtBytes`, `fmtDate`, `fmtTime`, `el`, `handleAuthError`) move into a shared
  module instead of being copied per page.
- **Player** — see "Shared audio player" below; this is the marquee reuse target.

## Non-goals

- No backend behavior change beyond what's already shipped. (Metadata-edit
  endpoint `PATCH /api/files/{hash}/metadata` is already live and gated on
  `metadata.edit`.)
- No new permissions or roles.
- Cover-image editing is **out of scope** for this rework (deferred; see Open
  questions).
- No federation work.

---

## Current functionality inventory (must all survive)

Sourced from `admin.html` sections + `admin.js` function map.

| Area | Today's controls | API endpoints | Gating |
|------|------------------|---------------|--------|
| Upload | drop zone, queue, concurrency=3, XHR progress, dedupe result | `POST /files/upload` | `file.upload` |
| Verify & Prune | deep-verify checkbox, preview (non-destructive), confirm modal, commit | `GET/POST /api/admin/prune` | admin (file.delete) |
| Files | list, filter, per-file guest toggle, license dropdown, inline 2-step delete | `GET /api/files`, `POST /api/admin/files/{hash}/guest`, `.../license`, `DELETE /api/admin/files/{hash}` | admin; access controls need `user.manage` |
| Trash | list, restore, delete-forever (2-step) | `GET /api/admin/trash`, `POST .../trash/{id}/restore` (verify exact paths), `DELETE .../trash/{id}` | admin |
| Users | create (name/pass/role/force-change), list, edit roles, reset password, disable, delete | `GET /api/admin/roles`, `GET/POST /api/admin/users`, `PATCH/DELETE /api/admin/users/{id}`, `POST .../users/{id}/password` | `user.manage` |
| Access groups | create group, list, add/remove members, add/delete grants (scopes) | `GET/POST /api/admin/access/groups`, `.../groups/{id}` (members), `.../grants/{id}` | `user.manage` |
| Auto-publish | enable toggle, free-license allow-list, save | `GET/POST /api/admin/settings/autoderive` | `user.manage` |

Permission gating today lives in `applyPermissions()` (`admin.js:22`), which
hides sections by `aria-labelledby`. In the new layout this becomes per-page
`gatePage(...)` plus nav-link hiding.

**New for Files:** metadata edit via `PATCH /api/files/{hash}/metadata`
(fields: `title`, `album`, `album_artist`, `artist`; pointer semantics — absent
key = unchanged, `""` = clear), gated on `metadata.edit`. `canEditMeta` is
already computed in `applyPermissions` but wired to nothing today. Plus a
**per-track preview player** (the shared `player.js`) so an admin can listen
while editing.

---

## Target architecture

### Routes

`/admin` becomes a lightweight **dashboard / landing** with cards linking to each
sub-page and showing at-a-glance counts (files, trash, users). Each function
gets its own route, template, and JS module:

```
/admin            → dashboard (landing, links + counts)
/admin/files      → files list + per-file access/license/metadata edit + delete
/admin/users      → user CRUD
/admin/access     → access groups + grants
/admin/prune      → verify & prune (danger zone)
/admin/settings   → auto-publish policy
/admin/trash      → trash bucket
```

**Upload: reuse the existing `/upload` page (decided).** A complete standalone
upload page already exists in the webui group (`upload.html` + `upload.css` +
its own JS, with the shared header/nav). There is **no `/admin/upload` route**
and **no admin upload section** — the secondary admin nav simply links to
`/upload`. This kills the current duplication (the old admin Upload section and
`admin.js`'s entire upload block — drop zone, queue, XHR progress — go away;
`/upload` is the single source). One caveat: `/upload` gates on `file.upload`,
which an admin normally has; if an admin lacks it, the link can be hidden the
same way other nav links are gated.

### Routing changes (`webui/webui.go`)

`RegisterAdminPage` currently mounts a single `/admin`. It becomes a loop/series
of `r.Get` registrations, one per template, all in the **admin route group** (so
they stay co-located with `/api/admin/*` and behind `auth.RequirePermission`).
Add the new templates to the `buildPageTmpl` block. `pageData.Page` gains values
for sub-nav active-state (e.g. `"admin"`, `"admin-files"`, …) — or we add a
second field `SubPage`; decide during impl.

The chi router already gates the whole admin group via
`auth.RequirePermission(file.delete)` (see `madshare.go` / `buildHandler`), so
all sub-routes inherit baseline admin protection server-side. Finer gates
(`user.manage`, `metadata.edit`) remain client-side page guards + server-side
endpoint checks (already enforced).

### Shared "admin shell"

A new template partial `{{define "admin-shell"}}` (in `partials.html` or a new
`admin_partials.html`) provides:

- the existing site `{{template "header" .}}`,
- the persistent admin banner (currently inline in admin.html),
- a **secondary admin nav** (the per-function menu the user asked for) with
  active-state from `pageData`,
- a content slot each page fills.

Pages stay thin: shell + their one section + `{{template "auth-modals" .}}` +
their script.

### JS module layout

Extract shared helpers (currently duplicated/buried in admin.js) into a small
module set so each page imports only what it needs:

```
webui/static/js/admin/
  shared.js     → fmtBytes, shortHash, fmtDate, toast, el(), handleAuthError,
                  isAudioFile, API const, license constants
  files.js      → files table, filter, access controls, metadata edit, delete;
                  uses the shared player.js for per-row preview playback
  users.js      → user CRUD
  access.js     → groups + grants
  prune.js      → verify & prune + confirm modal
  settings.js   → auto-publish policy
  trash.js      → trash bucket
  dashboard.js  → landing counts
```

Theme handling already lives in `auth.js`/shared pattern — keep one copy. Each
page's boot does: `initAuth()` → `gatePage(PAGE_PERMS.admin)` → page-specific
permission check (e.g. bail/redirect if `!user.manage` on /admin/users) → load.

### Shared audio player

**Problem today.** Player logic is implemented twice and divergently:

- `library.html` + `app.js` (`#player-bar`, IDs like `playerTitle`/`btnPlay`,
  styled in `app.css`). The logic (`app.js:394+`) is tightly coupled to the
  library's `playlist` array and `.track-row` DOM (it sets `.playing`, writes
  durations back into rows, does shuffle/next/prev over the playlist).
- `cmus.html` + `cmus.js` (`.library-player` footer, different IDs like
  `player-title`, styled in `cmus.css`).

Neither is droppable into a new page as-is. The music-editing (Files) page needs
**single-track preview** — load one file, play/pause/seek/volume — not a playlist
engine.

**Plan: extract a self-contained player component.**

```
webui/static/js/player.js   → Player core: owns an <audio>, exposes
                              load({url,title,artist,art?}), play/pause/toggle,
                              seek, volume; renders into a given container.
                              Decoupled from playlists — emits events
                              ('play','pause','ended','error','timeupdate') the
                              caller subscribes to. No .track-row / playlist deps.
webui/static/css/player.css → the player-bar styling, lifted from app.css/cmus.css
                              into one shared sheet keyed off the design tokens.
{{define "player-bar"}}     → the player markup partial (one set of IDs/classes),
                              included by any page that wants a player.
```

The library's playlist concerns (next/prev/shuffle, row highlighting, duration
write-back) stay in `app.js` as a **thin caller** layered on top of the Player
core via its events — they are library-specific, not player-specific.

**Scope (decided — Decisions §4):** build `player.js`, then migrate **all three**
consumers onto it in this rework — the admin Files page, the library page, and
the cmus page. One player everywhere. The library/cmus migrations must preserve
their current behavior exactly (playlist next/prev/shuffle, `.track-row`
highlighting, duration write-back, cmus's art/scrubber/mute) by keeping those
concerns in the page's thin caller and driving them off the Player core's events.
Because this touches the working library player, treat the migration as its own
phase with focused regression testing (see Phasing + Testing).

### CSS

Split `admin.css` (896 lines) into a shared sheet plus a few page-specific files
(decided — see Decisions §3):

- `admin-shell.css` — header overrides, admin banner, secondary nav, and the
  shared table / button / modal styles used across pages.
- a small per-page file **only** where a page has unique styling — e.g.
  `admin-files.css` (table specifics), `admin-prune.css` (danger zone). The old
  drop-zone styles go away with the admin upload section.

---

## Phasing

Each phase leaves the app working (old page stays until its functions are ported,
or we port behind the new routes and flip `/admin` last).

1. **Scaffold** — admin shell partial + secondary nav; new `webui.go` routes
   returning placeholder pages; `shared.js` extracted with the common helpers
   and a unit-smoke that pages import cleanly. Old `/admin` untouched.
2. **Shared player** — extract `player.js` + `player.css` + `{{define
   "player-bar"}}` as a self-contained component (decoupled from playlists), then
   **migrate library and cmus onto it** (their playlist/highlight/duration and
   art/scrubber/mute concerns become thin callers over the Player core's events).
   Regression-test both pages to parity before moving on. Decisions §4.
3. **Files page** (`/admin/files`) — port list/filter/access/license/delete,
   **add metadata edit UI** (inline edit form per row or a row-expand/modal
   calling `PATCH /api/files/{hash}/metadata`), and wire the shared player for
   per-track preview while editing.
4. **Users + Access** (`/admin/users`, `/admin/access`) — port, gated on
   `user.manage`.
5. **Prune + Trash** (`/admin/prune`, `/admin/trash`) — port danger-zone +
   confirm modal + trash restore/hard-delete.
6. **Settings** (`/admin/settings`) — port auto-publish. (Upload is not ported —
   the admin nav links to the existing `/upload` page.)
7. **Dashboard + cutover** — `/admin` becomes the landing; delete the old
   monolithic `admin.html` / `admin.js` / dead CSS. Update header nav if needed.

---

## File-by-file change list

**New**
- `webui/html/admin/` — `dashboard.html`, `files.html`, `users.html`,
  `access.html`, `prune.html`, `settings.html`, `trash.html` (subdir, decided —
  see Decisions §2). No admin upload template — the nav links to `/upload`.
- `webui/static/js/admin/{shared,files,users,access,prune,settings,trash,dashboard}.js`
- `webui/static/css/admin-shell.css` + the few page sheets (`admin-files.css`,
  `admin-prune.css`).
- `webui/static/js/player.js`, `webui/static/css/player.css`, and a
  `{{define "player-bar"}}` partial — the shared, reusable player extracted from
  the library/cmus implementations (not admin-namespaced; it's app-wide).
- `{{define "admin-shell"}}` partial.

**Changed**
- `webui/webui.go` — multiple admin routes; new templates in build block; the
  embed pattern gains `html/admin/*.html` (current `//go:embed html/*.html` is
  flat and won't pick up the subdir); `buildPageTmpl` paths use `html/admin/…`;
  `pageData` sub-page field.
- `partials.html` — admin shell partial + secondary nav (if not a new file).
- `webui/html/library.html` + `webui/static/js/app.js` — migrate onto the shared
  `player.js` / `player-bar` partial; library-specific playlist/highlight/
  duration logic stays as a thin caller over Player events. Player styles move
  out of `app.css` into `player.css`.
- `webui/html/cmus.html` + `webui/static/js/cmus.js` — same migration; cmus's
  art / scrubber / mute concerns stay caller-side. Player styles move out of
  `cmus.css` into `player.css`.

**Deleted (phase 6)**
- `webui/html/admin.html`, `webui/static/js/admin.js`, superseded CSS.

**Backend: none required.** (Confirm `metadata.edit` is granted to the admin
role so the new edit UI is usable by admins; it should be, but verify.)

---

## Testing

- Go: `go build ./...` and `go test ./...` — the embed pattern now includes
  `html/admin/*.html`, so confirm the templates parse and any webui test passes.
- Manual browser checklist per page (parity with current behavior): prune
  preview/commit (deep + shallow), file delete (2-step), guest + license
  toggles, **metadata edit + reload persists**, trash restore + hard-delete,
  user create/edit/reset/disable/delete, group create + member + grant
  add/delete, auto-publish save, and the nav's **Upload link reaches `/upload`**.
- Player: on the Files page, preview a track (play/pause/seek/volume) independent
  of any playlist. Then full regression on **library** (playlist next/prev/
  shuffle, row highlighting, duration write-back, search-mode playback) and
  **cmus** (art, scrubber, mute/volume) after the migration — these must match
  pre-migration behavior exactly.
- Permission matrix: visit each route as admin / `user.manage`-less admin /
  uploader / listener / anonymous — correct gate (page guard + server 401/403),
  and the secondary nav hides links the identity can't use.
- Remember: webui assets are **compile-time embedded** — rebuild/restart to see
  changes (per local-verify notes).

## Decisions

All open questions are settled:

1. **Cover-image editing — deferred.** Metadata edit covers title / artist /
   album / album-artist via the existing `PATCH /api/files/{hash}/metadata`.
   Cover re-upload is out of scope. Because changing album/artist re-keys covers
   (see `updateFileMetadata` doc comment + upload-and-covers §5d), the Files page
   shows an inline note that renaming an album/artist may orphan the existing
   cover.
2. **Templates — subdir `webui/html/admin/`.** Cleaner grouping; costs one embed
   pattern line (`//go:embed html/*.html` → also `html/admin/*.html`) and
   subdir paths in `buildPageTmpl`.
3. **CSS — shared `admin-shell.css` + a few page sheets.** Page-specific CSS only
   where a page genuinely needs it (`admin-files.css`, `admin-prune.css`).
4. **Shared player retrofit — do it in this rework.** Build `player.js` /
   `player.css` / `player-bar` partial and migrate **all three** consumers onto
   it: admin Files, library, and cmus. Library/cmus migrations must preserve
   current behavior exactly (playlist next/prev/shuffle, `.track-row` highlight,
   duration write-back; cmus art/scrubber/mute) by keeping those concerns in each
   page's thin caller over Player events. This is its own phase with focused
   regression testing because it touches the working library player.
