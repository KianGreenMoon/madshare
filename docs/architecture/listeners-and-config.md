# Listeners, route groups, and the single-origin web UI

**Status:** implemented
**Related:** the auth layer (`docs/architecture/auth.md`) is what actually
protects routes; the listener/`serve` split is deployment topology, not access
control.

## 1. Motivation

The server is **one process that opens a list of listeners**, each of which
serves a chosen set of **route groups**. The web UI is served **same-origin**
with the API, so the front-end uses **relative URLs** and needs no per-deployment
address configuration — important for a yggdrasil mesh + optional clearnet/TLS,
where a hardcoded absolute base would have to be re-set for every deployment
(LAN IP, ygg address, behind TLS, …).

### What this is and isn't

- It **is** a way to control, per network interface, *which surface of the
  server is reachable there* — e.g. expose the API broadly but keep the web UI
  and the admin endpoints on loopback.
- It is **not** an authentication or authorization mechanism. Binding the admin
  surface to loopback narrows where it is reachable; it does not protect it.
  Real protection is the auth layer (`docs/architecture/auth.md`), which gates
  the admin group via `auth.RequirePermission`. Do not treat `serve` lists as
  access control.

### Goals

1. No per-deployment URL config in the common case; the in-process web UI talks
   to the API over relative, same-origin URLs.
2. One process, one config file, one or more listeners.
3. Each listener independently chooses which route groups it serves.
4. The web UI is **optional**: it can be turned off at runtime, and **compiled
   out entirely** so the binary ships as a pure API server (the API is the
   product; the bundled UI is a reference client).
5. Keep the door open for a *separately built* web UI that points at a remote
   backend — via an explicit, empty-by-default override, not a required value.

## 2. Concepts

### 2.1 Listeners

A **listener** is one bound `address:port` the process accepts connections on.
The config carries a list of them. Internally each listener gets its own
`net.Listener` and its own `http.Handler`, all driven by `http.Server.Serve` in
separate goroutines within the single process.

### 2.2 Route groups

Every route the server exposes belongs to exactly one **group**. A listener's
`serve` list names the groups mounted on that listener; routes outside the list
are simply never registered on that listener's handler (a request for them gets
`404`).

| Group   | Routes                                                                          | Purpose |
|---------|---------------------------------------------------------------------------------|---------|
| `api`   | `/healthz`, `/source`, `/license`, `/api/*` (library), `/files/*`, `/images/*`  | The machine-facing API. This is the product. |
| `webui` | `/` (library page), `/cmus`, `/static/*`                                         | The bundled reference browser UI. |
| `admin` | `/api/admin/*` (delete, prune) and the `/admin` page                             | Destructive operations + their UI. The API is gated by `auth.RequirePermission`. |

Notes:

- The web UI owns `/`, so the health check is `GET /healthz`, not `/`.
- The bundled `webui` reaches the API with **relative** URLs, so a listener that
  serves `webui` should also serve `api` — otherwise the page loads but every
  `fetch` 404s. Validation warns on `webui` without `api` (see §6).
- `admin` is its own group so it can be scoped independently of `api` (e.g.
  loopback only) on top of the auth gating. The destructive `/api/admin/*`
  endpoints and the `/admin` page move together as one unit.
- When the web UI is compiled out (§5), the `webui` and the `/admin` *page* part
  of `admin` do not exist; the `/api/admin/*` endpoints still do.

### 2.3 Same-origin, relative front-end

Because the web UI and the API are served from the same listener (same origin),
the front-end needs no absolute base. The `meta[name="api-url"]` tag defaults to
**empty**, so calls resolve relative to the page origin:

```js
const API = document.querySelector('meta[name="api-url"]')?.content || '';
fetch(`${API}/api/artists`);   // -> fetch('/api/artists'), resolved same-origin
```

The only use of an absolute base is the **separately built** web UI talking to a
remote backend, which sets the meta tag explicitly via the optional, empty-by-
default `api_base` (§4.3) — never required for the bundled server.

## 3. The exposure problem (why a *list* of listeners)

