# API Tokens

Personal **API tokens** let non-browser clients (scripts, `curl`, a desktop
player) authenticate to Madshare without a session cookie. A token belongs to one
user and carries **that user's permissions** — it is an alternative credential for
the same account, not a separate access level.

Browsers use the session cookie from `POST /api/auth/login`; everything else uses
a token. The web UI manages tokens on the **Settings** page
(`docs/ui/user-settings.md`); this document covers the HTTP API behind it.

Related: `docs/architecture/auth.md` (the auth model).

---

## Authenticating with a token

Send the token in the `Authorization` header with the `Bearer` scheme:

```
Authorization: Bearer <token>
```

```bash
# Reach a protected endpoint as the token's owner.
curl -H "Authorization: Bearer $MADSHARE_TOKEN" \
     http://localhost:3000/api/auth/me
```

How a request is resolved (`auth.Identify`):

1. If a valid `madshare_session` **cookie** is present, it wins.
2. Otherwise, if `Authorization: Bearer <token>` resolves to a live token, the
   request acts as that token's user.
3. Otherwise the request is **anonymous**.

The scheme name is case-insensitive (`Bearer`, `bearer`). Identification never
rejects a request on its own — an unrecognised or anonymous request simply has no
identity, and the endpoint's own authorization decides the outcome (a protected
endpoint then returns `401`). An **expired** or **revoked** token resolves to no
identity, i.e. it is treated exactly like an anonymous request.

The token is an opaque URL-safe Base64 string (32 random bytes, ~43 characters,
no prefix). Only its SHA-256 hash is stored server-side, so the raw value is
shown **once**, at creation, and can never be retrieved again.

---

## Create a token — `POST /api/auth/tokens`

Requires an authenticated caller (a session cookie **or** an existing token). The
token is created for the **calling** user.

### Request body

| Field             | Type   | Required | Meaning                                                                 |
|-------------------|--------|----------|-------------------------------------------------------------------------|
| `name`            | string | yes      | A label to recognise the token later (e.g. `"laptop cmus"`).            |
| `expires_at`      | int    | no       | Absolute expiry as a **Unix timestamp (seconds)**. Must be in the future. |
| `expires_in_days` | int    | no       | Expiry as a **duration** from now, in days.                            |

Expiry is optional. If **both** fields are sent, `expires_at` wins; if **neither**
is sent (or both are `0`), the token **never expires**. `expires_at` is what the
web UI's date picker sends; `expires_in_days` is the convenience form for scripts.

### Response — `201 Created`

```json
{
  "id": 7,
  "name": "laptop cmus",
  "token": "Jq3xrJ0m8b2f9Yk1...Q"
}
```

> ⚠️ `token` is the raw secret and is returned **only here**. Copy it now — the
> server keeps only a hash, so it cannot show it again. If you lose it, revoke the
> token and create a new one.

### Examples

```bash
# A token that never expires.
curl -b cookies.txt -X POST http://localhost:3000/api/auth/tokens \
     -H "Content-Type: application/json" \
     -d '{"name": "backup script"}'

# Expires in 90 days (duration form).
curl -b cookies.txt -X POST http://localhost:3000/api/auth/tokens \
     -H "Content-Type: application/json" \
     -d '{"name": "ci runner", "expires_in_days": 90}'

# Expires at an absolute date — end of 2026-12-31 UTC (1798761599).
curl -b cookies.txt -X POST http://localhost:3000/api/auth/tokens \
     -H "Content-Type: application/json" \
     -d '{"name": "laptop", "expires_at": 1798761599}'
```

---

## List your tokens — `GET /api/auth/tokens`

Returns the calling user's tokens, newest first. Hashes are never returned, and
you only ever see your **own** tokens.

### Response — `200 OK`

```json
[
  {
    "id": 7,
    "name": "laptop cmus",
    "created_at": 1718200000,
    "last_used": 1718286400,
    "expires_at": 1798761599,
    "revoked": false
  },
  {
    "id": 6,
    "name": "old script",
    "created_at": 1717000000,
    "last_used": null,
    "expires_at": null,
    "revoked": true
  }
]
```

