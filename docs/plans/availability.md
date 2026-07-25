# Implementation plan — Madnetwork availability & node health

Turns `docs/architecture/federation.md` §"Availability & node health" (backend)
and `docs/ui/madnetwork-page.md` §Availability (UI) into build steps. Replaces
the reverted 10 s presence feature. Working checklist — delete once shipped and
folded into the reference docs (per `docs/plans/roadmap.md` convention).

**Core idea (recap):** availability is per **track** (union over holders), derived
from a slow/passive per-peer `last_seen` with a **minutes-wide** freshness window,
computed **at request time**; the UI hides unavailable tracks only at refresh
boundaries (page load / search), never live. Never fail dark: a dead local
inbound path makes us stop filtering, not blank the library.

## What already exists (don't rebuild)

- **`last_seen` is already maintained**, inbound and outbound:
  - `federation/friendship.go` `meshAuth` calls `TouchFederationPeerSeen` on
    **every** inbound request from a known peer (catalog pull, blob fetch, ping).
  - `sweep` (1-min `refreshLoop`) calls `pingPeer` for every friend → touches on
    a 200; `pairWith` touches too.
  - `database/federation.go` `TouchFederationPeerSeen` is **monotonic** (never
    moves backwards).
- **The browse queries already gate on friendship.** Every merged query joins
  `federation_peers p ... AND p.state = 'friend'` (`database/madnetwork.go`:
  `fedcatBase`, `fedcatRemoteRows`, `remoteTrackRows`, `MadnetworkSummary`). The
  availability filter is an **added predicate on that same join**, not a new join.
- **Holders already carry `last_seen` to the client** (`madnetworkHolder.LastSeen`
  in `api/madnetwork_handlers.go`) and the status strip shows per-friend
  `last_seen` (`MadnetworkFriend`). The UI greying needs no new data, only new
  rendering.
- **`remotePlaylistItems`** (`database/playlists.go`) already computes an
  `available` bool (local blob ∨ friend catalog ∨ friend holdings) — it just
  doesn't apply the freshness window yet, and has no cached-blob branch.
- `federation/node.go` holds `n.core`; `core.GetPeers() []PeerInfo` (each with
  `.Up bool`) is the underlay-liveness signal the self-health watchdog needs.

**Net:** the passive-liveness substrate is ~80% built. The new work is (a) a
freshness window in the queries, (b) a self-health watchdog + fail-open, (c) the
netstack fix that makes the SPOF recoverable, and (d) small UI polish.

## Policy constants (propose)

- `reachableWindow = 3 * time.Minute` — a friend is *reachable* if
  `last_seen ≥ now − reachableWindow`. Several × the 1-min refresh cadence, so a
  single missed ping never flips it (this is the whole anti-flap fix).
- `inboundSuspectRounds = 3` — consecutive 1-min sweeps with **zero** friends
  touched **and** ≥1 underlay peer `Up` before we declare the local inbound path
  suspect (→ fail open).
- Optional config knob `[federation] reachable_window_sec` (0 → default) and a
  runtime setting `madnetwork.hide_unavailable` (default **on**) in the existing
  `MadnetworkPolicy` — an escape hatch for an admin who prefers never to hide.

---

## Phase 0 — Netstack resilience (issue #398)

**Built (commit a85e618).** Reader loop now log-and-continues with 50ms→1s
backoff, exits only on `Close()` or the terminal `types.ErrClosed`; nil-guarded
delivery; `InboundReaderAlive()` accessor added; 5 tests. Error analysis: in the
pinned deps only `ErrClosed` is reachable and it's terminal — everything else is
defensively treated as transient. See `third_party/yggstack/MADSHARE-PATCH.md`.

Independent of the rest; makes the single-inbound-reader SPOF a recoverable fault
instead of a silent permanent death. The availability watchdog (Phase 1) is the
safety net that lets the feature ship even before this lands, but this is the real
fix and gates *trusting* liveness.

- **File:** `third_party/yggstack/src/netstack/yggdrasil.go`, the reader goroutine
  (currently `for { rx,err := Read(...); if err != nil { log; break } ... }`).
- Replace the unconditional `break` with:
  - a `closed` signal (add a `chan struct{}` / `atomic.Bool` to `YggdrasilNIC`,
    set in `Close()`): if closing → `return` (clean shutdown, exiting is correct);
  - otherwise **log-and-continue** with a short backoff (e.g. 50 ms, capped) so a
    genuinely permanent error can't hot-spin a core;
  - guard `DeliverNetworkPacket` when `dispatcher == nil` (post-Close race).
