# User settings page

A per-user **Settings** page in the listening shell that gathers the account-level
controls that were previously scattered across the header (or did not exist at
all): **change password**, **API token** management, and **theme**. It is reached
from a new right-side header entry and is available to any signed-in user — there
is no extra permission, every account manages its own settings.

This doc is the design of record. Related: `docs/ui/shells.md` (the listening
shell + client router), `docs/architecture/auth.md` (password / token model),
`docs/ui/toast.md` (the shared toast system this page reuses).

## Goals

- One place for the things a signed-in user changes about *their own* account.
- Move **change password** out of the header into the page (the header button goes
  away; the bootstrap *forced* change keeps its modal — see below).
- Give API tokens a real UI: list, create with an **optional expiry date**, and
  revoke. The raw token is shown exactly once.
- Move **theme** selection here and remove the header theme dots. Fix the theme
  FOUC (flash) while we are at it with an inline `<head>` guard.

## Non-goals

- No server-side per-account settings store. Theme stays **per-device** in
  `localStorage` (`madshare-theme`), as today. Server-side theme persistence is a
  possible future addition, noted at the end.
- No new permission. The page is gated only on "is signed in"; anonymous visitors
  never see the entry point and the page renders a sign-in prompt if reached
  directly.
- Admin-specific settings stay on `/admin/settings` (license / guest / access
  groups). This page is the *user's* settings, not the server's.

## Decisions

1. **It is a page, not a modal.** A shell-native page at `/settings`, registered
   like `/playlists` (`pageData{Page: "settings"}`, `data-module=/static/js/settings.js`).
   It is **not** a header *section* (it does not join the Library subtab family);
   it is reached from the right-side header area. Playback and the shared queue
   survive navigation to it, like every listening-shell page.
2. **Entry point: right side of the header.** When signed in, the header's
   `.header-actions` user area shows **`username · ⚙ Settings · Sign out`**. The
   old **Change password** button is removed; **Settings** takes its slot. `Sign
   out` and the username stay in the header (no extra clicks to log out).
3. **Token expiry is an optional date.** The create form has an
   `<input type="date">` (blank = never expires). The client sends an absolute
   `expires_at` (unix seconds); the create handler is extended to accept it (the
   table and `CreateToken` already store an absolute timestamp — no migration).
   `expires_in_days` is kept for non-browser API clients.
4. **Theme moves here; header dots are removed.** The `.theme-switcher` dots leave
   the shared header partial. Theme is changed only on this page. Because the dots
   live in the *shared* header, this also removes them from admin pages (see
   "Admin shell" below).
5. **No FOUC.** A tiny inline `<head>` script applies the saved theme before first
   paint on every shell page. This is the long-deferred fix that was bundled with
   "when the user-settings page is built".
6. **The forced first-run password change keeps its modal.** Bootstrap forces a
   password change before the user can do anything; that path must not depend on
   the user navigating to a page. The existing forced modal (auto-opened from
   `password_change_required`) stays. Only the *voluntary* change moves to the
   page.

## Entry point & header changes

`webui/html/partials.html`, `{{define "header"}}`, the `.header-actions` block:

- **Remove** `#changePassBtn` (the voluntary "Change password" button).
- **Remove** the `.theme-switcher` dot group entirely.
- **Add** a Settings entry inside the existing `#userArea` (which is already shown
  only when signed in), between the username and Sign out:

  ```html
  <div class="user-area" id="userArea" hidden>
    <span class="user-name" id="userName"></span>
    <a href="/settings" class="btn btn-neutral" id="settingsLink"
       aria-label="Settings"><span aria-hidden="true">⚙</span> Settings</a>
    <button class="btn btn-neutral" id="logoutBtn">Log out</button>
  </div>
  ```

  It is an `<a href="/settings">` so the shell router intercepts it as a normal
  in-shell navigation (and middle-click / ctrl-click open a new tab naturally). No
  JS wiring is needed for the link itself; `applyNavPermissions` does not touch it
  (every signed-in user may open it). It lives inside `#userArea`, which `auth.js`
  already toggles, so it appears exactly when the user is signed in.

The change-password **modal markup** (`#passModal` in `{{define "auth-modals"}}`)
stays — it is still used by the forced first-run flow.

## Route & shell wiring

Server (`webui/webui.go`):

```go
// in Register():
r.Get("/settings", makeHandler(settingsTmpl, "settings.html",
    pageData{APIURL: apiBase, Page: "settings", GitRepo: gitRepo}))
```

