# Persistent Shell + Continuous Playback

Status: **planned** (design; no code yet)
Branch: aidev
Supersedes: `IDEA-01` in `.issues/ui-issues.md` (continuous playback across navigations)

## Where this sits in the bigger plan

This is the **foundation phase** of a larger UI body of work. The pieces depend
on each other; this doc covers only the first and the queue model the rest build
on. The whole roadmap, for context:

```
Phase 1  Persistent shell + continuous playback (LISTENING side)   ← THIS DOC
            router + page lifecycle, move the player + queue into a
            persistent shell, migrate the library onto it. Admin stays
            OUT of the shell. Queue modelled to extend to playlists.

Phase 2  Hash-precheck backend            → docs/plans/upload-rework.md
Phase 3  Upload page rewrite              → docs/plans/upload-rework.md
Phase 4  Admin files rework (metadata /   → docs/plans/admin-files-rework.md
            rename / merge / delete)         (decoupled — parallelizable)
Phase 5  Playlists + favorites + likes + queue editing  (future; builds
            directly on this doc's queue model)
```

The critical path is **1 → (2 → 3)**. Phase 4 is independent of the shell and
can run in parallel. Phase 5 is the payoff and is deliberately last because
Phase 1 is what makes it cheap — so the **queue model here must be designed as if
playlists already existed**, just without persistence.

## Why

Today the listening UI is a multi-page app: every nav link is a full document
load, so the `<audio>` element (in the `player-bar` partial) is destroyed on each
navigation and playback stops. Goal: start a track on the library page, navigate
elsewhere in the listening app, and the music keeps playing gaplessly — with the
queue intact (next/prev/shuffle still work).

This is now feasible because the prerequisite is already paid for: `player.js` is
a **standalone core** (owns its own `<audio>`, decoupled from playlists, driven by
callbacks — see `webui/static/js/player.js`), and the pages are clean per-section
modules. What's missing is a **persistent shell** to host the player and a small
client router to swap page content without tearing the player down.

## Goals

1. The player (`<audio>` + `player-bar` + **queue state**) lives in a shell that
   survives navigation between listening pages. Music plays continuously.
2. **Full playlist continuity** (decided): next/prev/shuffle keep working on any
   listening page, not just the library — because the queue is shell-owned, not
   page-owned.
3. Pages become thin views: the library is a *view over* the shared queue, not
   the owner of it.
4. Server keeps rendering **complete pages** — the shell is progressive
   enhancement. Deep links, refresh, and `-tags nowebui` degrade to full-page
   nav with zero loss of function.
5. Model the queue as a first-class, mutable, ordered entity so Phase 5
   (playlists/favorites) is a thin layer on top, not a rewrite.

## Non-goals

- **Admin is NOT part of the shell** (decided — see Decisions §1). Admin pages
  stay separate full-load pages with their own page-local preview player.
- No backend changes in this phase. (The hash-precheck endpoint and playlist
  tables belong to later phases.)
- cmus stays out of the shell (it is a paused view with its own player design —
  see Decisions §3).
- No playlist persistence, favorites, or likes yet (Phase 5).

---

## Architecture

### The shell model — progressive enhancement, not an SPA rewrite

The server keeps rendering one complete HTML document per listening route exactly
as it does today (`webui.go` `Register`). The shell is a small always-present JS
module that intercepts same-origin nav clicks, fetches the target document, and
swaps **only the page-content slots** into the current document — leaving the
header, player-bar, and `<audio>` untouched. This is the Turbo Drive / htmx-boost
pattern.

**The persistence rule:** *everything outside `<main>` is shell chrome and is
never torn down.* In `library.html` the `player-bar` and `<audio>` already sit
**after** `</main>` (see `webui/html/library.html:48` and the
`{{define "player-bar"}}` partial), so the swap leaves them alone by construction.

**Swap contract** — on navigation the shell replaces, from the fetched document:

- the `<main>` element's contents (the page body),
- the **header-insert** region (the library search bar lives here via
  `{{define "header-insert"}}`; it must appear on the library and vanish
  elsewhere),
- the active nav-link state (`.nav-link--active` / `aria-current`) — read from
  the fetched header,
- `document.title`.

Everything else in the header (logo, nav links, user area, theme switcher) and
the entire player-bar + `<audio>` are static and survive.

**Why progressive enhancement (Option B), not a single shell document (Option A):**
keeping the server rendering full pages means deep links, refresh, the `nowebui`
build, and JS-disabled clients all keep working with no special-casing — the
shell is pure enhancement layered on top. A true single-shell-document rewrite
would force a server-side fragment/layout split for no added user benefit.