A single listener bound to several addresses serves the *same* routes to anyone
who can reach any of those addresses — exposure is decided at the listener,
before routing. So "web UI for me, API for everyone" cannot be expressed with
one listener. It needs two:

- a loopback listener serving the full stack (`api` + `webui` + `admin`), and
- a public/ygg listener serving only `api`.

The local user loads `http://127.0.0.1:3000/`, the page's relative `fetch`es hit
the **same** loopback listener (which also serves `api`) — works, with no URL
config. Remote callers hit the public listener's `/api/...`; `/` and `/admin`
404 there because those groups were never mounted on it.

This is independent of auth: even where `/api/admin/*` *is* mounted, the auth
layer still gates it. The `serve` split decides reachability per socket; auth
decides who may call what.

## 4. Config schema

Listeners are configured with a `[[listen]]` array of tables; `[webui]`,
`[database]`, and `[storage]` round out the file.

### 4.1 `[[listen]]`

```toml
[[listen]]
addr  = "127.0.0.1"            # bind address; "0.0.0.0", "[::]", a LAN/ygg addr, or "" = all interfaces
port  = 3000                   # 1..65535
serve = ["api", "webui", "admin"]   # subset of: api, webui, admin
# allow_from = ["127.0.0.0/8", "::1/128"]   # optional source allowlist (see §4.2)
```

- `addr` — bind address. Empty string or `"0.0.0.0"`/`"[::]"` means all
  interfaces. Use a specific address to scope to one interface. IPv6 literals
  may be given with or without brackets.
- `port` — TCP port, `1`–`65535`.
- `serve` — non-empty subset of the known groups.
- `allow_from` — optional; see §4.2.

### 4.2 `allow_from` (optional)

A list of CIDRs; if present, the listener accepts requests only from matching
source addresses (others get `403`). This is an application-layer filter applied
in middleware, **not** a substitute for auth:

- On **yggdrasil**, addresses are cryptographic and unspoofable, so a CIDR
  allowlist over ygg space is meaningful.
- On **clearnet**, treat it as convenience/defense-in-depth, not security; source
  IPs can be spoofed or NAT'd and `X-Forwarded-*` is not consulted.

See §8, decision 4.

### 4.3 `api_base` (optional, rare)

```toml
[webui]
api_base = ""   # default empty -> relative, same-origin URLs
```

Only set this for a **separately deployed** web UI that must point at a remote
API origin. For the normal bundled server, leave it empty. When non-empty it is
injected into `meta[name="api-url"]`; when empty the meta tag is empty and the
front-end uses relative URLs.

### 4.4 Defaults (no config file, or omitted keys)

If no `[[listen]]` is given, the default is a single loopback listener serving
the full stack — the safe default:

```toml
[[listen]]
addr  = "127.0.0.1"
port  = 3000
serve = ["api", "webui", "admin"]
```

`[storage]` / `[database]` defaults are unchanged (`./data/madshare.db`,
`./data/files`, `max_upload_mb = 500`).

## 5. Examples

### 5.1 Local development — everything on one loopback port

```toml
[[listen]]
addr  = "127.0.0.1"
port  = 3000
serve = ["api", "webui", "admin"]
```

Open `http://127.0.0.1:3000/`. UI, API, and admin all here. No URL config.

### 5.2 Web UI for me, API for everyone (the asymmetric case)

```toml
[[listen]]                       # full stack, loopback only
addr  = "127.0.0.1"
port  = 3000
serve = ["api", "webui", "admin"]

[[listen]]                       # API only, reachable by others
addr  = "201:abcd:...:1"         # the ygg (or LAN) address of THIS host — not 0.0.0.0
port  = 3000
serve = ["api"]
```

> **Gotcha:** do not pair `127.0.0.1:3000` with `0.0.0.0:3000` — `0.0.0.0`
> includes loopback, so the two bindings collide on the same port. Bind the
> second listener to a *specific* non-loopback address (as above), or give it a
> different port.

### 5.3 Pure API node (UI compiled out)

