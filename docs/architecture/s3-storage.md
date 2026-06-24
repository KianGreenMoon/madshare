# Storage priority & object storage (S3) — future

> **Status: design notes for a feature that does not exist yet.** Madshare v0 has
> exactly two storages — `local` and `links`, one each — with a **fixed**
> precedence (`local` before `links`) and no configurable ordering. See
> `docs/architecture/data-sources.md` for what is actually built. This document
> captures the precedence logic that only becomes meaningful once a second
> *interchangeable, uploadable* storage (an object store) exists, so the reasoning
> isn't lost. Nothing here is implemented.

## Why precedence is trivial today and convoluted later

In v0 the resolver "tries storages in order", but the order is the constant
`[local, links]`: `links` can never outrank `local`, and uploads only ever land
in `local`, so there is nothing to choose. Add S3 — an uploadable store that may
serve as the primary, an overflow tier, or a cache — and several knobs appear at
once. The rules below look tangled precisely because they encode those choices.

## The general model

Storages have a **priority order** (a list) whose head is the **default**. The
resolver probes storages in priority order and serves the first that has the
hash. Two settings — `settings.storage_order` (JSON array) and
`settings.storage_default` — hold it; an admin reorders them and picks the default.

### Precedence rules

- **`local` before `s3` by default.** The owned local blob store is canonical; the
  resolver prefers it unless the admin deliberately raises another store above it.
- **`links` can never outrank `local`**, and is **never uploadable** — imports
  only. It stays a constrained special storage no matter how the rest are ordered.
- **Multiple object stores** (e.g. two buckets/regions) are ordered among
  themselves by the same list.
- **The default storage is the upload target.** A new upload lands in the default
  store; it goes elsewhere only if an admin manually moves it.
- **An admin may raise `s3` above `local`** — S3-as-primary, or an S3 cache tier
  that should be hit first. Then S3 outranks `local` for resolution (and, when S3
  is the default, for uploads too).

### The "local-first vs priority-order" apparent contradiction

Two statements seem to conflict: *"probe local-filesystem stores first"* and
*"probe in the admin's priority order (which could put S3 first)."* They don't,
once you separate **logical priority** from **how existence is checked**:

- **Logical priority** (the configured order) decides *which copy is preferred*.
- **Existence checking** differs by storage kind. A local-fs store (`local`,
  `links`) is checked with a cheap `stat`. An object store is **never** checked
  with a blind per-request network probe; its presence comes from a
  known-location lookup / index / presence cache. "Don't blind-probe S3" is a
  rule about *how* we ask, not *whether* S3 may rank first.

So when S3 is ranked above `local`, the resolver consults S3's index (cheap), not
a network round-trip per play; and when S3 sits below `local` (the default),
`local`'s cheap `stat` usually answers first anyway.

## UI (S3 era)

Storages become a **reorderable list** with a default-for-uploads selector.
`links` is shown as a constrained, non-reorderable special entry — or kept in its
own *Imported directories* submenu, as in v0. It is not a peer you can drag above
`local`, and not an upload target.

## Resolver caching

An object store likely warrants a **presence cache / location index** so the
resolver answers "does S3 have this hash?" without a network call, and so
`storageStats` can account S3 without walking the bucket on every request.

## Open

- Is `local` an immovable floor, or fully reorderable against the object stores?
  Leaning: `links` is pinned lowest and never uploadable; `local` and the object
  stores are freely orderable above it.
- Tiering / migration policy (auto-move cold blobs to S3) is a separate concern.
- Relationship to **adopt/materialize** (copy a `links` file into `local`): the
  same machinery could move a blob between any two stores.
