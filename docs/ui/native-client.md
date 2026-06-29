# Native desktop/mobile client

> **Status: designed, not built — deferred.** This is forward direction, not
> scheduled work. Implementation waits until the backend is **stable**, and in
> particular until **federation** (the milestone this client exists to use, see
> below) is designed and landed. Building the native shell before the API and the
> federation model settle would mean re-chasing a moving contract in two UIs at
> once. Revisit when the server core is solid.

## What this is

A **separate, native GUI client** for desktop and mobile — *not* a reuse of the
web UI. By default it is "just another music player" running against its own
**embedded** madshare backend, with the federation features layered on so it can
also act as a peer to other madshare nodes.

The deliberate consequence is **two UIs that solve the same task**: the existing
HTML/CSS/JS web UI for the browser, and this native client for desktop/mobile.
That cost is accepted on purpose — see *Why two UIs*.

## The decisions

### Two renderers over one shared contract

The madshare **HTTP API is the single shared contract.** Features land in the
backend once; each UI just paints them — exactly the relationship the web UI and
the (already-built) Capacitor app have today. The two clients are *renderers*, not
two products.

- **Web → keep HTML/CSS/JS.** JS is the right tool *for the browser*: the DOM,
  accessibility (screen readers), and web-native behaviours (find-in-page, text
  selection, deep links, native scrolling) are real advantages, and the existing
  web UI already delivers them. A canvas-rendered Go UI (Gio/Fyne compile to a
  WebAssembly `<canvas>`) was considered for web reuse and **rejected** — the
  canvas loses all of the above. So web stays JS.
- **Native → a separate pure-Go GUI.** Desktop + mobile get a Go UI (Gio / Fyne
  leaning; toolkit not finalised — see *Open: UI toolkit*). Not Flutter/Godot:
  those still leave a second language + an FFI seam, and don't reuse on the web
  the way you'd want anyway.

### The native client embeds the backend as a library

The native app is a **single Go binary**: the madshare server **and** a Yggdrasil
node compiled in as libraries and run **in-process**, with the Go UI on top.

- **In-process, not a sidecar.** Spawning the compiled server as a child process
  is the simplest model on desktop, but **iOS forbids spawning processes** and
  Android makes it awkward — so mobile forces the in-process library path, and we
  plan for that everywhere.
- **`modernc.org/sqlite` (pure Go, no CGo)** is what makes the mobile embed
  (`gomobile` / `c-archive`) feasible at all. This was already the project's DB
  driver — the choice pays off here.
- **Local-first.** With the backend embedded, the app is a standalone music
  player against its *own* library over loopback, with **no server required**.

### Federation peers is the milestone

The point of embedding the backend is to make every install a **node** that can
**peer** with other madshare nodes. Plain "authenticate to a remote server as a
thin client" is an acceptable fallback but is **not** the goal.

- **Yggdrasil as a transport library.** One of the reasons the project is in Go:
  Yggdrasil-go is itself Go, so it links into the same binary. Use it as a
  **transport** (route madshare traffic over its connection API) — preferably
  **without** creating a system TUN device, because a TUN on mobile needs VPN
  entitlements (Android `VpnService`, iOS `NetworkExtension`), a permissions and
  store-review headache. *Verify the current yggdrasil-go library API exposes
  library-as-transport before committing.*
- **Self-certifying identity.** The Yggdrasil node key derives the node's mesh
  address, so the address *is* proof of who the node is — it can double as the
  madshare node identity and remove a whole identity-bootstrap problem.
- **The real open problem is cross-node authorisation.** Today auth is per-instance
  roles; "which remote node may pull which content from me?" needs a new trust
  model. That is its own design — `docs/architecture/federation.md`, picking up
  the Phase-4 thread already deferred in `docs/architecture/auth.md` §8. **Write
  that doc before this client's federation code.**

### The payoff: one networking layer, local and remote

