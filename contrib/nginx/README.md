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

> **The Yggdrasil vhost is now the alternative, not the only route.** It assumes
> a *system* yggdrasil daemon (root + a TUN device), with nginx in front because
> port 80 is a privileged kernel socket. Madshare can instead serve its own
> embedded mesh address directly — `[[listen_mesh]]` in `madshare.toml.example`,
> no daemon, no root, no proxy, and per-client login throttling that works
> (see the note below). Use this file when you already run Yggdrasil system-wide
> and want the host's address rather than the app's own.

## Notes

- **Upload size:** `client_max_body_size` must be at least `storage.max_upload_mb`
  (default 500 MiB) or large uploads get a `413` from nginx before reaching the app.
- **Streaming:** `proxy_buffering off` plus forwarding the `Range` header lets
  clients seek within audio without nginx buffering the whole response.
- **Secure cookies:** Madshare marks the session cookie `Secure` only when it
  sees a TLS connection. Behind a TLS-terminating proxy the backend connection
  is plain HTTP, so the cookie won't currently be flagged `Secure` unless the
  app is taught to trust `X-Forwarded-Proto` (the examples already send it).
- **Login rate-limiting:** the app has a built-in per-IP login throttle that
  keys on the TCP peer. Behind a proxy that peer is *always nginx's loopback
  address*, so the app deliberately **exempts loopback** — otherwise one shared
  bucket would throttle every proxied user at once (one user's failed logins
  would lock out the rest). Per-client login limiting is delegated to nginx
  here, so the examples add a `limit_req` zone on `/api/auth/login` keyed on the
  real client IP (`$binary_remote_addr`) — the only layer that sees it when
  proxied. The app's global cap on *concurrent* (expensive) password
  verifications is not per-IP and keeps working regardless. On a **direct bind**
  (no nginx — the app listening straight on its public/Yggdrasil address) the
  app's own per-IP throttle sees the real client and applies normally.