- Characterise `ipv6rwc.Read`'s error set first (issue's prerequisite): confirm
  which errors are terminal (core stopped / NIC closed) vs transient. Keep the
  terminal ones exiting.
- **Also expose the reader's liveness** as `(*YggdrasilNetstack) InboundReaderAlive() bool`
  (backed by the same alive/closed state — an `atomic.Bool` set true when the
  goroutine starts, false in a `defer` when it returns). Phase 1 wires
  `node.readerAlive = stack.InboundReaderAlive`; this is the **only unambiguous
  self-health signal** (see Phase 1).
- **Fork hygiene:** this is a **second** local patch — add a `LOCAL PATCH` marker
  and document it in `third_party/yggstack/MADSHARE-PATCH.md`; update the
  `[[yggstack-fork-patch]]` memory. Prefer upstreaming (issue's option 5).
- **Test:** a unit test that injects a transient read error and asserts the loop
  survives and later packets still deliver; a close test that asserts clean exit.

## Phase 1 — Liveness substrate + self-health watchdog

Backend signal only; no browse/UI change yet, so it can land and bake alone.

1. **Close the passive gaps** (`federation/`). **Built (commit 5b7a0be):**
   - `Node.observePeerAlive` touches `last_seen` on a **verified swarm chunk**
     (`swarm.go`) and a **completed whole-file fetch** (`transfer.go`), throttled
     to ≤1 write per peer per 30 s; `last_seen` stays monotonic.
   - Catalog/holdings sync run right after `pingPeer` in the same sweep, so
     they're already covered — not duplicated.
2. **Self-health signal** — `Node.InboundHealthy() bool` (+ `node_stub.go` stub
   returning true). **Built (commit 5b7a0be):** the method reads a pluggable
   `readerAlive func() bool` (nil ⇒ healthy). The signal source is settled below.
   - **The signal is the netstack inbound-reader liveness** (Phase 0's
     `InboundReaderAlive`), wired as `node.readerAlive = stack.InboundReaderAlive`
     in `Start` once Phase 0 lands. Until then it defaults healthy (safe — the
     browse keeps hiding unreachable friends, the common case).
   - **Two rejected alternatives (decided while building Phase 1):**
     - *Self-ping* (dial our own mesh address): **ruled out** — the netstack sets
       `HandleLocal: true`, so gVisor loops local traffic internally and a
       self-ping never exercises the real `ipv6rwc.Read` inbound reader (the
       SPOF). It would report healthy even with the reader dead.
     - *The "all-friends-stale N rounds + underlay peers `Up`" heuristic* (the
       original plan sketch): **not used to drive fail-open** — it's ambiguous
       with the *common* case where friends are simply offline while we hold
       public backbone underlay peers. Failing open there would re-show the exact
       stale/dead rows the feature hides. `core.GetPeers().Up` can't distinguish
       "my inbound died" from "friends are off". The reader-liveness signal is
       unambiguous, so it's the sole driver. (The heuristic may still surface as
       an *advisory* note on `/admin/network` — "haven't heard from any friend in
       a while" — but must not flip hiding.)
   - Blast radius if this is wrong is small: failing dark hides only friends'
     *remote* rows on `/madnetwork`; own library and the home page are untouched.

## Phase 2 — Availability predicate in the browse queries

**Built (commit 4c86c10).** `MadnetworkView{IncludeSelf, Cutoff}` threaded through
the browse/summary/search queries; friend join gated on `last_seen >= cutoff`
(inlined server-computed int; `<=0` = no filter). Handler sets `cutoff = now−180s`
when healthy, `0` when `InboundHealthy()` is false; summary carries
`inbound_healthy` + per-friend `reachable`. Playlists intentionally not gated.
Tests: `TestMadnetworkAvailability`, `TestMadnetworkView_FailOpen`.

The visible behavior. All in `database/madnetwork.go` + the handler wiring.

1. **Thread the policy into the store.** Replace the bare `includeSelf bool`
   argument on the `MadnetworkStore` methods with a small value:
   ```go
   type MadnetworkView struct {
       IncludeSelf bool
       Cutoff      int64 // unix; a friend is reachable if last_seen >= Cutoff. 0 = no filter (fail open / hiding disabled)
   }
   ```
   Handler builds it: `Cutoff = now - reachableWindow` normally; `0` when
   `!node.InboundHealthy()` **or** the `hide_unavailable` setting is off.
   *(Gotcha: this changes every `MadnetworkStore` signature → update
   `api/madnetwork_handlers_test.go`'s `fakeMadnetwork` and the `database` fake.)*
2. **Apply the predicate.** In the friend join used by every query, add the
   reachability gate, disabled when `Cutoff = 0`:
   ```sql
   JOIN federation_peers p ON p.id = c.peer_id AND p.state = 'friend'
        AND (:cutoff = 0 OR p.last_seen >= :cutoff)
   ```
   Touches: `fedcatBase`, `fedcatRemoteRows`, `remoteTrackRows`'s join, and the
   `MadnetworkSummary` track-count subquery. Self rows (`fedcatSelfRows`,
   `ownTrackRows`) are **never** gated. Because the count/album/artist queries
   dedupe across friends, a track held by one reachable + one stale friend still
   shows — exactly the union-over-holders semantics.
3. **Summary carries reachability for the strip.** Add a per-friend
   `Reachable bool` (computed `last_seen >= cutoff`) and a top-level
   `inbound_healthy` to `MadnetworkSummary` / the `madnetworkSummary` response, so
   the client greys stale friends and can show the fail-open banner.
4. **Remote playlists** — *decided NOT to freshness-gate* (2026-07-23).
   `remotePlaylistItems` keeps its "local blob ∨ any friend advertises (catalog ∨
   holdings)" semantics. Rationale: browse is discovery (hide unreachable to cut
   dead-link noise), but a playlist/favorite is an **intentional saved item** — it
   should not dim just because its holder missed a ping (the swarm may still fetch
   it); `available:false` should mean "no holder anywhere," which is the current
   rule. This is a deliberate browse-vs-saved distinction, and it avoids threading
   a cutoff through the general `GetPlaylist`/`Repository` interface.

