# Listeners, route groups, and the single-origin web UI

**Status:** design / proposal (not yet implemented)
**Supersedes:** the `[api]`/`[webui]` `addr` + `public_url` config layout
**Related:** future auth layer (out of scope here)

## 1. Motivation

Today the server runs two HTTP servers on two ports — the API (`:3000`) and the
web UI (`:8080`) — and the browser is told the API's absolute address through
the `public_url` config value, which the web UI injects into a
`<meta name="api-url">` tag. That value is then the base for every `fetch()` the
front-end makes.

`public_url` exists *only* because the two ports are two separate origins, so the
browser needs an absolute URL to cross from the page's origin to the API's. It is
a hardcoded address that has to be re-set for every deployment (LAN IP, ygg
address, behind TLS, …), which is brittle and does not survive the planned move
to a yggdrasil mesh + optional clearnet/TLS.

This proposal removes `public_url` from the normal path and replaces the
two-fixed-ports layout with **one process that opens a list of listeners**, each
of which serves a chosen set of **route groups**. The web UI is served
**same-origin** with the API, so the front-end uses **relative URLs** and needs
no address configuration at all.

### What this is and isn't

- It **is** a way to control, per network interface, *which surface of the
  server is reachable there* — e.g. expose the API broadly but keep the web UI
  and the (currently destructive, unauthenticated) admin endpoints on loopback.
- It is **not** an authentication or authorization mechanism. Binding the admin
  surface to loopback narrows where it is reachable; it does not protect it.
  Real protection is the auth layer, which is deliberately deferred. Do not
  treat `serve` lists as access control.

### Goals

1. Drop `public_url` from the common case; the in-process web UI talks to the
   API over relative, same-origin URLs.
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

| Group   | Routes                                                                 | Purpose |
|---------|------------------------------------------------------------------------|---------|
| `api`   | `GET /` health, `/api/*` (library), `/files/*`, `/images/*`            | The machine-facing API. This is the product. |
| `webui` | `/` (library page), `/cmus`, `/static/*`                               | The bundled reference browser UI. |
| `admin` | `/api/admin/*` (delete, prune) and the `/admin` page                   | Destructive operations + their UI. Currently unauthenticated. |

Notes:

- The bundled `webui` reaches the API with **relative** URLs, so a listener that
  serves `webui` should also serve `api` — otherwise the page loads but every
  `fetch` 404s. Validation warns on `webui` without `api` (see §6).
- `admin` is its own group so it can be scoped independently of `api` (e.g.
  loopback only) until auth exists. The destructive `/api/admin/*` endpoints and
  the `/admin` page move together as one unit.
- When the web UI is compiled out (§5), the `webui` and the `/admin` *page* part
  of `admin` do not exist; the `/api/admin/*` endpoints still do.

### 2.3 Same-origin, relative front-end

Because the web UI and the API are served from the same listener (same origin),
the front-end no longer needs an absolute base. The `meta[name="api-url"]`
mechanism stays, but its default becomes **empty**:

```js
// before: const API = meta?.content || 'http://localhost:3000';
const API = document.querySelector('meta[name="api-url"]')?.content || '';
fetch(`${API}/api/artists`);   // -> fetch('/api/artists'), resolved same-origin
```

`public_url` is gone from the in-process path. The only remaining use of an
absolute base is the **separately built** web UI talking to a remote backend,
which sets the meta tag explicitly. That is exposed as an optional, empty-by-
default `api_base` (§4.3), never required for the bundled server.

## 3. The exposure problem (why a *list* of listeners)

A single listener bound to several addresses serves the *same* routes to anyone
who can reach any of those addresses — exposure is decided at the listener,
before routing. So "web UI for me, API for everyone" cannot be expressed with
one listener. It needs two:

- a loopback listener serving the full stack (`api` + `webui` + `admin`), and
- a public/ygg listener serving only `api`.