`settings.html` is a new template parsed alongside the other listening-shell pages
(mirror how `playlistsTmpl` is built). No `Section` — Settings is not part of the
Library section, so no header tab lights up for it (`setActiveNav` simply matches
nothing, which is correct).

Page shell (`webui/html/settings.html`), modeled on `playlists.html`:

```html
<body data-page="settings" data-module="/static/js/settings.js">
{{template "header" .}}
<main> … the three sections … </main>
{{template "player-bar" .}}
{{template "auth-modals" .}}
<script type="module" src="/static/js/shell.js"></script>
</body>
```

Client gate (`webui/static/js/settings.js`, `init()`):

- `getIdentity()` from `auth.js`. If `null`, render the same "Sign in required"
  notice the privileged pages use (`gatePage` renders it; or reuse
  `renderAccessDenied`). There is no permission to check — being signed in is the
  whole gate, so a thin inline check is fine:
  ```js
  if (!getIdentity()) { /* render sign-in-required panel */ return; }
  ```
- Otherwise fetch the token list and render the three sections.

`teardown()` is a no-op (no timers/listeners outside `<main>`).

## Page layout

A single `<main>` with three stacked `<section>`s, each a card. Reuse `app.css`
form/card/button styles; add a small `settings.css` only for page-specific spacing.

```
Settings
├─ Account
│   └─ Change password (current / new / confirm)  → POST /api/auth/password
├─ API tokens
│   ├─ Create token (name + optional expiry date) → POST /api/auth/tokens
│   ├─ One-time reveal of the new raw token (copy button)
│   └─ List: name · created · last used · expires · [Revoke]
└─ Appearance
    └─ Theme: Dark / Light / Ocean / Sunset
```

### Account — change password

Reuse the existing endpoint and validation; just relocate the form into the page.

- Form fields: current / new / confirm (same `minlength=8`, same client checks for
  match + length that `auth.js` does today).
- `POST /api/auth/password` with `{ old_password, new_password }`.
- On success: clear the form, `showToast('Password changed.', { type: 'success' })`.
- On `401`: "Current password is incorrect." On other errors: surface the body.
- The change-password logic currently inside `auth.js`'s `initAuth` is the
  reference; factor the voluntary path into `settings.js`. Keep `auth.js`'s
  **forced** path (`if (_identity?.password_change_required) openPassModal(true)`)
  — that modal is the bootstrap gate and must keep working independent of this page.

### API tokens

**List** — `GET /api/auth/tokens` returns `[{id, name, created_at, last_used,
expires_at, revoked}]`. Render a table; format `created_at` / `last_used` /
`expires_at` (unix seconds, or `null`) as locale dates; show "Never" for a null
expiry, "—" for never-used. A `revoked` row is shown struck-through / disabled
(or filtered — see open questions). Each active row has a **Revoke** button →
`DELETE /api/auth/tokens/{id}` (204), then re-list.

**Create** — a small form:

```html
<form id="tokenForm">
  <input type="text"  id="tokenName" maxlength="100" required placeholder="Token name">
  <input type="date"  id="tokenExpiry">           <!-- optional; blank = never -->
  <button type="submit">Create token</button>
</form>
```

- Validate `name` non-empty (server also enforces).
- If a date is set, convert it to an absolute unix timestamp at **end of that day,
  local time** (so "expires 2026-12-31" is valid through that whole day), and post
  `{ name, expires_at }`. Blank date → omit `expires_at` → never expires.
- Reject a past date client-side (the server also rejects it — see backend change).

**One-time reveal** — the create response is `{ id, name, token }`. Show `token`
once in a highlighted box with a **Copy** button and a "you won't see this again"
note; it is never returned by the list endpoint. After the user dismisses the
reveal, refresh the list (the new row appears without its secret).

### Appearance — theme

A radio group (or a labeled dot row) for the four themes. The shared apply/persist
logic moves into a tiny module so the page, the shell, and the no-FOUC guard agree:

```js
// webui/static/js/theme.js
export const VALID_THEMES = new Set(['dark', 'light', 'ocean', 'sunset']);
export function applyTheme(name) {
  if (!VALID_THEMES.has(name)) name = 'dark';
  document.documentElement.dataset.theme = name;
  localStorage.setItem('madshare-theme', name);
}
export function currentTheme() {
  return localStorage.getItem('madshare-theme') || 'dark';
}
```

- `settings.js` renders the control reflecting `currentTheme()` and calls
  `applyTheme(picked)` on change — the change is live and global (it sets the
  `<html data-theme>` the persistent shell already styles against).
