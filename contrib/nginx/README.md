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