## Phase 2b — Fully-cached exception (deferrable)

Keeps a streamed-into-cache track visible when its only friend goes offline. Own
library (materialised → approved) already survives via the self UNION; this covers
only *cache-only* content, so it can ship after Phase 2 without blocking it.

- **Migration 030** `federation_cache (hash TEXT PRIMARY KEY)` — this node's
  fully-cached blobs. *(Gotcha: bumps the schema version → update
  `database/database_test.go` version/table assertions.)*
- Insert on cache finalize (the `.part` → `<hash>` rename in the fetch path) and
  a startup reconcile scanning `<data_dir>/cache/madnetwork/` (reuse whatever
  `handleHoldings` already enumerates).
- Fold into the predicate as an OR so a cached rendition keeps its row regardless
  of `last_seen` — the LIKE-over-renditions pattern already used by
  `madnetworkRowsForHash`:
  ```sql
  AND (:cutoff = 0 OR p.last_seen >= :cutoff
       OR EXISTS (SELECT 1 FROM federation_cache fc WHERE c.renditions LIKE '%'||fc.hash||'%'))
  ```
  Add the same OR to `remotePlaylistItems`.

## Phase 3 — UI polish

**Built (this commit).** `/madnetwork`: stale friends greyed on the status strip
(`mn-friend--stale`, off `reachable`), a fail-open banner when `inbound_healthy`
is false (`mn-status-warn`), and stale holders struck through in the ⓘ panel
(`mn-holder--stale`, off a server-computed display `reachable` flag on each holder
— always `now−window`, so fail-open still shows staleness). `/admin/network` shows
an inbound-down warning (`#inboundWarn`, off `inbound_healthy` added to
`GET /api/admin/federation`). No client poll (confirmed); hide-at-refresh comes
from the existing load + search fetches. Tests: `TestMadnetworkHolders_Reachability`.

Small — the server now returns the available set and the client already re-fetches
on load and on search, so "hide at refresh boundaries" mostly falls out for free.

