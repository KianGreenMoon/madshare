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
├── native/               TRACKED native sources (android/ is gitignored + regenerable)
│   └── java/ygg/daemonlord/madshare/
│       ├── MainActivity.java        installs the media bridge on the WebView
│       ├── MediaBridge.java         window.MadshareMedia (addJavascriptInterface)
│       └── MediaPlaybackService.java foreground service + MediaSession + notification
├── tests/
│   └── classify.test.mjs unit tests for classify.js (node --test)
├── capacitor.config.json appId/appName, allowNavigation:["*"], cleartext
└── package.json          Capacitor 6 deps; `npm test` runs the classifier tests
```

`native/` holds the native Java that `build/build-apk.sh` copies into the
regenerable `android/` tree on every build (background audio — see the design doc
§6). `allowNavigation` does **not** inject the Capacitor bridge on the remote
origin, so OS media controls / background playback are driven by the
`window.MadshareMedia` bridge here, not by a Capacitor plugin.

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
(host Node) and then **auto-detects how to build**:

```bash
cd mobile
./build/build-apk.sh
# -> android/app/build/outputs/apk/debug/app-debug.apk
```

- **native** — on an x86_64 host that has a **JDK + Android SDK** (via
  `ANDROID_SDK_ROOT`/`ANDROID_HOME`, or an `sdk.dir` that `cap add` wrote into
  `android/local.properties`), it runs Gradle directly. Fastest, no container.
- **container** — otherwise (x86_64 host without a local SDK), it builds inside
  the `build/Dockerfile` amd64 image via podman/Docker. No Android toolchain
  needed on the host.

Force one with `BUILD_METHOD=native ./build/build-apk.sh` (or `=container`). The
`android/` dir is gitignored and fully regenerable, so building on a different
machine just needs this `mobile/` tree.

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

### Cleartext (handled automatically by build-apk.sh)

The server is user-chosen and unknown at build time, and is usually plaintext
HTTP (Yggdrasil / LAN). Android blocks cleartext by default, so without this the
WebView fails to load an `http://` server even though a desktop browser opens it
fine. `build-apk.sh` adds `android:usesCleartextTraffic="true"` to the app's
`<application>` after `cap add` (idempotent). This is the coarse OS permission;
the real protection is the in-app gate in `classify.js` (design doc §4.4).

## Status

- **P1 — launcher + safety gate + health probe + hand-off:** built (this dir).
- **P2 — background audio:** **done — verified on-device 2026-06-23** (GrapheneOS,
  Android 16): background playback survives, and the notification's
  play/pause/next/prev/scrubber work. The Android System WebView exposes no
  `navigator.mediaSession`, and Capacitor's bridge is **not** injected on the remote
  server origin where the player runs — so the `createMediaSession()` adapter routes
  through `window.MadshareMedia`, a native bridge injected via `addJavascriptInterface`
  (`native/java/.../MediaBridge.java`) backed by a `MediaPlaybackService` foreground
  service + `MediaSessionCompat`. `build-apk.sh` copies the native sources in and
  patches the manifest (`FOREGROUND_SERVICE`/`…_MEDIA_PLAYBACK`/`POST_NOTIFICATIONS` +
  the `<service>`) and `build.gradle` (`androidx.media`). NB the player JS is served by
  the **server** (embedded in its binary), not the APK — shipping a player change means
  redeploying the server too. See design-doc §6.
- **P3 — multi-server polish + per-server trusted override** (storage already
  supports it) + native "Servers" return control (login-screen-only, gated on the
  session cookie via Android `CookieManager`).
- **P4 — network-security-config hardening, icons, signed `.aab`.**
