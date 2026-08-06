# Configuration model (TOML vs runtime settings)

Madshare has **two configuration tiers**, and which tier a setting belongs to is a
deliberate choice. This doc is the cross-reference other docs point at when they
add a knob. Listener/bind config detail lives in
`docs/architecture/listeners-and-config.md`; this doc is about *where settings
live and who may change them*.

## The two tiers

| | **Deploy-time config** | **Runtime settings** |
|---|---|---|
| Where | `madshare.toml` (+ `webui.toml`) | DB `settings` table (migration 006) |
| Loaded by | `config.Load` at startup (`-config` path) | `GetSetting`/`SetSetting` (`database/settings.go`) |
| Mutable at runtime? | **No** — read once at startup | **Yes** |
| Who may change it | whoever has **filesystem / deploy access** | an admin via the **web UI** (gated API) |
| Applied | on (re)start | immediately |
| Examples | `[[listen]]`, `data_dir`, `database.path`, `storage.files_dir`, `storage.variants_dir`, `[sources].symlink_roots`, `[cors]`, `[auth]` bootstrap | trash-restore policy, cover auto-derive on/off + licenses, prune summaries |

This split already exists: runtime-tunable settings (autoderive, trash-policy)
are stored in the `settings` table and edited via `/api/admin/settings/*`
(`api/access_handlers.go`); **nothing writes the TOML back** — it is read-only at
runtime.

## Which tier for a new setting?

- **Deploy-time (TOML)** when it is *structural*, *security-relevant*, or *should
  require host access to change*: where data lives, what ports bind, the
  first-admin bootstrap, and **allow-lists / trust boundaries**.
- **Runtime (DB settings)** when it is an *operational toggle* an admin should be
  able to flip without touching the box: behaviour policies, feature on/off,
  thresholds.

### When a setting needs to be both: the override layer

The swarm rate caps (`docs/architecture/swarm-admin.md`) were the first knob that
genuinely needed **both** tiers, which is the case the OPEN section below parked.
The answer, and the pattern to copy:

> **TOML stays the deploy-time default. The DB carries an optional *override*.
> Resolution is `runtime override → config → built-in default`, and nothing ever
> writes the TOML back.**

Concretely: `[federation] seed_rate_kib` / `fetch_rate_kib` are what the node
starts with, and `swarm.up_rate_kib` / `swarm.down_rate_kib` in `settings` are
what an admin may set from `/admin/swarm` while the link is saturated. An **unset**
settings key means "no override" — deliberately distinct from a stored `0`, which
means unlimited and is a real override (it is how one node escapes a cap its
deployment ships with). That three-valued shape is the whole trick, and it is the
same one `share_depth` uses for absent / inherit / pinned.

This keeps every property tier (A) was chosen for: the file remains
operator-owned and readable, there is no write-back and no comment destruction,
and there is no split-brain over authority — the two layers do not compete,
because one is explicitly a *default* and the other explicitly an *override*.

It does **not** extend to trust boundaries. `symlink_roots` gets no override
layer, for the reason in the next section: a boundary a web admin can move is not
a boundary.

### Security: some things are TOML *on purpose*

`[sources].symlink_roots` (the import allow-list) is the clearest case. Its value
as a boundary is precisely that **changing it requires filesystem/deploy access**.
If a web-admin could edit it from the UI, it would stop protecting anything — a
compromised admin session could add `/` and symlink `/etc/shadow` into the
library. So it stays deploy-time TOML, **not** UI-editable, by design. The same
logic applies to any future trust boundary.

## webui.toml

A separate `webui.toml` (loaded via `-webui-config`, mapped to `config.UIConfig`)
holds UI-side controls served verbatim at `GET /api/ui/config` (public — the
upload page needs it pre-login). It is deploy-time like `madshare.toml`. Distinct
from the `[webui]` section of `madshare.toml` (`config.WebUIConfig`).

---

## OPEN — to discuss (not decided)

**How should UI-editable configuration work going forward?** Raised because the
data-sources feature surfaced UI controls that *look* like config. Two directions:

- **(A) DB settings, TOML stays static** *(matches what the codebase already
  does)* — every runtime-editable setting goes in the `settings` table (like
  autoderive/trash-policy); TOML remains deploy-time, read-only at runtime,
  operator-controlled. No TOML write-back. `symlink_roots` stays operator-only.
  - For data-sources specifically this means **no TOML write-back is needed**:
    symlink *sources* already live in the DB (`data_sources`), and the allow-list
    is intentionally operator-only.
- **(B) Live TOML editor** — the service writes changes back to `madshare.toml`
  and re-reads on (re)start. One place to edit, but: destroys the file's comments,
  creates file-vs-DB split-brain over authority, needs atomic-write + permission
  handling, usually still needs a restart to apply, and **weakens any allow-list
  as a boundary** (a UI that can edit it defeats the point).

Owner has parked the decision. My lean is **(A)**; nothing in data-sources v0 is
blocked by it either way.

**Revisited 2026-08-06**, when the swarm rate caps became the first setting
needing to be both UI-editable and TOML-sourced: **(A) holds**, with the override
layer described above. No TOML write-back was needed then and none is now.

## Related

- `docs/architecture/listeners-and-config.md` — the `[[listen]]` model + validation.
- `docs/architecture/data-sources.md` — `data_dir`, `[sources].symlink_roots`.
