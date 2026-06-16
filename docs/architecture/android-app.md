# Android app — Capacitor remote-URL shell

Status: **P1 built** on the `app-dev` branch (`mobile/` — launcher, §4 safety
gate, health probe, hand-off; the `classify.js` gate is unit-tested). P2–P4
pending. This describes how to ship an installable
Android app that reuses the existing web UI by wrapping it in a [Capacitor](https://capacitorjs.com/)
WebView pointed at a running Madshare server. The emphasis is the
**connection-safety model**: the app must work over plaintext on an
already-encrypted network (Yggdrasil or any other encrypted overlay/VPN) *and*
over TLS, while loudly warning the user before it ever sends credentials over
plaintext across an untrusted network.

## 1. Goal & approach

Reuse 100% of the web UI (`webui/`) — no second front-end to maintain. The app
is a thin native shell whose WebView loads the server's pages **same-origin**.

"Remote-URL" means the WebView's content origin *is* the server (e.g.
`http://[200:abcd:…]:3000` or `https://media.example.org`), not bundled assets
calling a remote API. That choice is deliberate and buys two things for free:

- **Auth just works.** The web UI authenticates with the session cookie
  (`madshare_session`, HttpOnly, SameSite=Lax, Secure-when-TLS). Same-origin
  means the cookie rides every request, including the `<audio>` element's
  streaming `GET /files/<hash>/<file>` — which **cannot carry an `Authorization`
  header**, so a bundled-assets + bearer-token design would not be able to play
  gated audio without extra machinery. Same-origin sidesteps that entirely.
- **No CORS.** Same-origin requests need no `Access-Control-Allow-Origin`, so the
  default-closed `[cors]` policy (see `listeners-and-config.md` §4.3b) needs no
  change and no app-origin allow-listing.

The only native pieces are (a) a small **connection manager / safety gate** the
user sees before the WebView loads a server, and (b) a **foreground-audio**
capability so playback survives backgrounding.

### Non-goals

- A bundled-assets / bearer-token client (revisit only if a TLS-everywhere,
  cross-origin deployment becomes the norm). The same `[webui].api_base` +
  bearer-token mechanism already exists if that day comes.
- A native (Kotlin/Compose) UI. Best background-audio fidelity, but throws away
  the web UI; out of scope here.
- iOS (Capacitor supports it; only Android is in scope for this doc).
- Offline downloads / sync.

## 2. Components

```
┌─────────────────────────── Android app (Capacitor) ───────────────────────────┐
│                                                                                │
│  Bundled launcher (web assets in the app)                                      │
│    • server list (add / pick / forget)                                         │
│    • connection-safety gate  ─────────► §4                                      │
│    • health probe (GET /healthz, /api/ui/config)                               │
│    • hands off: window.location = <serverUrl>                                  │
│                                                                                │
│  WebView ── after hand-off, loaded SAME-ORIGIN on the server ──────────────►   │
│    • the unchanged Madshare web UI (library / playlists / upload / settings)   │
│    • session-cookie auth, mediaSession already wired (player-controller.js)    │
│                                                                                │
│  Native plugins                                                                │
│    • foreground-service audio (keep <audio> alive in background)               │
│    • (optional) Yggdrasil status / connectivity hints                          │
└────────────────────────────────────────────────────────────────────────────────┘
```

