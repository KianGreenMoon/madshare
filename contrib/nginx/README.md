# nginx reverse-proxy examples

Example nginx vhosts for putting Madshare behind a reverse proxy. These are
**examples** — copy one, edit the marked values (server name, addresses,
certificate paths, upstream port), and drop it into your nginx config
(`/etc/nginx/sites-available/`, `/etc/nginx/conf.d/`, …).

Madshare itself serves plain HTTP; it is expected to listen on a loopback
socket and let nginx handle TLS / the public-facing interface. See the
`[[listen]]` section in `madshare.toml.example`.

| File | Use case |
|------|----------|
| `madshare-ssl.conf` | Public deployment. Terminates HTTPS, redirects HTTP→HTTPS, ACME-ready. |
| `madshare-yggdrasil.conf` | Plain HTTP bound to a [Yggdrasil](https://yggdrasil-network.github.io/) address. No TLS — Yggdrasil encrypts the link layer. |

## Notes

- **Upload size:** `client_max_body_size` must be at least `storage.max_upload_mb`
  (default 500 MiB) or large uploads get a `413` from nginx before reaching the app.
- **Streaming:** `proxy_buffering off` plus forwarding the `Range` header lets
  clients seek within audio without nginx buffering the whole response.
- **Secure cookies:** Madshare marks the session cookie `Secure` only when it
  sees a TLS connection. Behind a TLS-terminating proxy the backend connection
  is plain HTTP, so the cookie won't currently be flagged `Secure` unless the
  app is taught to trust `X-Forwarded-Proto` (the examples already send it).
- **Login rate-limiting:** the app has a built-in per-IP login throttle, but it
  keys on the TCP peer — behind a proxy that peer is *always nginx's loopback
  address*, so every proxied user shares one bucket. The examples therefore add
  an nginx `limit_req` zone on `/api/auth/login`, keyed on the real client IP
  (`$binary_remote_addr`), which is the only layer that sees it when proxied.
  The app's global cap on *concurrent* (expensive) password verifications is
  not per-IP and keeps working regardless. On a **direct bind** (no nginx — the
  app listening straight on its public/Yggdrasil address) the app's own per-IP
  throttle already sees the real client, so the nginx zone is just defense in
  depth there.
