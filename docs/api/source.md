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
- A "Source" link pointing to `/source` (with the `download` attribute) appears
  in the navigation header of every web UI page.