- `webui/static/js/madnetwork.js` / `admin/network.js`:
  - Grey (don't remove) an unreachable **holder** in the ⓘ panel and a stale
    **friend** on the status strip, keyed off `Reachable`/`last_seen` in the
    responses.
  - When `inbound_healthy` is false, show a one-line banner ("This node can't
    reach the mesh right now — showing last-known catalog") — the visible face of
    fail-open.
- `/admin/network`: surface the inbound-suspect flag as a node-health indicator
  (`Node.InboundHealthy`).
- Confirm there is **no** residual presence-style client poll (the P4 5 s summary
  poll was removed in the revert; the `#mnBulk` materialize-all poll is unrelated
  and stays).
- Update `docs/ui/madnetwork-page.md` §Availability build-order item to "shipped".

## Phase 4 — Config, verification, docs

**Built (this commit).** `[federation] reachable_window_sec` (default 180,
clamped up to a 120s anti-flap floor with a warning; `config.Default/MinReachableWindowSec`)
→ `Deps.ReachableWindowSec` → the handler's `reachWindow()`. Runtime
`madnetwork.hide_unavailable` toggle (default on) in `MadnetworkPolicy`
(`GET/POST /api/admin/settings/madnetwork` + checkbox card on `/admin/settings`);
`madnetworkView` returns cutoff 0 when it's off. `madshare.toml.example` +
`TestConfig_ReachableWindow_DefaultAndClamp` + the toggle-off case in
`TestMadnetworkView_FailOpen`. Owner reports Phases 0–3 working on the real mesh
(2026-07-23). Original checklist:

- Optional `[federation] reachable_window_sec` in `config/` + validation; wire the
  runtime `madnetwork.hide_unavailable` toggle into `MadnetworkPolicy`
  (`GET/POST /api/admin/settings/madnetwork`, card on `/admin/settings`).
- `go build ./...` (+ `-tags nofederation` and `-tags nowebui`), `go vet`,
  `go test ./...`, and `-race` (per the GC-model memory: `-race` needs
  `-timeout ≥ 3300s`, no parallel suites; Go is at
  `~/.guix-home/profile/bin/go`, not on PATH).
- Playwright (`BASE_URL=…`, disposable seeded server — the dev DB is content-empty).
- **Two-node live mesh** — and, crucially, the failure the revert demands:
  reproduce/observe on a **real lossy/latent mesh**, not loopback, that (a)
  transfers no longer stall and (b) availability doesn't flap. Verify: kill a
  friend → its exclusively-held tracks gone on the **next reload/search** (not
  live); restart → back after a reload; own/cached always present; kill the local
  inbound path → banner + full catalog stays (fail open), not a blank page.

  **CLOSED (2026-07-25)** by the mesh test toolset (`tests/mesh/README.md`),
  which made a hostile mesh something you start rather than wait for. Both halves
  are covered, by the arm suited to each:

  - **(b) availability doesn't flap** — the chaos suite proves it under faults a
    real network only supplies occasionally: `TestChaosFlappingLinkStaysFresh` (a
    link dropping every few seconds never pushes a friend past the window) and
    `TestChaosSustainedLossStaysReachable` (5 % packet loss on a `quic://`
    underlay; worst `last_seen` age 1.0 s normally, 2.4 s under `-race`, against
    an 11.6 s window). `TestChaosPartitionThenHeal` covers the input to the
    hiding predicate, and asserts the load-bearing inverse: a *remote* outage
    must never read as a *local* fault, or every partition would fail open.
  - **(a) transfers no longer stall, and the user-visible half** — meshlab, on
    real servers. Measured on a 3-node triangle with one track each: partition a
    friend → at 120 s (`reachable_window_sec`, the lab runs at madshare's floor)
    the other two go `madnetwork 3 → 2`, losing only the track held *exclusively*
    by the unreachable node and keeping their own and the reachable friend's;
    heal → back to 3 within ~90 s with no restart and no admin action. The
    walkthrough is in `tests/mesh/README.md` §The availability walkthrough.

  One item is *deliberately* not reachable by cutting a link, and that is the
  design working: `InboundReaderAlive` watches the goroutine reading the
  yggdrasil core into the netstack, which sits **above** the underlay, so a cut
  peering leaves it blocked but alive. The dead-reader path stays unit-tested
  with an injected read error; `meshlab status` surfaces the flag if it ever
  trips.

## Dependencies & sequencing

```
Phase 0 (netstack) ─┐  (independent; the real SPOF fix)
Phase 1 (liveness + watchdog) ─┬─> Phase 2 (predicate) ─> Phase 3 (UI) ─> Phase 4
                               └─> Phase 2b (cache exception, optional, after 2)
```

Phase 2 is shippable on Phase 1 alone (watchdog fail-open covers a dead reader);
Phase 0 upgrades "restart to recover" into "auto-recover" and should not block the
feature. Recommended first cut: **1 → 2 → 3**, with **0** in parallel and **2b**
deferred until cache-only content is proven to matter.

## Known gotchas

- Signature change on `MadnetworkStore` breaks `fakeMadnetwork` (api tests).
- Migration 030 breaks `database_test.go` version/table assertions.
- `TouchFederationPeerSeen` is monotonic — reactive *failure* can't be modelled by
  moving `last_seen` back; if reactive de-ranking is wanted it's a separate
  provider-failure signal (already partly present in `swarm.go` failover). Treat
  as an optional refinement, out of the critical path.
- Don't reintroduce any fast dedicated prober — the 1-min sweep + passive touches
  are the whole liveness budget by design.
