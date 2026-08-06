# Madnetwork Nodes — the directory and the per-node page

*Companion to `docs/ui/madnetwork-page.md`, which owns the landing view and the
merged browse. This doc owns two surfaces: **`/madnetwork/nodes`** (the
directory) and **`/madnetwork/node/<key>`** (one node). **Built 2026-08-02** —
this describes what shipped, verified on a real three-node mesh (`meshlab up
-topology chain -nodes 3 -friends adjacent`: self at 0, the direct friend at 1,
the friend's friend at 2, matching the admin map's distances exactly).*

*(Raised by the owner 2026-08-02: the landing view's node list shows every node
at once, there is no page for the node list, and there is no address for a single
node. The same round settled the local-library lane and the lane-shaped node
digest — those are in madnetwork-page.md §Rework.)*

## Why a node needs an address

The `/madnetwork` page could already browse one node's shelf: the status-strip
chip and the *By node* lane both dropped the drill-down into a `?source=<id>`
mode. Nothing about that was linkable. It was a mode of one page, held in a JS
variable, and the id it was keyed by is **a local row number** — `federation_catalog_sources.id`,
handed out by whichever sweep first pulled from that node. Two servers never
agree on it, and *this* server does not agree with itself over time: the frontier
rotation evicts the coldest sources past `discovery_cap`, so a node that goes
quiet and comes back is a different id afterwards.

So the address is the **public key**, which is the node's identity everywhere
(federation.md §Goal & vocabulary: *identity is always the key*, a name is a
claim). A node page URL pasted into a chat lands on the same node on the
recipient's server if their view reaches it — and lands on an honest "not in
view" if it does not. Neither is possible with a row number.

```
/madnetwork/nodes                 the directory
/madnetwork/node/<64-hex key>     one node
```

Lowercase hex, exactly as `federation.NormalizeKey` produces. **Our own node is
not a special case**: this server has a key too, and `/madnetwork/node/<our key>`
is our published shelf. `?source=self` stays as the API's shorthand, but there is
no `/madnetwork/node/self` — an address that means something different on every
server is the thing this section is replacing.

## Ordering: hops first, then the alphabet

Every node list on the page — the landing digest and the directory — is ordered

1. **hops ascending**, then
2. **name, case-insensitively**, then
3. **key**, so two unnamed nodes still have a stable order.

Hops is friendship distance from us, from the same BFS the network map is drawn
by (`federation.walkGraph` already returns it beside the branch attribution; see
"Where hops comes from"). Self is 0, a direct friend is 1, a friend's friend 2.

The ordering says something true and useful in one number: **the nodes you chose
personally come first, then the ones they chose, and so on outward.** It is the
only ordering of a community that is a fact about *our* view rather than a
ranking of other people — which is the same reason the lanes are computed from
what this node can see (madnetwork-page.md §The rules these lanes must obey).
It also happens to be stable: the alphabet churns as names are heard, hops
changes only when a friendship does.

**A node we cannot place sorts last, among its own kind, alphabetically.** That
happens when the gossip has not reached it (we hold its catalog from the frontier
rotation but no edge has arrived yet) and for every node when federation is off
or the graph is empty — in which case the whole list is plain alphabetical, which
is the same rule with a smaller world. An unplaceable node's row says *distance
unknown*; it never says 0, because 0 is us.

Hops replaces the current SQL ordering (direct friends first, then name). SQL
cannot know hops — the graph is the federation node's, not a table the browse
joins — so the final sort happens in the handler, over the whole list, once. The
SQL `ORDER BY` stays as the deterministic base underneath it.

### Where hops comes from

`walkGraph(selfKey, peers, edges)` already computes `dist` and throws it away:
`branchesOf` keeps only the `via` half for the browse's branch weighting. So the
hop map is free — the same walk, the other return value.

`federation.Node` gains `HopMap(ctx) (map[string]int, error)` beside `BranchMap`,
sharing **one memo** (`Intervals.MembershipTTL`, as now) so a page load that asks
for both walks the graph once. The api `FederationNode` interface gains the same
method; the `nofederation` stub and the test fakes answer nil, which the ordering
reads as "nobody is placeable" — the degenerate case above.

The shared memo has one visible consequence, seen on the mesh run: a node
discovered mid-minute is **unplaceable, and therefore last, until the memo
refreshes**, then takes its place in a ring. That is the same staleness
BranchMap accepts and in the same harmless direction — a node we have not
finished placing is sorted where "we know less about this one" belongs, never
promoted to 0.