- `shell.js`'s `wireTheme()` loses its dot-wiring (no dots in the header anymore);
  it can just `applyTheme(currentTheme())` on boot (idempotent with the inline
  guard) or be dropped in favor of the guard. `admin/shared.js`'s `initTheme`
  likewise drops dot-wiring.

### No-FOUC guard

Add to the `<head>` of the shell page templates (and admin pages), **before** the
stylesheets, an inline classic script (not a module — modules are deferred and run
too late):

```html
<script>
  try {
    var t = localStorage.getItem('madshare-theme');
    if (t) document.documentElement.dataset.theme = t;
  } catch (e) {}
</script>
```

This sets `data-theme` before first paint, eliminating the dark→saved-theme flash.
Because every page already hard-codes `<html ... data-theme="dark">`, the guard
only needs to *override* when a saved theme differs. (A shared head partial would
be the clean home for this, but a copied 4-line snippet per template is acceptable.)

## Backend change — accept `expires_at`

`api/auth_handlers.go`, `createToken`. The store already takes an absolute
timestamp; only the HTTP handler needs the new field.

```go
var req struct {
    Name          string `json:"name"`
    ExpiresAt     int64  `json:"expires_at"`      // NEW: absolute unix seconds, 0 = none
    ExpiresInDays int    `json:"expires_in_days"` // kept for API clients
}
...
var expires int64
switch {
case req.ExpiresAt > 0:
    if req.ExpiresAt <= time.Now().Unix() {
        http.Error(w, "expiry must be in the future", http.StatusBadRequest)
        return
    }
    expires = req.ExpiresAt
case req.ExpiresInDays > 0:
    expires = time.Now().Add(time.Duration(req.ExpiresInDays) * 24 * time.Hour).Unix()
}
```

`expires_at` wins when both are sent. No migration (the `api_tokens.expires_at`
column and `CreateToken(..., expiresAt int64)` already exist). Update
`docs/architecture/auth.md`'s token-endpoint note and the `fakeRepo` /
`auth_handlers_test.go` if a new case is asserted.

## Admin shell

The theme dots live in the **shared** `{{define "header"}}` partial, which every
admin page also renders. Removing the dots therefore removes them from admin too,
and `admin/shared.js`'s `initTheme` no longer has dots to wire. Admins are signed-in
users, so they change theme via `/settings` like everyone else (a hard navigation
out of the admin shell, the same as clicking Library). The no-FOUC guard should be
added to the admin templates as well so admin pages also apply the saved theme
before paint. This keeps theme single-sourced (`localStorage` + `theme.js`) instead
of split between the listening header and the admin header.

## Files touched

| File | Change |
| --- | --- |
| `webui/html/partials.html` | Header: drop `#changePassBtn` + `.theme-switcher`; add `#settingsLink` in `#userArea`. Keep `#passModal`. |
| `webui/html/settings.html` | **New** shell page (three sections). |
| `webui/webui.go` | Parse `settingsTmpl`; `r.Get("/settings", …)` in `Register`. |
| `webui/static/js/settings.js` | **New** page module: gate, password form, token list/create/revoke, theme control. |
| `webui/static/js/theme.js` | **New** shared `applyTheme`/`currentTheme`. |
| `webui/static/js/shell.js` | `wireTheme` → just apply saved theme; import from `theme.js`. |
| `webui/static/js/admin/shared.js` | `initTheme` drops dot-wiring; use `theme.js`. |
| `webui/static/js/auth.js` | Drop voluntary change-pass wiring (`#changePassBtn`); **keep** the forced-modal path. |
| `webui/static/css/settings.css` | **New** (minimal page spacing). |
| Shell + admin `*.html` `<head>` | Inline no-FOUC theme guard. |
| `api/auth_handlers.go` | `createToken` accepts `expires_at`; validates future. |
| `docs/architecture/auth.md` | Note `expires_at` on the token-create endpoint. |
| `webui/webui_test.go` | Settings page renders (mirror the admin-subpage render test if useful). |

## Open questions / future

- **Revoked tokens in the list:** show struck-through, or hide them? Default:
  show, struck-through, no Revoke button (so the user sees history). Easy to flip.
- **Server-side theme persistence:** today theme is per-device `localStorage`. If
  we later want theme to follow the account across devices, add a `theme` column to
  a per-user settings store and seed `localStorage` from `/api/auth/me`. Out of
  scope here; the `theme.js` seam makes it a localized change.
- **Token "copy" affordance:** `navigator.clipboard.writeText`; fall back to
  select-all if unavailable (non-HTTPS origins lack the Clipboard API — relevant on
  the plain-HTTP `.ygg` deployment).