### Navigation flow

```
click <a> (same-origin, listening route, not target=_blank, not modified click)
  → preventDefault
  → teardown() the current page module
  → fetch(url)                         (HTML; server renders the full page)
  → 401/403?  → toast + abort swap, leave current page intact (see Auth)
  → DOMParser the response
  → ensure CSS: inject any <link rel=stylesheet> from the fetched <head>
                whose href isn't already present (inject-and-keep, never remove)
  → swap the slots (main, header-insert, nav active, title)
  → history.pushState(url)
  → import the new page's module and init() it
popstate (back/forward) → same swap for the target URL
first load / refresh / deep link → server-rendered page is already correct;
                the shell hydrates it (init() the current route's module)
```

`<audio>` is never in a swapped slot, so playback is uninterrupted throughout.

### Page module lifecycle

Today each page boots once via a top-level IIFE (e.g. `app.js`'s
`(async function boot(){ await initAuth(); loadArtists(); })()`) — it relies on
the `<script type=module>` running on document load. With swaps there is no new
load event, so every listening-page module changes from "self-boot" to an
**exported `{ init, teardown }` pair**:

```js
// page module contract
export async function init()      // bind listeners, fetch data, render, subscribe
                                  //   to the player controller's events
export function  teardown()       // remove listeners, abort in-flight fetches,
                                  //   clear timers, unsubscribe — but NEVER touch
                                  //   the shared queue
```

The shell decides which module to run from a **`data-` attribute on the fetched
document**, so there is no hand-maintained route→module table:

```html
<body data-page="library" data-module="/static/js/app.js"> … </body>
```

The shell `import(bodyDataModule)`s it (ES modules are cached singletons, so a
re-visit reuses the same module), then calls `init()`. Before navigating away it
calls the previous module's `teardown()`.

**`initAuth()` moves to shell scope:** it runs **once** when the shell boots and
the identity persists across swaps (today `auth.js`'s `initAuth` re-runs per
page). Page `init()`s still call `gatePage(PAGE_PERMS.x)` for their own gating.

### The player controller (the heart of this phase)

A single `PlayerController` lives in a module imported once by the shell and
shared (ES-module singleton) by every page that touches playback:

```
webui/static/js/player-controller.js   (NEW)
  - wraps createPlayer() from player.js (owns <audio> + player-bar DOM)
  - owns the QUEUE: tracks[], index, shuffle, (repeat reserved for later)
  - the next/prev/shuffle/advance logic that lives in app.js today MOVES here
    (it is queue logic, not library logic)
  - public API (designed playlist-ready; implement what Phase 1 needs):
      setQueue(tracks, startIndex)   replace queue and play
      play() / pause() / toggle()
      next() / prev()
      current() -> { track, index } | null
      // queue mutation — API designed now, used by Phase 5:
      enqueue(tracks) / insertAt(i, tracks) / removeAt(i) / move(from,to) / clear()
      setShuffle(on) / setRepeat(mode)
      subscribe(event, cb) -> unsubscribe
        events: 'trackchange'(track,index) 'play' 'pause' 'ended'
                'timeupdate'(t,dur) 'duration'(track,seconds) 'queuechange'
                'error'(kind)   ← includes auth-failure (see considerations §2)
  - a "track" is { url, title, artist, album, art?, hash, durationSec? }
    (hash is the stable key pages use to map a track back to a DOM row)
  - REPEAT is first-class from day one: setRepeat('off'|'all'|'one') changes the
    'ended' → advance behavior (sits next to shuffle; see considerations §3).
  - The controller OWNS the Media Session integration (navigator.mediaSession):
    it sets metadata (title/artist/artwork) on 'trackchange' and registers the
    play/pause/next/prev action handlers, since it is the single playback owner
    (see considerations §1). Designed in here, not bolted on later.
  - The controller watches the <audio> 'error' event and classifies auth
    failures (a 401 on the next track's request, which does NOT pass through the
    shell's fetch interceptor) so playback failure surfaces a re-auth prompt
    rather than dying silently (considerations §2).
  - Keep the queue logic (next/prev/shuffle/advance/repeat) DOM-FREE and exported
    so it is unit-testable without a browser (considerations §6).
```

Because the controller is a module singleton and the `<audio>`/player-bar it
drives are in the never-swapped shell chrome, the queue **naturally survives
navigation** — no serialization, no save/restore. That is the whole mechanism for
full continuity.

### The library becomes a thin caller

`app.js` keeps the library-specific concerns and drops the queue ownership:

- **builds** track objects from rows and calls `controller.setQueue(tracks, i)`
  on row click (today it mutates a local `playlist` array — that array and
  `currentIndex`, `advance()`, `playIndex()` move into the controller);