Exposing hops to a `madnetwork.access` holder is not a new disclosure: the browse
already sends holder **keys** to the same audience (the ⓘ panel links them to the
map) and already prints branch counts. Hops is a fact about our own graph, which
is what every number on this page is.

## The directory — `/madnetwork/nodes`

A shell-native page in the `madnetwork` section, reached from the landing view's
*Nodes* lane ("See all →") and from the section's subtab bar.

```
┌─────────────────────────────────────────────────────────────────┐
│ Network   Nodes                                    ← subtab bar │
├─────────────────────────────────────────────────────────────────┤
│ 12 libraries · 4 direct friends · 1 not seen recently           │
│ [ filter nodes…                                             ]   │
│                                                                 │
│  madshare@home        this server   0     412 entries           │
│  a1b2c3d4…                                                      │
│                                                                 │
│  vinylcellar          direct friend  1 hop   1 204 entries      │
│  ab12cd34…            seen 4m ago · synced 12m ago              │
│                                                                 │
│  (unnamed)            member  2 hops   88 entries               │
│  9f8e7d6c…            not seen recently                    ░░░  │
└─────────────────────────────────────────────────────────────────┘
```

Each row carries the four things the owner asked for, and nothing that would need
a second query:

| shown | why |
|---|---|
| **name** (or *(unnamed)*) | the label chain the summary already resolves: our own name for a friend, the heard name otherwise |
| **key**, truncated | beyond our own friends a name is hearsay and the key is the fact — the same rule the network map follows, and it is what the URL is built from. The whole row is the link to the node, so the key gets its copy control on the card there, not here |
| **hops + class** — *this server* / *direct friend* / *member* | the ordering key, made visible so the order is not a mystery |
| **entries · seen · synced** | reachability and catalog freshness in the vocabulary the chips already use; a node outside its freshness window is greyed, not removed |

The filter box is client-side over the fetched list, matching name or key as a
substring — the two are trusted differently (a name beyond our own friends is
what a node claims about itself) and someone checking up on a node arrives
holding the key. The
list is bounded by `[federation].discovery_cap` — 200 by default — so it is one
fetch and no paging. If that bound is ever lifted the directory gets the same
keyset paging *Browse all* has; it does not need it at 200.

Rows are links (`<a href="/madnetwork/node/<key>">`), not click handlers, so
middle-click and "copy link" work. The shell router intercepts the click.

## One node — `/madnetwork/node/<key>`

The page **is** the shelf that `?source=` used to be, with the node's facts above
it. The in-page shelf mode goes away; there is one address for a node.

```
┌─────────────────────────────────────────────────────────────────┐
│ Network   Nodes                                                 │
├─────────────────────────────────────────────────────────────────┤
│ ‹ Nodes                                                         │
│ ┌─────────────────────────────────────────────────────────────┐ │
│ │ vinylcellar                          direct friend · 1 hop  │ │
│ │ ab12cd34ef56…  ⧉                                            │ │
│ │ 1 204 entries · seen 4m ago · catalog synced 12m ago        │ │
│ │                                          open on the map →  │ │
│ └─────────────────────────────────────────────────────────────┘ │
│ [ search this node…                                         ]   │
│ ┌─────────────────────────────────────────────────────────────┐ │
│ │ vinylcellar › Air › Moon Safari                             │ │
│ │  1  La femme d'argent                     ⓘ           7:07  │ │
│ │  2  Sexy Boy                              ⓘ           4:58  │ │
│ └─────────────────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────────────────┘
```

- **The card** is the directory row with room to breathe: name, full key with a
  copy control, class + hops, entries, last seen, catalog synced, greyed when
  stale. *open on the map →* points at `/admin/network#node=<key>` and is
  rendered **only** for holders of `federation.manage` — the same gate, for the
  same reason, as the ⓘ panel's holder links: sending someone to a page that will
  only refuse them is worse than not linking.
- **The shelf** is the existing drill-down, restricted to this node: artists
  (keyset-paged, windowed) → albums → tracks, with the same rows, hearts, ⋯
  menus, Materialize and ⓘ panel as everywhere else. The breadcrumb is rooted at
  the node's name; *‹ Nodes* returns to the directory.
- **Search within the node** — `/api/madnetwork/search?q=&source=<key>` — using
  the shared `browse-search.js` view. The endpoint already takes `source`; this
  is the surface that makes it worth having.
- **A node's shelf still never folds our own set in.** Unchanged rule from
  madnetwork-page.md §Browsing a single node, and on our own node page it works
  in the other direction: `/madnetwork/node/<our key>` shows what *we* publish,
  and nothing a friend holds.