```toml
[[listen]]
addr  = ""          # all interfaces
port  = 3000
serve = ["api"]     # "webui"/"admin"-page do not exist in this binary
```

Built with the web UI excluded:

```bash
go build -tags nowebui ./...
```

### 5.4 Admin only on loopback, API + UI on the LAN

```toml
[[listen]]
addr  = "127.0.0.1"
port  = 3000
serve = ["api", "webui", "admin"]

[[listen]]
addr  = "192.168.1.67"
port  = 3000
serve = ["api", "webui"]     # no admin surface on the LAN
```

## 6. Validation rules (`config.Load`)

`config.Load` keeps validating up front and aborting startup on error. New
checks:

1. At least one `[[listen]]` after defaults are applied.
2. Each listener: `port` in `1..65535`; `addr` parses (empty allowed); `serve`
   non-empty and every token is a known group (`api`/`webui`/`admin`).
3. No two listeners bind the same `addr:port`. Detect the `0.0.0.0`/`[::]` vs
   specific-address overlap and reject with a clear message (§5.2 gotcha).
4. If any listener lists `webui` but not `api`, **warn** (the UI would load but
   its fetches would 404). Warn, don't fail — a reverse-proxy operator might do
   this deliberately.
5. If the binary was built with `-tags nowebui` and a listener requests `webui`
   (or the admin *page*), fail with a message pointing at the build tag.
6. `allow_from` entries (if present) parse as CIDRs.

Existing `[storage]` checks are unchanged (`files_dir` non-empty;
`max_upload_mb` in `[1, MaxUploadMBLimit]`).

## 7. Where it lives in the code

The design above is implemented as follows:

- **Config schema** — `config/config.go`: `ListenConfig` (`addr`/`port`/`serve`/
  `allow_from`), `WebUIConfig.APIBase`, the exported group-token constants, the
  `defaults()` single-loopback listener, and the §6 validation in `validate()`
  (with the `(ListenConfig) BindAddr()` helper). Tests in `config/config_test.go`.
- **Mountable route groups** — `api/api.go` exposes `RegisterAPI` / `RegisterAdmin`
  (bundling deps into `api.Deps`); `webui` exposes `Register` / `RegisterAdminPage`.
- **Per-listener serving** — `madshare.go` `buildHandler` composes a `chi.Router`
  per listener with shared middleware (Logger, Recoverer, `api.CORS`,
  `auth.Identify`, and the `allow_from` filter), mounts only the requested groups,
  and `startListeners` runs one `http.Server` per `[[listen]]` with graceful
  SIGINT/SIGTERM shutdown.
- **Same-origin front-end** — the web UI reads `meta[name="api-url"]`
  (= `[webui].api_base`); empty → relative URLs.
- **Compile-out web UI** — `-tags nowebui`; `webui.Available` is the sentinel the
  validator checks (§6 rule 5).

## 8. Decisions

Resolved with the project owner on 2026-05-29:

1. **Build-tag polarity:** UI **in** by default. Plain `go build` / `go run`
   includes the web UI; `go build -tags nowebui ./...` strips it (and the
   templates/static/`tag` deps) for a pure-API binary.
2. **`admin` group:** the `/admin` page and the destructive `/api/admin/*` API
   stay **coupled** as one `admin` group and move together.
3. **Same port across listeners:** **allowed.** Reusing one port across listeners
   bound to *different specific addresses* (e.g. `127.0.0.1:3000` +
   `<ygg-addr>:3000`) is supported; validation still rejects the
   `0.0.0.0`/`[::]`-vs-loopback overlap (§5.2 gotcha, §6 rule 3).
4. **`allow_from`:** **included**, not deferred. Per-listener CIDR allowlist
   middleware (`403` on non-match), meaningful over yggdrasil's unspoofable
   addresses; convenience/defense-in-depth on clearnet. Runs in the
   `buildHandler` shared middleware.

5. **CORS once same-origin.** Kept: `api.CORS` runs as shared middleware on every
   listener. It is harmless same-origin and still helps non-browser/remote
   clients. (Whether to make it opt-in per listener can be revisited later.)
```