| Field        | Type        | Meaning                                                        |
|--------------|-------------|----------------------------------------------------------------|
| `id`         | int         | Token id (used in the revoke URL).                            |
| `name`       | string      | The label given at creation.                                 |
| `created_at` | int         | Creation time, Unix seconds.                                 |
| `last_used`  | int \| null | Last time the token authenticated a request; `null` if never. |
| `expires_at` | int \| null | Expiry, Unix seconds; `null` means it never expires.         |
| `revoked`    | bool        | `true` once revoked (kept in the list for history).          |

```bash
curl -b cookies.txt http://localhost:3000/api/auth/tokens
```

---

## Revoke a token — `DELETE /api/auth/tokens/{id}`

Immediately invalidates the token; clients using it stop working on the next
request. Revocation is permanent (you cannot un-revoke — create a new token
instead). You can only revoke your own tokens.

### Response

- `204 No Content` — revoked.
- `404 Not Found` — no token with that id belongs to the calling user, **or** it is
  already revoked (revoking is idempotent only in effect, not in status code — a
  second revoke of the same token returns `404`).

```bash
curl -b cookies.txt -X DELETE http://localhost:3000/api/auth/tokens/7
```

---

## Errors

| Status | When                                                                              |
|--------|-----------------------------------------------------------------------------------|
| `400`  | Create: `name` missing, `expires_at` not in the future, or malformed JSON. Revoke: non-numeric `{id}`. |
| `401`  | The caller is not authenticated (no valid session cookie or token).              |
| `404`  | Revoke: the token id is not one of the caller's tokens, or it is already revoked. |

A request made with an **expired or revoked** token is anonymous, so a protected
endpoint returns `401 authentication required` (not `403`).

---

## End-to-end example

Bootstrapping a token needs an existing credential. Logging in with a password
gives you a session cookie, which you then use to mint a token:

```bash
BASE=http://localhost:3000

# 1. Log in (stores the madshare_session cookie in cookies.txt).
curl -c cookies.txt -X POST "$BASE/api/auth/login" \
     -H "Content-Type: application/json" \
     -d '{"username": "alice", "password": "s3cret-password"}'

# 2. Create a token using that session; capture the raw value once.
TOKEN=$(curl -s -b cookies.txt -X POST "$BASE/api/auth/tokens" \
     -H "Content-Type: application/json" \
     -d '{"name": "cli", "expires_in_days": 365}' | jq -r .token)

# 3. From now on, authenticate with the token alone — no cookie needed.
curl -H "Authorization: Bearer $TOKEN" "$BASE/api/auth/me"

# 4. When you're done with it, revoke it (look up its id via the list endpoint).
ID=$(curl -s -b cookies.txt "$BASE/api/auth/tokens" \
     | jq '.[] | select(.name=="cli") | .id')
curl -b cookies.txt -X DELETE "$BASE/api/auth/tokens/$ID"
```

A token inherits its owner's permissions, so step 3 can reach any endpoint the
user is allowed to use — e.g. uploading a file:

```bash
curl -H "Authorization: Bearer $TOKEN" \
     -F "file=@./track.flac" "$BASE/files/upload"
```

---

## Security notes

- **Shown once.** Only the SHA-256 hash is stored; treat the raw token like a
  password and store it in a secret manager, not in source control.
- **Scope.** A token can do anything its owner can. It is not a reduced-scope
  credential — revoke it if it leaks.
- **Transport.** Send tokens only over a trusted/encrypted channel. On a plain-HTTP
  origin the browser Clipboard API is unavailable, so the Settings page falls back
  to selecting the value for a manual copy.
- **Expiry.** Prefer setting an expiry for automated clients so a forgotten token
  doesn't live forever; rotate by creating a new token and revoking the old one.