- **reflects** state: subscribes to `'trackchange'` → paints `.playing` on the
  row whose `hash` matches; on `init()` calls `controller.current()` to
  re-highlight whatever is already playing when you return to the library;
- **duration write-back**: subscribes to `'duration'` → writes the value into the
  matching row and the existing `DUR_CACHE` (`madshare-durations`);
- search-mode playback (building a queue from search results) becomes another
  `controller.setQueue(...)` caller — same path, no special "playing from search"
  bookkeeping needed because the queue is now authoritative and page-independent.

Row→track mapping uses the track `hash` as the stable key, since rows are
re-rendered on re-entry and the active queue may have been built from a different
view (library vs. search).

### Routing / server changes (`webui/webui.go`)

Minimal. The server keeps rendering full pages. Changes:

- Every **listening** page template (`library.html`, the reworked `upload.html`,
  and future playlist pages) gets:
  - `<body data-page="…" data-module="/static/js/…">`,
  - `{{template "player-bar" .}}` after `</main>` (library already has it; the new
    upload page adds it so playback shows there too),
  - `<script type=module src="/static/js/shell.js">` **instead of** the page's own
    direct `<script src=…app.js>` (the shell imports the page module via
    `data-module`).
- No new routes for Phase 1. `pageData` is unchanged.
- **Admin templates are untouched** — they keep their own `<script>` boot and do
  **not** load `shell.js` (Decisions §1).

### Admin separation

The header's **Admin** nav link gets `target="_blank"` so opening admin from the
listening shell launches a **separate tab** — the listening tab (and its
playback) stays alive. Admin pages are full-load pages with their own page-local
preview player (a private `createPlayer()` instance, no shared queue). Server-side
auth (`auth.RequirePermission` on the whole `/admin/*` group) is unchanged and is
the real boundary; the shell never weakens it. Keeping admin out of the shell also
keeps admin JS off the always-loaded surface (defense-in-depth) and dissolves the
"admin preview vs. live queue" question — they are simply different players.

Accepted tradeoff: entering admin is a full load, so library playback **stops**
when you open an admin page in that tab. Admin is a rare, distinct mode; this is
acceptable.

### Auth during swap

