# Metered-network controls for listener devices (DRAFT)

Status: **draft, 2026-08-22.** A separate task from
`docs/plans/full-node-mode.md` on purpose (owner): that plan is about desktop
machines becoming members; this one is about phones staying *good listeners*.
Phones are never full nodes — running the mesh as a member would dry the
battery and churn the graph — but even the listener path moves real bytes both
ways, and on mobile data that is the user's plan being spent.

What this fixes: a madplayer on a phone seeds its cache over the mesh and
fetches via the swarm regardless of what network it is on. Every torrent-class
app learned the same lesson and grew the same switch: **wifi-only transfer
policy**. Our swarm is torrent-like by design, so it inherits the obligation.

## What the existing design already provides

- **The empty holdings push is the advertised-state tool.** A push is defined
  as a complete statement replacing the whole set in one transaction, so *an
  empty list is meaningful, not a no-op* (federation-access.md §"The
  household"). "Stop advertising" is therefore one ordinary API call, and
  "start again" is the next full push. The 90-min holdings TTL bounds
  staleness even if the phone vanishes without saying so.
- **A refusing holder costs little.** If a peer still asks for a hash after we
  stopped advertising, the swarm's failover treats the refusal as one bad
  holder, not an incident — but the polite path is to stop *serving* too, not
  only advertising.
- **Fetching is user-initiated.** A listener fetches when its user presses
  play or materialize; there is no background pull to police, only a policy
  question of whether that press is honoured on metered data.

## Design sketch

Client-side policy; the server needs nothing new.

- **One master policy: "Transfers on mobile data", three-way per direction:**
  - **Seed on mobile data — default OFF.** On a metered network the device
    (1) pushes an **empty holdings list** to its home server and (2) stops
    answering mesh blob requests (the node-side seed switch). Back on wifi it
    re-pushes the real list and serves again.
  - **Fetch on mobile data — default ON but explicit.** Playing/materializing
    is a deliberate act; refusing it outright on mobile data would surprise
    more than it protects. Worth a per-action size hint ("this will fetch
    ~42 MB over mobile data") rather than a hard block; a hard-block variant
    can be the third position of the switch.
- **Detection is platform-truth, not heuristics:** Android's
  `ConnectivityManager` metered flag (which also covers metered wifi
  hotspots); desktop treats the OS metered flag the same way where it exists
  (Windows has one), else never-metered.
- **Battery is a follow-up, not this task.** "Mesh only while charging" or
  foreground-only seeding are plausible later switches; keep this plan to the
  metered-data rule so it ships small. (Playback/Doze handling already exists
  and is untouched.)

## Work items — madplayer

- **M1. Connectivity watcher** with the metered verdict per platform.
- **M2. The seed gate:** on metered → empty holdings push + stop serving; on
  unmetered → full push + serve; idempotent on flapping networks (debounce —
  a train ride toggles connectivity constantly).
- **M3. The fetch hint/policy** on play/materialize over metered data.
- **M4. Settings UI** for the switches, defaults as above.

## Work items — madshare

- **M5. Verify only:** a test that an empty holdings push followed by a full
  re-push round-trips cleanly (rows gone, rows back, TTL untouched) — likely
  already covered by the household suite; confirm rather than assume.

## Open questions (owner)

1. Should the *fetch* side default to hint-and-allow or hard-block on mobile
   data? (Proposal above: hint-and-allow.)
2. Does the debounce live on the watcher (report stable state only) or on the
   gate (act at most every N seconds)? Watcher-side keeps the gate dumb.
3. Is there a data-budget appetite ("at most N MB/day over mobile") or is
   that scope creep for v1? (Proposal: creep — the switch is the feature.)
