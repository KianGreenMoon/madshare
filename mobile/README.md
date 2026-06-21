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

## Build the APK

There is **no native aarch64 Android SDK** — Google ships `aapt2` (the only
x86_64-only tool in the build) for x86_64 only. So the build must run on an
x86_64 toolchain. `build/build-apk.sh` runs the Capacitor scaffolding natively
(aarch64 Node) and the Gradle build in the `build/Dockerfile` amd64 image:

```bash
cd mobile
./build/build-apk.sh
# -> android/app/build/outputs/apk/debug/app-debug.apk
```

This works on an **x86_64 host** (or CI runner) with podman/Docker. On a plain
x86_64 box you can also skip the container entirely:

```bash
npm install && npx cap add android && cd android && ./gradlew assembleDebug
```

The `android/` dir is gitignored and fully regenerable, so building on a
different machine just needs this `mobile/` tree.

### Why it does NOT build on a 16 KB-page aarch64 host (Asahi)

On Apple-Silicon Linux with a **16 KB page kernel** (`uname -r` ends `+16k`),
x86 emulation can't run the toolchain locally — confirmed the hard way:

- **qemu-user** (what container emulation uses): trivial binaries run, but
  mapping a real x86 shared library (`libstdc++.so.6`) fails — its 4 KB-aligned
  ELF segments can't map onto 16 KB host pages (`failed to map segment`). So
  `apt`/`sdkmanager`/`aapt2` all die.
- **FEX alone**: `<jemalloc>: Unsupported system page size` → segfault. FEX needs
  a 4 KB-page environment.
- **FEX + muvm** (the host's real x86 path): works for desktop apps but requires
  a rootfs + microVM boot; driving a headless SDK/Gradle build through it is a
  project of its own.

Bottom line on such a host: **build on x86_64** (another machine, a VPS, or CI).
Do *not* disable the FEX/qemu binfmt dispatcher to force qemu — qemu can't do it
on 16 KB pages anyway, and you'll just break desktop x86 until
`sudo systemctl restart systemd-binfmt`.

### Cleartext (added when the android/ project exists)

The server is user-chosen and unknown at build time, so the app must permit
plaintext HTTP broadly (Yggdrasil / LAN). After `cap add android`, allow
cleartext via an Android **network-security-config** (preferred over a blanket
`usesCleartextTraffic`). This is the coarse OS permission; the real protection is
the in-app gate in `classify.js` (design doc §4.4).

## Status

- **P1 — launcher + safety gate + health probe + hand-off:** built (this dir).
- **P2 — background audio:** wired (on-device verification pending). The Android
  System WebView's `navigator.mediaSession` is a no-op, so `player-controller.js`
  routes through `@jofr/capacitor-media-session` (GPL-3.0) via the
  `createMediaSession()` adapter when running in the shell. `build-apk.sh` patches
  the app manifest with `FOREGROUND_SERVICE_MEDIA_PLAYBACK` + `POST_NOTIFICATIONS`
  (the plugin omits both). To verify on a device: background playback survives,
  the notification's play/pause/next/prev/scrubber work, and the notification
  shows once notification permission is granted. See design-doc §6.
- **P3 — multi-server polish + per-server trusted override** (storage already
  supports it) + native "Servers" return control (login-screen-only, gated on the
  session cookie via Android `CookieManager`).
- **P4 — network-security-config hardening, icons, signed `.aab`.**
