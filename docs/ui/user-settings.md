# User settings page

`/settings` gathers the account-level controls a signed-in person changes about
**their own** account: password, API tokens, theme. It is a shell-native page in
the listening shell, available to any signed-in user — there is **no permission**
on it, because every account manages its own settings.

Server settings are a different page. `/admin/settings` holds what the *node*
does (licence defaults, guest access, madnetwork policy, swarm limits); this page
holds nothing that affects anybody else.

Related: `docs/ui/shells.md` (the shell it lives in), `docs/api/tokens.md` (the
token API), `docs/architecture/auth.md` (the auth model), `docs/ui/clipboard.md`
(why the copy button works the way it does).

## Entry point

**The username in the header is the link.** When signed in, the right-hand
`.header-actions` area is `username · Log out`, where the username is an `<a
href="/settings">` styled as a `.nav-link` — the same underline tab as the
left-side Library/Madnetwork tabs, on the right, underlined when the page is
open. There is no separate Settings button, and there is no header
change-password button or theme switcher: both moved here.

The header's auth state is server-rendered, so a signed-in load paints the
username straight away rather than swapping it in after `/api/auth/me`
(`docs/ui/shells.md` §"Header auth state is server-rendered").

Settings is **not** a header *section* — it joins no subtab family, and
`setActiveNav` lights nothing in the left-hand nav for it.

## The page

Three panels behind a `.subtabs` bar, one visible at a time, defaulting to
**Account**. The bar follows the ARIA tablist keyboard pattern (arrows / Home /
End, roving `tabindex`), and the tab is the panel's label, so the cards carry no
heading of their own.

```
Settings
[ Account | API tokens | Appearance ]
├─ Account      → change password
├─ API tokens   → create (optional expiry) · one-time reveal · list · revoke
└─ Appearance   → theme
```

A visitor who reaches `/settings` without a session gets a sign-in notice instead
of the panels; the entry point is hidden for them anyway.

### Account — change password

Current / new / confirm → `POST /api/auth/password` with `{old_password,
new_password}`. A `401` means the current password is wrong and says so; anything
else surfaces the server's message. Success clears the form and toasts.

**The forced first-run change keeps its own modal.** Bootstrap sets
`password_change_required` on the first admin, and `auth.js` opens the modal
automatically when the identity carries it — that gate must not depend on the
user finding a page. Only the *voluntary* change lives here.

### API tokens

Tokens are the credential for non-browser clients — scripts, `curl`, a native
player (`docs/ui/madplayer.md`). A token belongs to one user and carries **that
user's permissions**; it is an alternative credential for the same account, not a
separate access level.

- **Create** — name (required) plus an optional expiry **date**. Blank = never
  expires. The client converts the date to an absolute unix timestamp at the end
  of that day, local time, and sends `expires_at`, so "expires 2026-12-31" is
  valid through that whole day. (`expires_in_days` still exists for API clients;
  `expires_at` wins when both are sent, and a past expiry is a `400` on both
  sides.)
- **One-time reveal** — the create response carries the raw token, and it is the
  only time it exists anywhere: the server stores only its SHA-256. It is shown
  in a highlighted box with a **Copy** button, which goes through the shared
  `clipboard.js` and falls back to selecting the text when the origin has no
  clipboard API at all — the ordinary case on a plain-HTTP deployment
  (`docs/ui/clipboard.md`).
- **List** — name · created · last used · expires · Revoke, with `—` for
  never-used and `Never` for a null expiry. `DELETE /api/auth/tokens/{id}`
  revokes; the row **stays in the list**, marked *Revoked* and without the
  button, so the user keeps the history rather than watching a row vanish.

### Appearance — theme

Four themes: **dark** (default), **light**, **ocean**, **sunset**. Picking one
applies it live and globally — it sets `<html data-theme>`, which the persistent
shell is already styled against — and persists it in `localStorage`
(`madshare-theme`).

Theme is **per-device, not per-account.** There is no server-side user settings
store, so a second browser starts on the default. `theme.js` (`currentTheme` /
`applyTheme` / `VALID_THEMES`) is the single seam if that ever changes; a
server-side theme would seed `localStorage` from `/api/auth/me` and change
nothing else.

**No flash of the wrong theme.** Every page template runs a tiny inline
`<head>` script — the `theme-guard` partial, a classic script rather than a
module, because modules are deferred and run after first paint — that applies the
saved theme before the stylesheets load. Admin pages carry it too, which is why
the theme dots could leave the shared header without admin losing the setting.

## What a second client takes from this page

Only one thing, but it is load-bearing: **create a token here, send it as
`Authorization: Bearer <token>`.** Everything else on the page is a browser
concern — a native client owns its own theme and can offer password change
against the same endpoint if it wants to.