The launcher is itself web tech (HTML/JS bundled as the app's default assets), so
the only native code is the foreground-audio plugin and Capacitor glue. The
launcher is the app's "home"; selecting a server navigates the WebView to that
origin. A persistent **"Servers" affordance** (a native back-action or a small
floating control injected by the shell) returns to the launcher to switch
servers — see §7 Open questions.

## 3. Supported connection types

The app must support all three, because Madshare is commonly deployed on an
encrypted overlay with no TLS:

| Type | Example URL | Transport encryption | Default UX |
|------|-------------|----------------------|------------|
| **TLS** | `https://media.example.org` | TLS | allowed, no warning |
| **Encrypted overlay, plaintext** | `http://[200:abcd:…]:3000` (Yggdrasil), WireGuard/Tailscale/Nebula/Tor | the overlay / VPN | allowed; auto-detected ones silent, others need a one-time "trusted network" confirm |
| **Local network, plaintext** | `http://192.168.1.67:3000`, `http://localhost:3000` | none (link-layer only) | allowed, no hard warning (the user's own LAN) |
| **Public network, plaintext** | `http://203.0.113.5:3000`, `http://host.example:3000` | **none** | **blocked behind a warning** (§4) |

The danger we are guarding against: over plaintext on an untrusted path, the
**session cookie and the entire library stream in clear**, so an on-path attacker
can read or replay the credential. TLS or an encrypting overlay removes that;
a public plaintext endpoint does not.

## 4. Connection-safety gate (the core of this design)

Before the launcher hands the WebView to a server URL, it classifies the
endpoint and decides whether to warn. The app can rarely *prove* a network is
encrypted (a Yggdrasil tunnel looks like ordinary IPv6 to the OS), so the model
is: **auto-trust what we can positively recognise, treat the user's own LAN as
acceptable, hard-warn on everything else over plaintext, and let the user
explicitly vouch for overlays we can't detect.**

### 4.1 Classification

```
classify(url):
  if url.scheme == "https":            return SAFE_TLS          # transport-encrypted
  # scheme == "http" from here on:
  host = url.host
  if host is loopback (127.0.0.0/8, ::1, "localhost"):  return SAFE_LOCAL
  if host endsWith ".onion":           return SAFE_OVERLAY      # Tor-encrypted
  if host is an IP literal:
    if ip in 0200::/7:                 return SAFE_OVERLAY      # Yggdrasil
    if ip in 10/8, 172.16/12, 192.168/16, 169.254/16,
            fc00::/7 (ULA), fe80::/10 (link-local):  return LOCAL_LAN
    else:                              return UNSAFE_PUBLIC     # global/public IP, plaintext
  else (a hostname, not an IP):
    return UNSAFE_PUBLIC                # can't classify; DNS+host in clear → treat as unsafe
```

Per-server override: a user may mark a saved server **"I trust this network
(encrypted/VPN)."** That downgrades `UNSAFE_PUBLIC → SAFE_OVERLAY` for that
server only, covering WireGuard / Tailscale / Nebula / corporate VPNs the app
cannot auto-detect. The override is remembered with the server entry and shown
(and revocable) in its settings.

We deliberately do **not** auto-trust the Tailscale/CGNAT range `100.64.0.0/10`:
it overlaps carrier-grade NAT, so plaintext there is not guaranteed encrypted.
Such a server uses the explicit override instead.

### 4.2 Outcomes

| Class | Action |
|-------|--------|
| `SAFE_TLS`, `SAFE_OVERLAY`, `SAFE_LOCAL` | connect silently |
| `LOCAL_LAN` | connect; optional one-line, dismissible note ("plaintext on your local network") — no blocking dialog |
| `UNSAFE_PUBLIC` | **block** with a confirm dialog; only proceed on explicit "I understand" |

### 4.3 The warning

Shown only for `UNSAFE_PUBLIC` (and not suppressed by an override):

> **⚠️ This connection isn't encrypted.**
> You're about to connect to `http://<host>` over a public network with no TLS.
> Your login and everything you browse or play could be read or stolen by others
> on the network — **you may be about to leak your auth data.**
>
> Only continue if you trust this network (for example a VPN, or a mesh like
> Yggdrasil that encrypts traffic itself).
>
> `[ Cancel ]`  `[ I understand the risk — continue ]`  `[ ☐ Don't ask again for this server ]`

"Don't ask again for this server" sets the per-server trusted-network override
(§4.1). The dialog re-appears if the host changes. The recommended remedy
(use `https://` or put the server on Yggdrasil) is linked from the dialog.

### 4.4 Why this is the real protection (not the OS setting)

Android blocks cleartext HTTP by default (API ≥ 28). Because the server is
user-configured and unknown at build time, the app must permit cleartext broadly
(via a network-security-config / `usesCleartextTraffic`) so Yggdrasil/LAN work
at all. That OS permission is coarse and cannot tell "ygg" from "public Wi-Fi" —
so the **in-app gate in §4.1–4.3 is what actually protects the user.** This is a
conscious trade and is documented for reviewers: broad OS cleartext + a narrow,
explicit in-app warning.

## 5. Auth & streaming

No server change. In same-origin remote-URL mode:

- Login posts to `/api/auth/login`; the `madshare_session` cookie is stored by
  the WebView and sent on every subsequent request, including media.
- `Secure` cookie behaviour is already correct: over plaintext (ygg/LAN) the
  server sees no TLS and sets the cookie **without** `Secure` (so it is sent over
  http); over TLS it is `Secure`. Behind a TLS-terminating proxy, trust
  `X-Forwarded-Proto` to get the `Secure` flag (existing `contrib/nginx` note).
- Gated streaming (`/files/<hash>/<file>`, Range-enabled) is served to the cookie
  identity by `fileAccessGuard` — works unchanged because the `<audio>` request
  is same-origin and carries the cookie.

Bearer API tokens remain the path for *non-app* clients; the app does not need
them in this mode.

## 6. Background audio

`player-controller.js` already drives `navigator.mediaSession` (metadata +
play/pause/seek). **Correction to an earlier assumption:** the Android *System
WebView* does **not** implement the Web Media Session API, so those calls are a
no-op there (lock-screen/notification controls do **not** light up on their own),
and a backgrounded WebView is suspended by the OS so playback pauses. A native
plugin is therefore **required**, not just nice-to-have.

Chosen plugin: **`@jofr/capacitor-media-session`** (GPL-3.0, compatible with this
project's AGPL). It starts an Android **foreground service** for an active media
session — keeping the WebView's `<audio>` playing in the background — and renders
the media notification, with an API modelled on the Web Media Session API the web
UI already uses. Caveats to handle at P2: it is lightly maintained (single
maintainer; the 4.x line targets Capacitor ≤6 — we pin Capacitor 6), and because
the player runs on the **remote** server origin, the web UI must reach the plugin
across the hand-off — either a small `player-controller.js` tweak that routes
through the plugin when `window.Capacitor` is present, or an app-side adapter
injected into the remote page (same bridge-on-remote-origin mechanism the §10-Q1
return-to-launcher control relies on; the app sets `allowNavigation: ["*"]` so the
Capacitor bridge is injected into the server's pages). A fully native app would be
sturdier here; for v1 the foreground service is sufficient.

## 7. Packaging

1. `npm init @capacitor/app`; set `appId` / `appName`.
2. Bundle the launcher (server list + §4 gate + health probe) as the app's web
   assets; do **not** set a static `server.url` (the server is chosen at runtime).
   Hand off with `window.location.href = <serverUrl>` after the gate passes.
3. `npx cap add android`; in the Android project enable cleartext via a
   **network-security-config** (cleartext permitted; see §4.4) rather than a blanket
   `usesCleartextTraffic` where possible.
4. Add the foreground-audio plugin (§6).
5. Build the `.aab`/`.apk`. Distribution: sideload or Play Store (no TLS/Asset-Links
   requirement, unlike a Trusted Web Activity — which is why Capacitor, not TWA).

A `GET /healthz` probe validates a server URL before hand-off; `GET /api/ui/config`
(public) can confirm the server is a Madshare instance and read display bits.

## 8. Server-side changes

None required for the core flow. Optional, additive niceties (separate work):

- A small public **server-identity** field (name / version) the launcher can show
  when adding a server — `/api/ui/config` is the natural home.
- A short-lived **media URL token** (query-param) endpoint *would* be the unlock
  for a future bundled-assets/bearer-token variant (so `<audio>` can stream
  without a cookie). Explicitly **deferred** — the remote-URL design does not need it.

## 9. Phasing

- **P0 — PWA stepping stone.** Add a `manifest.webmanifest` + a minimal service
  worker (shell-asset cache only) to `webui/`. Makes the site "Install to home
  screen" and validates same-origin behaviour with zero native code.
- **P1 — Capacitor shell + launcher + §4 gate.** Single hard-coded-then-editable
  server, the full safety classification and warning, health probe, hand-off.
- **P2 — Background audio.** Foreground-service plugin wired to `mediaSession`.
- **P3 — Multi-server + overrides.** Saved server list, per-server "trusted
  network" override, switch-server affordance.
- **P4 — Polish / distribution.** Network-security-config hardening, app icons,
  `.aab`, optional Play Store.

## 10. Open questions

1. **Return-to-launcher UX.** Once the WebView is on a remote origin, how does the
   user get back to switch servers?
   **Resolved (P3):** a **native control, login-screen-only** — not part of the
   server's web UI. The app shows a "Servers" affordance for *unauthorised* users,
   detected natively by the absence of the `madshare_session` cookie (Android
   `CookieManager`) for the current origin; signing out reveals it. Keeps the
   control off the web UI (so the localhost/browser UI never grows an app-only
   button) and avoids fragile DOM injection into authenticated pages.
2. **Override scope.** Per-server or also per-network (SSID)?
   **Resolved:** **per-server** only (simpler; no network-state permission). The
   `mobile/www/js/launcher.js` server store already carries a `trusted` flag.
3. **PWA vs Capacitor first.** P0 may be enough for some users.
   **Resolved:** **Capacitor first** (P0/PWA deferred — revisit later).
4. **Foreground-audio plugin choice.**
   **Resolved (P2):** **`@jofr/capacitor-media-session`** (GPL-3.0). See §6 for
   rationale and the Android-WebView caveat; alternatives (Capawesome Media
   Session — paid + needs native audio; `@mediagrid/capacitor-native-audio` —
   plays natively, not the WebView) were rejected as a worse fit for the
   reuse-the-web-UI model.
