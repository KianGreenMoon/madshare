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
| `503 Service Unavailable` | The server could not run `git ls-files` (git not available, or not running from a git repository). |
| `404 Not Found` | `SourceRoot` was not configured (should not happen in a standard deployment). |

### Implementation notes

- The archive is built once on the first request by running `git ls-files` in
  the working directory the server was started from (resolved at startup with
  `os.Getwd()`), then cached in memory for subsequent requests.
- Because `git ls-files` enumerates only **tracked** files, the archive reflects
  whatever is committed (plus any staged/tracked-but-modified files on disk at
  the time of first build). Instance-specific config files (`madshare.toml`,
  `webui.toml`) and runtime data (`data/`) are gitignored and therefore absent.
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

- Shares the `SourceRoot` dependency with `/source`: the file is read from
  `<SourceRoot>/LICENSE.md` and cached in memory on first request. When
  `SourceRoot` is empty (endpoint disabled), `/license` returns 404 just like
  `/source`.
- The endpoint is **public** — no authentication required.
- A "License" link pointing to `/license` (opening in a new tab) appears in the
  navigation header of every web UI page.
