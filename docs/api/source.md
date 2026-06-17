# Source Archive API

Madshare is released under the **GNU Affero General Public License v3 (AGPL-3.0)**.
AGPL §13 requires that users who interact with the server over a network be able to
receive the Corresponding Source of the running code. This endpoint fulfils that
obligation.

---

## `GET /source`

Downloads the complete source archive of the running server.

### Response

| Header              | Value                                                 |
|---------------------|-------------------------------------------------------|
| `Content-Type`      | `application/gzip`                                    |
| `Content-Disposition` | `attachment; filename="madshare-source.tar.gz"`     |

The body is a gzip-compressed tar archive (`tar.gz`) containing every file
tracked by git in the project root — i.e. everything that appears in
`git ls-files`. Files listed in `.gitignore` and the `.git/` directory are
excluded automatically.

```bash
curl -OJ http://localhost:3000/source
# saves: madshare-source.tar.gz
```

### Error responses

| Status | Meaning |
|--------|---------|
| `503 Service Unavailable` | No archive was embedded (dev build) **and** the server could not run `git ls-files` (git not available, or not running from a git repository). A binary built via `make build` never hits this. |
| `404 Not Found` | Neither an embedded archive nor a `SourceRoot` was configured (should not happen in a standard deployment). |

### Implementation notes

- **Release builds embed the archive at build time.** `make build` (and
  `make build-nowebui`) run `git archive HEAD` to package the tracked files at
  the built commit into `source.tar.gz`, then compile with `-tags embedsource`,
  which `//go:embed`s that tarball into the binary (`source.go`). An
  installed binary therefore carries its own Corresponding Source and serves
  `/source` with **no git checkout or working tree** present — the relevant
  case for AGPL compliance. `source.tar.gz` is a generated artifact (gitignored)
  and is only referenced by the build-tagged file, so plain `go build`/`go run`
  never need it.
- **Dev builds fall back to git.** Without the `embedsource` tag the embedded
  archive is `nil`, so on the first request the server runs `git ls-files` in
  the working directory it was started from (resolved at startup with
  `os.Getwd()`, carried as `Deps.SourceRoot`) and builds the tar.gz on the fly.
  The result is cached in memory either way.
- The embedded archive reflects the **committed** source at the built commit
  (`git archive HEAD`); the dev fallback reflects the **tracked** files as they
  exist on disk (including staged/modified content). Instance-specific config
  files (`madshare.toml`, `webui.toml`) and runtime data (`data/`) are gitignored
  and therefore absent from both.
- `go install`-style builds that skip the Makefile do **not** set the tag and so
  fall back to the git path; that is fine for development from a checkout but
  will return `503` if run from a tree without git. Distribute binaries built via
  the Makefile.
- The endpoint is **public** — no authentication required. AGPL demands source
  be freely available to anyone interacting with the service.
- The web UI's "Source" nav link is currently **hidden** (its markup is kept
  commented in the header templates): downloading a tar.gz next to a running
  deployment proved inconvenient, and the header now carries a **GitRepo**
  button instead (`[webui].git_repo`, see
  `docs/architecture/listeners-and-config.md` §4.3a). The endpoint itself
  remains live and public — it, not the GitHub link, is the binding
  corresponding-source offer for a modified deployment.

---

## `GET /license`

Serves the bundled `LICENSE.md` (the full AGPL-3.0 text) for the running server.

### Response

| Header                  | Value                       |
|-------------------------|-----------------------------|
| `Content-Type`          | `text/plain; charset=utf-8` |
| `X-Content-Type-Options`| `nosniff`                   |

The body is the verbatim `LICENSE.md` from the project root, served inline so a
browser displays it as plain text rather than downloading it.

```bash
curl http://localhost:3000/license
```

### Error responses

| Status | Meaning |
|--------|---------|
| `503 Service Unavailable` | `LICENSE.md` could not be read from `SourceRoot`. |
| `404 Not Found` | `SourceRoot` was not configured (should not happen in a standard deployment). |

### Implementation notes

- `LICENSE.md` is **always embedded** into the binary unconditionally
  (`//go:embed LICENSE.md` in `madshare.go`, passed as `Deps.LicenseText`), so
  `/license` works in every build — no build tag and no working tree required.
  Only when those embedded bytes are absent (e.g. the api package's own
  `NewRouter` in tests) does it fall back to reading `<SourceRoot>/LICENSE.md`
  from disk. The served bytes are cached in memory on first request. With no
  embedded text and no `SourceRoot`, `/license` returns 404 like `/source`.
- The endpoint is **public** — no authentication required.
- A "License" link pointing to `/license` (opening in a new tab) appears in the
  navigation header of every web UI page.
