# Madshare Android app (Capacitor remote-URL shell)

A thin native shell that wraps the existing Madshare **web UI** in a WebView
pointed at a running server. Design & rationale:
[`docs/architecture/android-app.md`](../docs/architecture/android-app.md).

The app does **not** ship a second front-end. Its only bundled web content is the
**launcher** in `www/` — a server picker plus the **connection-safety gate**
(§4 of the design doc). Once you pick a server and the gate passes, the WebView
navigates to that origin and runs the unchanged web UI same-origin (so the
session cookie and gated audio streaming "just work").

## Layout

```
mobile/
├── www/                  bundled launcher (Capacitor webDir)
│   ├── index.html        server list + add-server + §4 warning dialog
│   ├── launcher.css      self-contained, borrows the web UI's dark palette
│   └── js/
│       ├── classify.js   PURE connection-safety classifier (security core)
│       └── launcher.js   storage + health probe + gate + hand-off glue
├── tests/
│   └── classify.test.mjs unit tests for classify.js (node --test)
├── capacitor.config.json appId/appName, allowNavigation:["*"], cleartext
└── package.json          Capacitor 6 deps; `npm test` runs the classifier tests
```

`classify.js` is the security-critical part and is kept DOM-/network-free so it
is unit-tested without a browser (mirrors `tests/js/queue-ops.js` in the server
repo).

## Develop / test (no Android SDK needed)

```bash
cd mobile
npm install            # Capacitor CLI + core (network required)
npm test               # node --test tests/classify.test.mjs
```

You can also open `www/index.html` in a desktop browser to exercise the launcher
UI. (The health probe needs the native HTTP layer to bypass CORS, so in a plain
browser it degrades to "unreachable" unless the server enables CORS — the gate
and hand-off still work.)

## Build the APK (aarch64 host, e.g. Asahi)

There is **no native aarch64 Android SDK** — Google ships `aapt2` (the only
x86_64-only tool in the build) for x86_64 only. The supported path here is to run
the Gradle build inside an **x86_64 container** while the Capacitor scaffolding
runs natively on the host's aarch64 Node. `build/build-apk.sh` does both:

```bash
cd mobile
./build/build-apk.sh
```

It runs `cap add`/`cap sync` natively, then `gradlew assembleDebug` in the
`build/Dockerfile` amd64 image (under podman + qemu). Output:
`android/app/build/outputs/apk/debug/app-debug.apk`.

### One-time: let containers run x86_64 on this host

This machine emulates x86 with **FEX + muvm** behind a binfmt *dispatcher*, which
shadows the static `qemu-x86_64` handler that container builds need. If the
script reports `amd64 containers can't exec`, enable qemu for containers
(reversible) and re-run:

```bash
echo 0 | sudo tee /proc/sys/fs/binfmt_misc/binfmt-dispatcher-x86_64
echo 0 | sudo tee /proc/sys/fs/binfmt_misc/FEX-x86_64
# build, then restore your normal x86 emulation:
sudo systemctl restart systemd-binfmt
```

The first container build is slow (emulated `apt` + `sdkmanager`); the image and
the `madshare-gradle-cache` volume are reused afterwards.

> On an x86_64 machine (or CI) none of this applies — just
> `npm install && npx cap add android && cd android && ./gradlew assembleDebug`
> with a stock SDK. The `android/` dir is gitignored and fully regenerable.

### Cleartext (added when the android/ project exists)

The server is user-chosen and unknown at build time, so the app must permit
plaintext HTTP broadly (Yggdrasil / LAN). After `cap add android`, allow
cleartext via an Android **network-security-config** (preferred over a blanket
`usesCleartextTraffic`). This is the coarse OS permission; the real protection is
the in-app gate in `classify.js` (design doc §4.4).

## Status

- **P1 — launcher + safety gate + health probe + hand-off:** built (this dir).
- **P2 — background audio:** target plugin `@jofr/capacitor-media-session`
  (GPL-3.0). Note: the Android System WebView does **not** implement the Web
  Media Session API, so the server's `navigator.mediaSession` calls are a no-op
  there and a native plugin is required (correction to design-doc §6).
- **P3 — multi-server polish + per-server trusted override** (storage already
  supports it) + native "Servers" return control (login-screen-only, gated on the
  session cookie via Android `CookieManager`).
- **P4 — network-security-config hardening, icons, signed `.aab`.**