- **Materialize all is not a node-level action.** The bulk flow is sequential by
  design (one transfer at a time), and a whole node is thousands of tracks. It
  stays per artist / per album, where it is now.

### When there is nothing to show

Three answers, and they are different on purpose:

| case | the page says |
|---|---|
| well-formed key, no source cached | *This server holds no catalog from this node.* Plus the card with whatever the graph can say (hops, heard name) — a node we can place but have not pulled from is a normal state of the frontier rotation, not an error |
| well-formed key, nothing in view at all (unknown, or blocked, whose rows are hidden) | *This node is not in view.* The key is echoed so the reader can compare it |
| malformed key | *That is not a node key.* 400 from the API, no lookup |

## API

Everything is gated `madnetwork.access`, like the rest of the browse.

**`GET /api/madnetwork/summary`** — the payload's `friends` array becomes
`nodes`, and each entry gains:

```json
{ "id": 7, "key": "ab12…", "name": "vinylcellar", "hops": 1, "friend": true,
  "entries": 1204, "last_seen": 1754130000, "synced_at": 1754129000,
  "reachable": true, "self": false }
```

`hops` is omitted when the node is unplaceable. Our own node is now an ordinary
entry with `"self": true` and our key, instead of the separate `self_name` field —
one list, sorted by one rule, with us in it at 0 hops. `tracks` and
`inbound_healthy` are unchanged.

The rename is deliberate and follows federation.md §Goal & vocabulary: this list
has not been "friends" since F7 item 5 taught the sweep to pull from members, and
a field name that lies is how a UI ends up saying "4 friends" about nodes nobody
chose. The Go type `database.MadnetworkFriend` is renamed `MadnetworkNode` with
it.

**`GET /api/madnetwork/nodes/{key}`** — one node's card, or `404` with the "not
in view" body, or `400` for a malformed key. Same fields as a summary entry. It
exists rather than having the page filter the summary client-side because the
summary lists only sources we hold a catalog from, and the "no catalog yet" case
above is precisely a node that is not in it.

**`?source=` accepts a key.** Today it parses `<id>` or `self`; it gains a third
form, a 64-hex key, resolved to a source id in the handler (and to `SelfOnly`
when it is our own). One identifier drives the whole node page, and the numeric
form keeps working.

**An unresolvable `?source=` key yields an empty view, not the merged one.** The
current rule — "an unparseable source is the merged view rather than an error, a
stale link should land somewhere useful" — is right for a stale row *number* and
wrong for a key: a key is an explicit request for one node, and answering it with
the whole community's catalog is the one answer that is certainly wrong. This is
the same argument §Browsing a single node already made for the view that includes
neither half, so the machinery (`fedcatNoRows`) is there.

## What shipped (2026-08-02)

1. **Hops** — `walkGraph`'s `dist` surfaced as `Node.HopMap`, sharing the branch
   memo (`graphMemo`); `FederationNode.HopMap` on the api interface + stub +
   fakes. `TestHopsMatchTheMap` pins it against `BuildNetworkMap`, the same way
   `TestBranchesMatchTheMap` pins the other half.
2. **Summary rework** — `database.MadnetworkFriend` → `MadnetworkNode` carrying
   `public_key`; the payload's `friends` → `nodes` with `key`/`hops`/`self`; the
   ordering in `api/madnetwork_nodes.go` (`nodeList` + `sortNodes`), with
   `TestMadnetworkNodesOrderedByHops` pinning hops → name → key and
   unplaceable-last, and `TestMadnetworkNodesWithoutAGraph` the degenerate world.
3. **Node endpoint + key-addressed source** — `GET /api/madnetwork/nodes/{key}`
   (200 / 200+`no_catalog` / 404 / 400), `?source=<key>`, and
   `database.NoSourceID` for the unresolvable case.
4. **Pages** — routes `/madnetwork/nodes` and `/madnetwork/node/{key}`, the
   `madnetwork-subnav` partial (Network · Nodes), two templates, and the module
   split that made a node page possible at all: `mn-browse.js` (rows, ⓘ panel,
   Materialize, and `createShelf` — the drill-down as a factory instead of module
   state) plus `mn-nodes.js` (the card, the row, and the hops/class wording all
   three surfaces share).
5. **Landing view rewiring** — madnetwork-page.md §Rework: the `local` lane, the
   *Nodes* digest, every node entrance repointed at `/madnetwork/node/<key>`, the
   strip's chips removed, the in-page `?source=` shelf and its breadcrumb step
   deleted.

The drill-down becoming a factory is the load-bearing part of 4. A shelf held in
module-level state is a shelf that can only exist once, on one page — which is
exactly why "browse one node" could not have an address before.