A fetch that returns **401/403** (e.g. a non-admin following a stale link, or a
session that expired mid-session) must **not** blank `<main>`: the shell aborts
the swap, keeps the current page, and shows a toast / opens the login modal
(reusing `auth.js`'s `openLoginModal`). Nav links remain identity-gated as today.

### CSS / assets

**Inject-and-keep** (decided — Decisions §2): on navigation the shell injects any
of the fetched document's stylesheet `<link>`s not already present and never
removes them. Over a session the document accumulates every visited page's CSS —
tiny, and it avoids a "bundle everything into app.css" refactor and the reflow
churn of swapping stylesheets. Player styles stay in `player.css` (already
shared). No per-page CSS consolidation is required by this phase (BUG-15's
remaining `upload.css` `.btn` override is folded into the upload rework, Phase 3).

---

## Phasing (within this phase)

Each step leaves the app working.

1. **Extract the controller.** Create `player-controller.js`: move
   `playlist`/`currentIndex`/`advance()`/`playIndex()` + shuffle out of `app.js`
   into the controller; have `app.js` drive it through the new API. No shell yet —
   library still boots per-page. Regression-test the library to **exact parity**
   (drill-down playback, next/prev/shuffle, row highlight, duration write-back,
   search-mode playback). This de-risks the riskiest part (touching the working
   player) before the router exists.
2. **Build the shell + router.** Add `shell.js` (intercept nav, fetch, swap slots,
   pushState/popstate, CSS inject-and-keep, module `import` + lifecycle,
   one-time `initAuth`). Convert `app.js` from boot-IIFE to `{ init, teardown }`.
   Point `library.html` at `shell.js` with `data-page`/`data-module`. Now the
   library is a single-page shell of one page — verify swap mechanics with the
   library navigating to itself / breadcrumb, plus back/forward + deep-link +
   refresh.
3. **Make the listening set multi-page.** Bring the (reworked) upload page into
   the shell so there are ≥2 swappable pages and continuity is real
   (start playback on library → navigate to upload → music keeps playing,
   next/prev still work). *This overlaps Phase 3 of the roadmap (upload rewrite);
   coordinate so the new upload page is shell-native from the start.*
4. **Admin link → `target="_blank"`** and confirm admin pages are untouched and
   playback survives opening admin in a new tab.

Regression testing the library player after step 1 and again after step 2 is the
gate — everything else is additive.

---

## File-by-file change list

**New**
- `webui/static/js/shell.js` — the client router + page lifecycle + one-time
  auth boot.
- `webui/static/js/player-controller.js` — the shell-owned `PlayerController`
  (queue + player core wrapper), playlist-ready API.

**Changed**
- `webui/static/js/app.js` — drop queue ownership (moves to the controller);
  become a thin caller; convert boot-IIFE → exported `{ init, teardown }`;
  subscribe to controller events for highlight + duration write-back.
- `webui/html/library.html` — `<body data-page data-module>`; load `shell.js`
  instead of `app.js` directly. (player-bar already present after `</main>`.)
- `webui/html/partials.html` — header **Admin** link gains `target="_blank"`.
- `webui/webui.go` — none required for Phase 1 (pages still render full); the
  upload-page template gains the shell wiring in Phase 3 of the roadmap.

**Unchanged (deliberately)**
- `webui/static/js/player.js` — the low-level core stays as is; the controller
  wraps it.
- All `webui/html/admin/*` and admin JS — admin is out of the shell.
- `webui/html/cmus.html` + `cmus.js` — cmus stays out (paused view).

**Backend: none.**

---

## Testing

- **Library parity (the gate)** after controller extraction *and* after the shell
  lands: drill-down playback, next/prev/shuffle, `.playing` row highlight,
  duration write-back + `DUR_CACHE`, search-mode playback. Must match
  pre-change behavior exactly.
- **Continuity:** start a track on the library, navigate to the upload page,
  confirm playback is uninterrupted and next/prev/shuffle still operate; return
  to the library and confirm the playing row re-highlights.
- **Router mechanics:** back/forward (`popstate`) restores the right page and
  active nav; deep-link straight to `/upload` works (full server render);
  refresh mid-session works; a modified click (ctrl/cmd/middle) and
  `target="_blank"` links are **not** intercepted.
- **Auth:** expire the session, click a gated link → toast / login modal, current
  page preserved (no blank `<main>`).
- **Admin separation:** Admin link opens a new tab; playback in the original tab
  is unaffected; admin pages render without `shell.js`.
- **Degradation:** with JS disabled (or a `nowebui` build for the API surface),
  full-page navigation still works and nothing 500s.
- Remember: webui assets are **compile-time embedded** — rebuild/restart to see
  changes (per local-verify notes).

---

## Decisions

1. **Admin stays OUT of the shell.** Server-side `auth.RequirePermission` is the
   real boundary and is unchanged, but separating keeps admin JS off the
   always-loaded surface, dissolves the admin-preview-vs-queue question (admin
   uses a page-local preview player), and matches the conventional admin/app
   split. The Admin nav link is `target="_blank"`. Accepted cost: entering admin
   stops playback in that tab.
2. **CSS = inject-and-keep**, driven off the fetched document's `<head>` links.
   No per-page CSS consolidation required here.
3. **cmus deferred.** Paused view with its own player design and non-shared
   header; migrating it now is unverifiable work. Revisit when un-paused.
4. **Full playlist continuity** (vs. audio-only): the queue is shell-owned so
   next/prev/shuffle work on every listening page. This is the whole point of the
   phase and the reason the queue model is built playlist-ready.

## Additional considerations & recommendations

Captured 2026-06-09 so they aren't lost. The first four (§1–3, §6) are meant to be
**designed into Phase 1's controller** (cheap now, costly to retrofit); the rest
are "keep in mind." `/files/*` is served by Go's `http.FileServer` / `ServeFile`,
which already supports HTTP **Range**, so seeking + efficient large-file playback
work server-side today (see §9 for the invariant to protect).

**Design into Phase 1 (they shape the controller):**

1. **Media Session API — the highest-value addition.** With the controller as the
   single playback owner, wiring `navigator.mediaSession` gives OS-level
   integration nearly for free: lock-screen / notification-shade controls,
   artwork + title/artist, and **hardware media keys** (keyboard play/pause,
   headphone/Bluetooth/car buttons). The controller already knows the current
   track and has next/prev — it's the only correct place to set it. Much harder to
   add later. Folded into the controller API above.
2. **Auth expiry mid-playback — a failure the shell CANNOT fetch-intercept.** The
   `<audio>` element fetches each track itself (cookie-based) and bypasses the
   router's fetch interceptor. If a session expires during a long listening
   session, the next track's request silently 401s and playback just dies. The
   controller must watch the audio `error` event, detect auth failure, and surface
   the login modal / re-auth. Folded in as the `'error'` event above.
3. **Repeat mode** (off / all / one). Reserved in the API; spec it now because it
   changes the `'ended'` → advance logic and users expect it beside shuffle.
   Cheaper designed alongside shuffle than threaded in later.
6. **The queue logic is the one genuinely unit-testable piece of JS — and there is
   no JS test harness today** (all UI testing is manual browser checklists). The
   controller's next/prev/shuffle/advance/repeat is pure logic with no DOM; keep
   it DOM-free and exported so it can be unit-tested without a browser. This
   matters because it's about to hold the trickiest, most regression-prone state
   in the UI, and Phase 5 piles more onto the queue. Worth a lightweight JS test
   setup for just that module.

**Keep in mind (not Phase 1 blockers):**

4. **SPA accessibility on swap.** Swapping `<main>` via JS is invisible to screen
   readers (no page-load event) and drops keyboard focus. The router must move
   focus to the new main/heading and announce via an `aria-live` region on each
   navigation. Easy in the router, easy to forget, a real regression vs. today's
   full-page loads.
5. **Router loading + generic error states.** Beyond the 401/403 case already in
   the design: decide what shows on a slow swap fetch (loading indicator) and on a
   network failure (graceful "couldn't load — stay put / retry", never a blanked
   `<main>`).
7. **Queue persistence across a hard reload.** Full continuity survives *swaps*,
   but a refresh / tab-restart loses the queue. Persisting the current queue +
   position to `localStorage` and resuming on load is a small win now and a direct
   stepping stone to Phase 5 (a playlist is a named, server-persisted version of
   the same shape). Decide where it sits — even a v0 localStorage resume.
8. **Keep the track model federation-friendly.** Federation is the deferred
   *project* Phase 4. The track is `{ url, … }`, which is already fine — just
   consciously avoid baking "local content-hash only" assumptions into the
   queue/controller so a future queue can mix local + remote (federated) sources.
   Free insurance if you're aware of it.
9. **Range-serving is a "don't break it" invariant.** Seeking works today because
   `/files/*` uses Go's `FileServer`/`ServeFile`. The content-access auth layer
   wraps `/files/*` — ensure any wrapping handler does **not** buffer the body or
   strip `Range`/`Accept-Ranges`, or seeking dies and mobile downloads whole files
   before playing. Worth an explicit test.
10. **PWA / mobile background playback (future direction, name only).** Continuous
    playback matters most on mobile (screen-off listening), but mobile browsers
    aggressively suspend background tabs. The eventual answer is a PWA (web app
    manifest + service worker) for installable, background-capable playback — and
    this persistent-shell architecture is its prerequisite. Not now; noted because
    the shell is what makes it possible later.
11. **Theme FOUC (deferred to a user-settings page).** `<html data-theme="dark">`
    is hardcoded and JS swaps to the saved theme after load, so on a slow
    connection the user sees a flash from dark → their theme. Fix is a tiny inline
    `<script>` in `<head>` that reads the saved theme and sets `data-theme` before
    CSS paints (a no-FOUC guard). Owner's call (2026-06-09): minor, handle it when
    the **user-settings page** is built. Noted so it isn't lost.

## Step-1 status (controller extraction)

Done on `aidev` (pending browser verification): `player-controller.js` owns the
queue + wraps `player.js`; `app.js` is a thin caller that builds queues
(`controller.setQueue` on a track click) and reflects state via callbacks.
Decisions made:

- **Track identity key = track URL** (decided). Rows carry `data-url`; the
  highlight matches by URL across the library and search views and survives a
  re-render. (We keep the full `${API}${t.url}` string as the key, not the bare
  hash — it's already unique and is what the row/queue use.) This replaced the old
  position-index + `playingFromSearch` scheme, which is now deleted.
- **Stable queue** (decided, owner-approved): browsing never changes the queue —
  only an explicit track click (`setQueue`) does. The old "browsing a new album
  silently became the Next/Prev queue" behavior is gone (it fought continuity).
- `app.js` still boots via its IIFE for now; the `{ init, teardown }` conversion
  happens in **step 2** (the router), where it's actually needed.
- **Deferred to later step-1 sub-work** (kept out to keep the parity diff clean):
  Media Session, repeat mode, and audio-`error`→re-auth (note: a `<audio>` error
  doesn't expose HTTP status, so auth-expiry detection needs a probe — design when
  we add it).

## Open questions

- **Duration cache vs. controller.** `DUR_CACHE` stays in `app.js` (page-side);
  durations also ride on the queue's track objects (`track.dur`) once known.
  Good enough for now; revisit if step 2 re-entry needs more.
- **Scroll restoration** on back/forward — nice-to-have; Turbo-style per-URL
  scroll memory. Defer unless it feels bad in testing (step 2).
