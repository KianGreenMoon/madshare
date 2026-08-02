# The mesh listener — serving madshare on the node's own Yggdrasil address

**Status:** BUILT 2026-08-02. The operator-facing reference now lives in
[`docs/architecture/listeners-and-config.md`](../architecture/listeners-and-config.md)
§4.3c / §5.3a / §6.7 and in the README; this file is kept for the *reasoning* —
the gate, what transport-only deliberately does not start, and the deferred
items — until those are folded in and it is deleted.

## 1. Motivation

Today, reaching a madshare server from outside its LAN takes one of two setups,
and both are more than a first-time operator should have to do:

- **Public internet:** a domain, a certificate, port forwarding, and nginx
  terminating TLS ([`contrib/nginx/madshare-ssl.conf`](../../contrib/nginx/madshare-ssl.conf)).
- **Yggdrasil, the way it works now:** a *system* yggdrasil daemon (root, a TUN
  device), then either binding madshare directly at the host's mesh address, or
  fronting it with [`contrib/nginx/madshare-yggdrasil.conf`](../../contrib/nginx/madshare-yggdrasil.conf)
  to get it onto port 80. Port 80 is a kernel socket, so an unprivileged
  `madshare` user needs `setcap` or the proxy.

Meanwhile a federated node **already runs a full Yggdrasil node in-process** — a
`core.Core` plus a gVisor userspace netstack, no TUN, no root — and already
serves HTTP on it: the madnetwork protocol lives on port 1314 of the node's mesh
address (`federation/node.go:222`). The web UI and API could be served on port 80
of that same address for about fifty lines of code.

That is the whole feature. And since being *on* the mesh is a smaller thing than
federating over it, the two are separated here (§4): a server can take the
address without joining anything. The result, for a new operator:

> Install madshare. Put one peer URI and one `[[listen_mesh]]` block in the
> config. Give people `http://[201:…]/`. No proxy, no certificate, no domain, no
> port forwarding, no root, no TUN device — and no obligation to federate.

### What this is and isn't

- **It is** a second *transport* for the existing route groups. Same handler,
  same middleware, same auth, same `serve = [...]` vocabulary — only the
  listener's address space differs.
- **It is not** a new access-control layer. The mesh address is reachable by
  **every node on the global Yggdrasil network**, not just your friends or your
  madnetwork community (§7). Authentication remains the only gate, exactly as on
  a LAN listener.
- **It is not** the madnetwork protocol. That stays on port 1314 with its own
  audience model (`federation.Audience`); this is the ordinary human-facing UI
  and API on port 80. They share an address and an identity key, nothing else —
  and on a node that has not enabled federation, 1314 answers nothing at all
  while port 80 works normally (§4.2).

## 2. Concepts

### 2.1 Two address spaces, one process

A madshare with the mesh enabled has two independent networking stacks:

| | `[[listen]]` | `[[listen_mesh]]` |
|---|---|---|
| Socket | host kernel (`net.Listen`) | gVisor userspace (`stack.ListenTCP`) |
| Address | whatever you bind | exactly one: the node's, derived from its key |
| Reachable from | that interface's network | the whole Yggdrasil mesh |
| Reachable from *this host* | yes | **no** (§2.3) |
| Ports below 1024 | need root or `setcap` | **free** — no kernel, no privilege model |
| Transport encryption | none (proxy's job) | Yggdrasil, end-to-end, always |

The two never collide: a `[[listen_mesh]]` entry on port 80 has nothing to do
with port 80 on the host, and the existing "conflicting binds" validation
(`config/config.go:648`) must skip mesh entries for that reason.

### 2.2 The address is self-authenticating

An Yggdrasil address in `200::/7` is **derived from the node's public key**, the
same key in `<data_dir>/federation.key` and, if you federate, on your node card.
Connecting to
`http://[201:abcd:…]/` is therefore authenticated by construction, in the way a
`.onion` address is: you cannot be silently routed to a different server,
because no other key produces that address. Combined with Yggdrasil's
end-to-end link encryption, **plain HTTP here is not the same proposition as
plain HTTP on the clearnet** — there is nothing for TLS to add that the overlay
is not already doing. This is why the no-proxy deployment is defensible rather
than a shortcut, and it is why `[[listen_mesh]]` needs no certificate story.

Consequence worth stating plainly: your web UI's address and your madnetwork
node card's public key are **the same identity**. Anyone you hand a node card to
can also find your login page. That is intended (one node, one address), but it
should not be a surprise.

### 2.3 You cannot reach your own mesh address from the host

There is no TUN device. Packets on the netstack move only `gVisor → ipv6rwc →
core → mesh`; the host kernel has no route to that address. So on the server
itself, `curl http://[201:…]/` **fails**, while a remote mesh peer reaches it
fine. This looks exactly like a bug the first time you hit it.

Therefore: **keep an ordinary loopback `[[listen]]` alongside the mesh one.**
Every example below does. The doc must say this at the point of first contact,
not in a footnote.

## 3. Config schema

### 3.1 `[[listen_mesh]]`

```toml
[[listen_mesh]]
port  = 80                          # optional, defaults to 80
serve = ["api", "webui", "admin"]   # the same three groups as [[listen]]
```

- **No `addr`.** There is exactly one mesh address and the node derives it from
  its key. An `addr` key here would be either ignored or wrong, so it is a
  **fatal** unknown-field error rather than a silent no-op.
- **`port` defaults to 80.** Nothing in the userspace stack makes 80 privileged,
  and a URL with no `:port` in it is the entire user-facing point of the
  feature. Any port in `1..65535` is allowed except `1314`
  (`federation.MeshPort`), where the madnetwork protocol is served on this same
  address — a **fatal** conflict with a message naming it. The reservation is
  **unconditional**, including in the transport-only mode where 1314 is
  genuinely free (§4): a config that works must not stop working the day its
  operator enables federation.
- **`serve`** is the existing group vocabulary (`api` / `webui` / `admin`) with
  the existing rules, plus one new advisory (§5, rule 5).
- **`allow_from`** is accepted and CIDR-validated as on `[[listen]]`, but it is
  close to useless here and the docs should say so: mesh addresses are
  key-derived, not allocated in meaningful prefixes, so there is no subnet to
  name. The filter operators actually want — *friends and community members
  only* — is a real thing this node can compute and is deferred to §8.

Multiple `[[listen_mesh]]` blocks are allowed (e.g. port 80 for the UI, another
port for an API-only surface), subject to no two using the same port.

### 3.2 `[yggdrasil]` — the transport

```toml
[yggdrasil]
#enabled  = true                            # implied by [federation].enabled (§4)
#key_file = "./data/federation.key"         # the node identity; address derives from it
#peers    = ["tls://peer.example:12345"]    # outbound underlay peerings
#listen   = ["tls://0.0.0.0:12345"]         # incoming underlay peerings (backbone nodes)
```

This section owns **the mesh itself**: the identity key, the underlay peering,
and therefore the address. `[federation]` keeps everything that is a *madnetwork
feature* — friendship, catalogs, scope, discovery, quotas, seeding.

The split falls where it does because that is where the concepts actually
divide. A node's key and its peers are what put it on the mesh and give it an
address; friendship and catalogs are what it then chooses to do there. Serving
your web UI over the mesh needs the first half and none of the second.

**`key_file` is the same file and the same meaning in both modes.** It is the
node identity: the mesh address derives from it, so a server that later enables
federation comes back on the address people already had. Default stays
`<data_dir>/federation.key` — the name is now slightly off-topic, but renaming
it would orphan every existing node's identity, which is the one file that must
never move.

**Compatibility.** `key_file` / `peers` / `listen` are still read from
`[federation]` when `[yggdrasil]` does not set them, so every config written
before this change keeps working untouched. `[yggdrasil]` wins when both are
present. The `[federation]` copies are documented as deprecated aliases; new
examples use `[yggdrasil]`.

`enabled` is a **tri-state `*bool`** (absent / `true` / `false`), following the
existing precedent of `WebUIConfig.GitRepo` (`*string`) and
`default_share_depth` (`*int`): "absent" and "explicitly false" must be
distinguishable, because absent means *infer it* and false means *the operator
said no*, and those get different answers in §4.

## 4. The mesh gate

`[[listen_mesh]]` requires **the mesh**, which is a smaller thing to require
than federation. Two config paths turn it on, and either is sufficient:

- `[yggdrasil].enabled = true` — **transport only.** The core and netstack come
  up and mesh listeners are served. No madnetwork protocol on port 1314, no
  friendship, no catalog sync, no discovery. A private server that is reachable
  from anywhere and federates with nobody.
- `[federation].enabled = true` — **implies the transport**, because madnetwork
  is served on the mesh address and has no other route to a peer. This is what
  keeps every config written before this change valid without editing it.

| `[federation].enabled` | `[yggdrasil].enabled` | Result |
|---|---|---|
| `false` | absent / `false` | No mesh. A `[[listen_mesh]]` entry is **fatal**. |
| `false` | `true` | Mesh **on**, transport only. Mesh listeners served; no madnetwork. |
| `true` | absent | Mesh **on** (inferred) + madnetwork. Every current config lands here. |
| `true` | `true` | Same. Said out loud. |
| `true` | `false` | **Fatal.** See below. |

The one contradiction, and the refusal an operator is most likely to meet:

```
config: [federation].enabled is set but [yggdrasil].enabled = false.
  Madnetwork IS served over the Yggdrasil mesh — peers reach this node at its
  mesh address and there is no other transport — so federation cannot run with
  the mesh switched off. Remove [yggdrasil].enabled to let federation bring the
  mesh up, or set [federation].enabled = false to turn both off.
```

And with a mesh listener but no mesh at all:

```
config: listen_mesh[0] needs the Yggdrasil mesh, but neither
  [yggdrasil].enabled nor [federation].enabled is set. The address a
  listen_mesh block binds is this node's own mesh address, which exists only
  while the mesh is running. Set [yggdrasil].enabled = true for a reachable
  server that federates with nobody, or [federation].enabled = true to join the
  madnetwork too (README, "Deploying a madnetwork node"). To bind an ordinary
  host address instead, use [[listen]].
```

Both are fatal at `config.Load`, before anything opens.

### 4.1 The build gate stays

Separating the two at the *config* level does not separate them in the
*binary*. `-tags nofederation` compiles the yggdrasil and gVisor dependencies
out entirely, so under that tag there is no transport either and
`[yggdrasil].enabled` is as unserveable as `[federation].enabled`. It reuses the
existing gate at `madshare.go:72`, now naming whichever key asked for it:

```
config: [yggdrasil].enabled is set but this binary was built with
  -tags nofederation; rebuild without that tag, or remove [yggdrasil] and
  [[listen_mesh]].
```

A build tag that keeps the mesh while stripping the madnetwork feature set is a
real thing to want and is deferred (§8) — the transport/feature split lands in
the config first, where it costs almost nothing, and in the build later, where
it costs a dependency graph.

### 4.2 What transport-only does *not* start

Worth pinning, because each one is a place where a later change could
accidentally couple the two back together:

- **No protocol listener on `MeshPort` (1314)** and no `protocolHandler`. Nothing
  answers `/madnetwork/v0/*`, so nothing can be pulled from this node.
- **No refresh loop, no catalog sync, no discovery sweep, no gossip.** The
  friendship layer is not merely idle, it is not constructed.
- **`Deps.Federation` stays nil**, so `/api/madnetwork/*` and `/admin/network`
  are not registered, and `webui.Register`'s `federated` flag stays false — `/`
  keeps forwarding to `/library` rather than `/madnetwork`, which is right,
  since there is no network to land on.
- **`fpcalc` is not required.** `requireFingerprinting` (`madshare.go:432`) is
  gated on `cfg.Federation.Enabled` and must stay that way: it exists because a
  federated node re-fingerprints *downloaded* audio before trusting it, and a
  transport-only node downloads nothing from anyone. Serving your own library
  over the mesh is not a trust question.

## 5. Validation rules

Added to `config.Load`'s existing list
([listeners-and-config.md §6](../architecture/listeners-and-config.md)):

1. `[[listen_mesh]]` requires the mesh — `[yggdrasil].enabled` **or**
   `[federation].enabled` (§4) — and a build with federation compiled in (§4.1).
2. `[yggdrasil].enabled = false` with `[federation].enabled = true` is fatal (§4).
3. Each mesh listener: `port` in `1..65535` and `≠ 1314` unconditionally (§3.1);
   `serve` non-empty and every token a known group. No `addr` field.
4. No two mesh listeners share a port. Mesh ports are **not** compared against
   `[[listen]]` ports — different address spaces (§2.1).
5. **Warn** when a mesh listener serves `admin`. Not an error: a single-operator
   node reached from a phone is exactly the case this feature is for, and the
   admin API is permission-gated regardless. But the audience of a mesh listener
   is *the entire Yggdrasil network*, which is a materially larger blast radius
   than the LAN address an operator is used to typing, so it should be a
   sentence at startup rather than a discovery.
6. The existing `webui`-without-`api` advisory applies unchanged.

## 6. Examples

### 6.1 A reachable private server — no proxy, no TLS, no root, no federation

The simplest useful deployment there is. The whole config:

```toml
[[listen]]                          # local admin; the mesh address is NOT
addr  = "127.0.0.1"                 # reachable from this host (§2.3)
port  = 3000
serve = ["api", "webui", "admin"]

[[listen_mesh]]                     # you, from anywhere
port  = 80
serve = ["api", "webui", "admin"]

[yggdrasil]
enabled = true
peers   = ["tls://peer.example:12345"]
```

Start it, and the log names the address to hand out:

```
yggdrasil: mesh up — address 201:abcd:… (key file ./data/federation.key)
listening on mesh [201:abcd:…]:80 serving [api webui admin]
listening on 127.0.0.1:3000 serving [api webui admin]
```

You administer at `http://127.0.0.1:3000/`. You — or anyone you give the address
to — reach the server at `http://[201:abcd:…]/`: brackets required, no port, from
anywhere on the mesh, behind any NAT, on any network that lets you out at all.
If you run a mesh name resolver (Alfis, or a `hosts` entry) you can hand out a
`.ygg` name instead of the literal.

This node federates with nobody. Nothing is published, no catalog is served,
port 1314 answers nothing (§4.2). It is a personal server that happens to have a
stable global address.

### 6.2 The same node, on the madnetwork

Add the feature to the transport:

```toml
[yggdrasil]
enabled = true
peers   = ["tls://peer.example:12345"]

[federation]
enabled = true
name    = "my madshare"             # display only; identity is the key
```

Same key, so **the same address** — the URL you already gave people does not
change. `[yggdrasil].enabled` is redundant here (federation implies it, §4) and
is kept only because writing it out is clearer than inferring it.

### 6.3 Public UI on the mesh, admin only at home

```toml
[[listen]]
addr  = "127.0.0.1"
port  = 3000
serve = ["api", "webui", "admin"]

[[listen_mesh]]
port  = 80
serve = ["api", "webui"]            # no admin surface on the mesh
```

### 6.4 Alongside a clearnet deployment

A mesh listener composes with everything already supported; it is one more entry
in the list, so an nginx-fronted public node can add mesh reachability without
touching its existing setup.

## 7. Exposure — what changes and what doesn't

**The audience is the whole mesh, not your community.** This is the one thing an
operator must understand. The madnetwork protocol on port 1314 answers
`federation.Audience` — a stranger is served nothing, by construction. Port 80
has none of that: it is the same login page a LAN visitor sees, offered to every
node on the Yggdrasil network. Nothing is *published* by enabling it; an
unauthenticated visitor gets the login form and the public
`GET /api/ui/config`, as they would on any other listener. But the set of people
who can reach that form grows from "my LAN" to "the mesh".

Three things happen to be *better* here than in the proxy deployment:

- **Per-client login throttling works again.** `api/login_throttle.go` keys on
  the peer address and deliberately exempts loopback, because behind nginx every
  client arrives from `127.0.0.1` and throttling it would lock out everyone at
  once — which is why `contrib/nginx/madshare-yggdrasil.conf` has to carry a
  `limit_req_zone`. A mesh peer arrives as its own `200::/7` address, so the
  app's own throttle applies per client, with no proxy config to get right.
- **Addresses are unspoofable.** Yggdrasil addresses are key-derived and routed
  cryptographically, so `allow_from` and the throttle are working with real
  identities rather than forgeable source IPs.
- **No TLS surface to misconfigure.** No certificate to renew, no expiry, no
  mixed-content, no `X-Forwarded-Proto` to get wrong.

One behaviour to note rather than fix: the session cookie is set
`Secure: r.TLS != nil` (`api/auth_handlers.go:137`), so over plain HTTP on the
mesh it is issued **without** the `Secure` flag. That is correct — a `Secure`
cookie would never be sent back over an `http://` origin and login would break —
and it costs nothing here, because the transport is encrypted a layer down and
there is no plaintext-downgrade path for an attacker to steal it over.

## 8. Deferred

- **A transport-only build tag.** The config-level split lands here (§4); the
  build-level one does not. `-tags nofederation` currently strips the mesh and
  the madnetwork feature set together, so a node that only wants to be reachable
  still carries the whole friendship/catalog/swarm layer in its binary. The
  clean end state is two tags — the yggstack/gVisor dependencies are the heavy
  half and the ones a transport-only node genuinely needs, while
  `federation/`'s own code is what it does not. Deferred because it is a
  dependency-graph and build-matrix job, not a feature: §4's config gate already
  gives operators the behaviour, and this only makes the binary smaller.
- **`serve_members_only`.** The access filter mesh listeners actually want.
  `federation/membership.go` already maintains a **mesh-address index** of every
  community member (addresses are pre-derived precisely so an inbound request
  can be matched against them), so a middleware that checks the peer address
  against that index — friends only, or the whole community — is a small piece
  of work on top of machinery that exists. This is the real answer to §7's
  "audience is the whole mesh", and `allow_from` is a poor stand-in for it.
- **UDP / QUIC on the netstack.** `stack.ListenUDP` exists; nothing needs it.
- **A `.ygg` name.** Out of scope — it is a resolver question, not a madshare one.

## 9. Implementation plan

Built as follows. No migration, no schema change, no new dependency: every piece
of machinery this needed was already in the binary and already carrying the
madnetwork protocol.

1. **`federation/mesh.go` — the transport as its own type.** `Mesh` (identity
   key, `core.Core`, netstack) with `StartTransport(config.YggdrasilConfig,
   *log.Logger)`, `Address`, `PublicKeyHex`, `DialContext`, `ListenMesh(port)`,
   `InboundReaderAlive` and `Stop`. `Node` holds a `*Mesh` where it used to hold
   `core` + `stack` and delegates to it; `Mesh.Stop` owns the teardown ordering
   (`stack.Close()` → `core.Stop()`) that `Node.Stop` used to inline. Stubs in
   `node_stub.go` for `-tags nofederation`.

   Distinct types are the point: `Deps.Federation` takes a `*Node`, so handing it
   a transport-only mesh is a compile error rather than a half-working
   `/api/madnetwork`. It is also the seam the deferred build split (§8) cuts
   along.

   `Start` **keeps its signature** and adopts a mesh via the new
   `federation.WithMesh(m)` option, building its own from `fc`'s
   key_file/peers/listen when none is given. Threading a `*Mesh` parameter
   through instead would have rewritten sixteen call sites — fifteen of them
   tests that have no interest in the distinction — to express an ownership rule
   that a doc comment states precisely: whoever creates a Mesh stops it, except
   that `Start` adopts it and `Node.Stop` then stops it.

2. **`config/mesh.go`** — `MeshListenConfig{Port, Serve, AllowFrom}` +
   `Config.ListenMesh`, `YggdrasilConfig{Enabled *bool, KeyFile, Peers, Listen}`,
   `Config.MeshEnabled()` (the §4 table, resolved once), `resolveMesh` (folds the
   deprecated `[federation]` transport aliases, derives the key path, applies the
   port default), `validateMesh` (§5) and `rejectUnknownMeshKeys` — the last
   scoped to the two new sections, so `addr` on a mesh listener is an error
   naming the reason rather than a silent no-op. `validateFederation` walks
   `[federation]` before `[yggdrasil]` so an aliased bad URI is reported against
   the section the operator actually wrote it in. Mesh entries are absent from
   the kernel bind-conflict walk by construction.

3. **`madshare.go`** — the mesh comes up when `cfg.MeshEnabled()`, the node only
   when `cfg.Federation.Enabled` (with `WithMesh`), and `deps.Federation` is
   assigned in that second branch alone (§4.2). Shutdown stops exactly one of the
   two. The `federation.Available` gate now also fires for `[yggdrasil].enabled`,
   naming whichever key asked for it (§4.1), and `requireFingerprinting` stays on
   `cfg.Federation.Enabled`.

4. **`startListeners`/`buildHandler`** — `buildHandler` now takes `serve []string,
   allow []string` rather than a `ListenConfig`, since a mesh listener has no bind
   address to give it; `startListeners` shares one `serve` closure across both
   kinds, so the two listener types cannot drift in middleware or route groups.

5. **Tests** — `config/mesh_test.go` (the gate table, both refusals, the schema
   rules, alias folding and precedence, the section-naming of URI errors, the
   admin warning) and `federation/mesh_test.go`, whose main case peers two bare
   `Mesh`es over a loopback underlay and fetches an ordinary `http.Handler` on
   port 80 across the mesh with **no `Node` on either side** — which is the claim
   the whole feature rests on. `federation/meshport_test.go` pins
   `MeshPort == config.MeshProtocolPort`, the duplicated constant the port
   reservation is checked against.

6. **Docs** — `madshare.toml.example`, `listeners-and-config.md` §4.3c / §5.3a /
   §6.7 / §7 / §8-5a, a README *Reachable from anywhere, without a reverse proxy*
   section (outside *Deploying a madnetwork node*, since it no longer requires
   federating), and a note in `contrib/nginx/README.md` demoting the yggdrasil
   vhost to the alternative route.

## 10. Decisions

Resolved with the project owner on 2026-08-02:

1. **No reverse proxy is the point.** The feature exists so a first-time
   operator can deploy a reachable node from one config file. Anything that
   reintroduces nginx, a certificate, or `setcap` into that path defeats it.
2. **Mesh-gated, not federation-gated.** `[[listen_mesh]]` requires
   `[yggdrasil].enabled` **or** `[federation].enabled` (§4), so a server can be
   reachable from anywhere while federating with nobody — the transport is the
   smaller, more general thing and the config now says so. The **build** gate is
   unchanged: federation must be compiled in either way (§4.1), and splitting
   the tags is deferred (§8).
3. **`[federation].enabled` implies `[yggdrasil].enabled`** unless the config
   says otherwise, so no existing config breaks; saying otherwise is a fatal
   error with an explanatory message, not a silent override (§4).
4. **The transport parameters move to `[yggdrasil]`**, since a transport-only
   node must be able to configure its peers without writing them under a
   `[federation]` section it has switched off. `[federation].key_file` /
   `peers` / `listen` keep working as deprecated aliases, so no existing config
   breaks (§3.2).
5. **Named `[[listen_mesh]]`.** It sits beside `[[listen]]`, and "mesh" is
   already this codebase's word for the address space (`MeshPort`, `meshAuth`,
   "mesh address", "mesh listener"). Rejected: `listen_local_ygg` — "local" is
   precisely backwards, since that listener is the most globally reachable thing
   in the file; `application_listen` — `[[listen]]` is also the application, so
   it distinguishes nothing; `yggdrasil_listen` — names the implementation, and
   would read as a lie if the underlay ever changed.
6. **`admin` on the mesh warns, it does not fail.** Administering your own node
   from your phone is the use case; the exposure is still worth one line of log
   (§5, rule 5).