The local user loads `http://127.0.0.1:3000/`, the page's relative `fetch`es hit
the **same** loopback listener (which also serves `api`) — works, no
`public_url`. Remote callers hit the public listener's `/api/...`; `/` and
`/admin` 404 there because those groups were never mounted on it.

## 4. Config schema

The `[api]` and `[webui]` sections (with `addr` / `public_url`) are **replaced**
by a `[[listen]]` array of tables. `[database]` and `[storage]` are unchanged.

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

Included in the first cut (see §9, decision 4).

### 4.3 `api_base` (optional, rare)

```toml
[webui]
api_base = ""   # default empty -> relative, same-origin URLs
```

Only set this for a **separately deployed** web UI that must point at a remote
API origin. For the normal bundled server, leave it empty. When non-empty it is
injected into `meta[name="api-url"]`; when empty the meta tag is empty and the
front-end uses relative URLs. (This is the spiritual successor to `public_url`,
demoted from required to optional.)

### 4.4 Defaults (no config file, or omitted keys)

If no `[[listen]]` is given, the default is a single loopback listener serving
the full stack — the safe default while admin is unauthenticated:

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

## 7. Implementation plan

Ordered so each step compiles and tests green.

### Step 1 — config schema (`config/config.go`, `config/config_test.go`)

- Remove `APIConfig`, `WebUIConfig`, the `Addr`/`PublicURL` fields, and their
  defaults.
- Add:
  ```go
  type ListenConfig struct {
      Addr      string   `toml:"addr"`
      Port      int      `toml:"port"`
      Serve     []string `toml:"serve"`
      AllowFrom []string `toml:"allow_from"`
  }
  type WebUIConfig struct { APIBase string `toml:"api_base"` } // optional override
  type Config struct {
      Listen   []ListenConfig `toml:"listen"`
      WebUI    WebUIConfig    `toml:"webui"`
      Database DatabaseConfig `toml:"database"`
      Storage  StorageConfig  `toml:"storage"`
  }
  ```
- `defaults()` returns the single loopback full-stack listener (§4.4).
- Extend `validate()` with the §6 rules. Add a `(ListenConfig) BindAddr() string`
  helper returning `net.JoinHostPort(addr, port)`.
- Define the group token set as exported constants so `main`/`api`/`webui` agree.
- Update `config_test.go`: drop `public_url`/`addr` cases; add listener parsing,
  the overlap rule, unknown-group, empty-`serve`, and webui-without-api warning.

### Step 2 — make route groups mountable (`api/api.go`, `webui/webui.go`)

The blocker today: `api.NewRouter` builds the whole tree (including `/`,
`/files`, `/images`, and `/api/admin`) and `webui.Route` calls
`http.ListenAndServe` itself. Both need to become *register onto a router I give
you* functions so a per-listener handler can compose any subset.

- `api` package — split registration by group:
  ```go
  func RegisterAPI(r chi.Router, deps Deps)    // /, /api/* (non-admin), /files/*, /images/*
  func RegisterAdmin(r chi.Router, deps Deps)  // /api/admin/*
  ```
  Bundle `store, repo, cacheDir, filesDir, maxUploadSize` into a `Deps` struct to
  avoid the long parameter list. Keep `corsMiddleware`; it becomes harmless
  same-origin and still helps non-browser/CORS clients (revisit later — with
  same-origin we may make CORS opt-in).
- `webui` package — stop owning the listener:
  ```go
  func Register(r chi.Router, apiBase string)  // /, /cmus, /static/*
  func RegisterAdminPage(r chi.Router, apiBase string) // /admin
  ```
  Templates/static stay disk-relative for now (see Step 5 for embedding).

### Step 3 — per-listener handler + multi-listener serving (`madshare.go`)

Replace the two fixed `wg.Go` blocks with:

- A `buildHandler(groups, deps, apiBase) http.Handler` that creates a
  `chi.NewRouter()`, applies shared middleware (logger, recoverer, and
  `allowFrom` if set), then calls the `Register*` funcs for the groups in the
  set. Register the more specific API prefixes so the web UI's `/` does not
  shadow `/api`/`/files`/`/images` (chi pattern precedence handles this; verify
  `/` is an exact route, not a catch-all).
- For each `ListenConfig`: `net.Listen("tcp", lc.BindAddr())`, construct an
  `http.Server{Handler: ...}`, and `wg.Go(func(){ srv.Serve(ln) })`.
- Add graceful shutdown: trap SIGINT/SIGTERM, `srv.Shutdown(ctx)` each server.
  (Current code has none; good time to add it.)

### Step 4 — front-end relative URLs (`webui/static/js/{app,cmus,admin}.js`)

- Change each `const API = … || 'http://localhost:3000'` to `|| ''`.
- Confirm every call site is `` `${API}/...` `` with a leading slash so the empty
  base yields a valid root-relative path. (Grep shows `/api/...`, `/files/upload`,
  `/api/admin/...`, `/api/files` — all already root-anchored.)
- The injected `meta[name="api-url"]` content becomes `{{.APIBase}}` (empty by
  default).

### Step 5 — embed templates & static (follow-up, optional but recommended)

Move `webui/html/*` and `webui/static/*` to `embed.FS` so the compiled-in UI has
no CWD dependency (removes the "must run from project root" constraint for the
UI). Natural partner to the build-tag work.

### Step 6 — compile-out build tag (webui)

- Put the web-UI registration behind a build constraint. Two files exposing the
  same symbols:
  - `webui/register.go`        `//go:build !nowebui`  → real `Register`/`RegisterAdminPage`
  - `webui/register_stub.go`   `//go:build nowebui`   → stubs; a sentinel
    `Available = false` the config validator checks (§6 rule 5).
- Default `go build` includes the UI; `go build -tags nowebui ./...` strips it,
  the templates/static, and the `tag`/template deps from a pure-API binary.

### Step 7 — docs & example config

- Rewrite `madshare.toml.example` to the `[[listen]]` layout with the §5
  examples as comments.
- Update `CLAUDE.md` (Configuration + Architecture sections) and `README`/concept
  doc references from "two ports / public_url" to "listeners + route groups".

## 8. Breaking changes

- `[api].addr`, `[api].public_url`, `[webui].addr` are **removed**. Existing
  `madshare.toml` files must migrate to `[[listen]]`. Acceptable: pre-release v0,
  config file is gitignored and local.
- The web UI no longer has its own port (default `:8080` is gone); it shares a
  listener with the API.
- `public_url` behaviour is replaced by relative URLs + optional `[webui].api_base`.

## 9. Decisions

Resolved with the project owner on 2026-05-29:

1. **Build-tag polarity:** UI **in** by default. Plain `go build` / `go run`
   includes the web UI; `go build -tags nowebui ./...` strips it (and the
   templates/static/`tag` deps) for a pure-API binary. (Step 6 reflects this.)
2. **`admin` group:** the `/admin` page and the destructive `/api/admin/*` API
   stay **coupled** as one `admin` group and move together. Revisit only when
   auth lands.
3. **Same port across listeners:** **allowed.** Reusing one port across listeners
   bound to *different specific addresses* (e.g. `127.0.0.1:3000` +
   `<ygg-addr>:3000`) is supported; validation still rejects the
   `0.0.0.0`/`[::]`-vs-loopback overlap (§5.2 gotcha, §6 rule 3).
4. **`allow_from`:** **included in v1**, not deferred. Per-listener CIDR allowlist
   middleware (`403` on non-match), meaningful over yggdrasil's unspoofable
   addresses; convenience/defense-in-depth on clearnet. Folds into the Step 3
   `buildHandler` middleware.

Still genuinely open (revisit alongside auth):

- **CORS once same-origin.** Keep permissive CORS for non-browser/remote clients
  for now, or make it opt-in per listener? Leaning keep; defer the call to the
  auth work.
```