Because the **embedded local backend speaks the same HTTP API as a remote peer**,
the native client needs only **one** networking layer. "My local library" vs.
"a friend's node over Yggdrasil" differ only by **base URL** (loopback vs. a peer's
`.ygg` address) — the same relative-URL trick the web UI already uses for
same-origin. Local-first and federated are the *same code path*.

## Why two UIs (and keeping the cost bounded)

Two UIs is real maintenance cost, accepted because each is best-in-class for its
target (JS for the web, native Go for desktop/mobile). The cost stays bounded by
discipline, not luck:

- **Push logic into the API, not the clients.** Anything that today lives as
  *client-side JS logic* — queue index arithmetic (`queue-ops`), the disc-numbering
  rule (`disc.js`), the `album_artist ?? artist` grouping fallback, the rendition
  quality ladder — would otherwise need a **Go twin** in the native client. Compute
  it server-side and return it wherever feasible, so neither UI re-implements it.
  (The existing instinct to keep grouping server-side is exactly this pattern.)
- **Design docs are the shared behavioural spec.** `docs/ui/player-and-queue.md`,
  `docs/ui/shells.md`, etc. define behaviour once; both UIs *follow* the spec
  rather than each inventing it. Keep them authoritative.

## The native client's own burdens (not shared with web)

- **Audio playback/decoding.** No Flutter-style plugin ecosystem — this is the
  native client's job. Pure-Go decoders cover MP3/FLAC/Vorbis; **Opus/AAC/M4A**
  need cgo bindings or ffmpeg. Synergy: lean on the **embedded backend's** ffmpeg
  (the deferred recordings **P5** transcode work) to decode/transcode locally to a
  Go-playable format. Gapless / ReplayGain / crossfade are all manual.
- **Mobile background playback & process lifecycle.** The OS aggressively kills
  background work — the lesson already learned in the Capacitor app's
  `MediaPlaybackService` foreground service (`docs/architecture/android-app.md`).
  An embedded *server* backgrounding is harder still, and on iOS a persistent
  server effectively can't run backgrounded at all.
- **Intermittent mobile peers.** A phone serves over the mesh only while
  foregrounded, so federation must expect an **asymmetry**: a backbone of
  always-on desktop/server nodes plus flaky mobile peers. This pushes toward a
  "favourite/subscribe → replicate" model later so content survives a peer going
  offline.

## Relationship to the Capacitor Android app

The Capacitor app (`docs/architecture/android-app.md`) is a thin WebView that
reuses the **web UI** same-origin and is already built and on-device-verified.
This native Go client is a **more ambitious, different direction** for the mobile
slot: an embedded backend + a real federation peer, not a remote-URL shell. They
are alternatives — when this client is actually scheduled, decide whether the
Capacitor app remains the lightweight "connect to someone's server" option while
the native client is the "run your own node" option, or whether one supersedes the
other.

## Open: UI toolkit

Gio vs. Fyne (or something better) is **not** decided. The criterion is the one
tradeoff that matters:

- **Single-language Go (Gio / Fyne):** one language end-to-end, no FFI seam, one
  build for desktop + mobile (+ a WASM/canvas web build we are *not* using). Cost:
  weaker UI polish / smaller widget ecosystem, and you own audio playback.
- vs. a two-language stack with a Go core — rejected here for the seam and the
  lack of web reuse.

**De-risk before committing:** build one real screen (a track list + the player
bar) in the candidate toolkit, compile it to the actual target (desktop *and* a
phone), and **look at it** — load size, scroll feel, text rendering, "real app vs.
game UI". The feel is the thing you cannot take on faith.

## First steps when it's time (not now)

1. `docs/architecture/federation.md` — the cross-node trust/ACL model. The
   riskiest unknown; precedes any client federation code.
2. UI-toolkit prototype + decision (above).
3. Native shell: embed the Go core (madshare + Yggdrasil) in-process; UI talks to
   it over loopback using the *same* API the web UI uses.
4. Federation peering on top, once the trust model is designed.
