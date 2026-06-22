#!/usr/bin/env bash
# Build the Madshare Android debug APK.
#
# The build METHOD is auto-detected (override with BUILD_METHOD=native|container):
#   • native    — on an x86_64 host that already has a JDK + Android SDK, Gradle
#                 runs directly. Fastest, no container.
#   • container — otherwise, the Gradle build runs inside a self-contained amd64
#                 image (build/Dockerfile) via podman. This is the path for an
#                 x86_64 host WITHOUT a local SDK, and needs no Android toolchain
#                 on the host.
#
# The Capacitor scaffolding (cap add/sync) always runs natively on the host's Node.
# A 16 KB-page aarch64 host (Asahi) cannot build at all — see the guard below and
# ../README.md ("Why it does NOT build on a 16 KB-page aarch64 host").
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
mobile=$(cd "$here/.." && pwd)
img=madshare-android-build
arch=$(uname -m)

cd "$mobile"

echo "==> native scaffold (host Node)"
[ -d node_modules ] || npm install
[ -d android ] || npx cap add android
npx cap sync android

# The @jofr/capacitor-media-session foreground service is type "mediaPlayback":
# Android 14 (targetSdk 34) requires FOREGROUND_SERVICE_MEDIA_PLAYBACK to start
# it, and Android 13+ needs POST_NOTIFICATIONS to show the media notification.
# The plugin declares neither, so add them to the app manifest (idempotent).
manifest=android/app/src/main/AndroidManifest.xml
add_perm() {
  grep -q "$1" "$manifest" 2>/dev/null && return 0
  sed -i "s#</manifest>#    <uses-permission android:name=\"android.permission.$1\" />\n</manifest>#" "$manifest"
  echo "   + $1"
}
if [ -f "$manifest" ]; then
  echo "==> ensure foreground-service / notification permissions"
  add_perm FOREGROUND_SERVICE_MEDIA_PLAYBACK
  add_perm POST_NOTIFICATIONS
fi

# ── Pick build method ─────────────────────────────────────────────────────────
# Native needs an x86_64 host (aapt2 is x86_64-only), a JDK, and a discoverable
# Android SDK: ANDROID_SDK_ROOT / ANDROID_HOME, or an sdk.dir that `cap add` wrote
# into android/local.properties when it detected one.
sdk_dir="${ANDROID_SDK_ROOT:-${ANDROID_HOME:-}}"
method=container
if [ "$arch" = "x86_64" ] && command -v java >/dev/null 2>&1; then
  if { [ -n "$sdk_dir" ] && { [ -d "$sdk_dir/platform-tools" ] || [ -d "$sdk_dir/cmdline-tools" ]; }; } \
     || grep -qs '^sdk\.dir=' android/local.properties; then
    method=native
  fi
fi
method="${BUILD_METHOD:-$method}"   # allow an explicit override

if [ "$method" = "native" ]; then
  echo "==> native Gradle build on this x86_64 host (no container)"
  (
    cd android
    if [ -n "$sdk_dir" ]; then export ANDROID_SDK_ROOT="$sdk_dir" ANDROID_HOME="$sdk_dir"; fi
    ./gradlew --no-daemon assembleDebug
  )
else
  rm -f android/local.properties   # let the container's ANDROID_SDK_ROOT win

  echo "==> container build (no local SDK, or non-amd64 host)"
  if [ "$arch" = "aarch64" ] && [ "$(getconf PAGE_SIZE)" = "16384" ]; then
    cat >&2 <<'EOF'

This is a 16 KB-page aarch64 host (e.g. Asahi). x86 emulation cannot run the
Android toolchain here: qemu can't map x86 libstdc++ onto 16 KB pages, and FEX
needs muvm's 4 KB microVM. Build on an x86_64 machine or CI instead — copy this
mobile/ tree there and run this script.

See README.md, "Why it does NOT build on a 16 KB-page aarch64 host".
EOF
    exit 2
  fi

  echo "==> check amd64 container exec"
  if ! podman run --rm --platform linux/amd64 docker.io/library/alpine:3.20 true >/dev/null 2>&1; then
    echo "amd64 containers can't exec here — no working x86 emulation for podman." >&2
    echo "Install qemu-user-static (binfmt), or build on a native x86_64 host." >&2
    exit 1
  fi

  echo "==> build toolchain image (cached after first run)"
  podman build --platform linux/amd64 -t "$img" "$here"

  echo "==> assemble debug APK in the amd64 container"
  podman run --rm --platform linux/amd64 \
    -v "$mobile":/app:z \
    -v madshare-gradle-cache:/root/.gradle \
    -w /app/android \
    "$img" ./gradlew --no-daemon assembleDebug
fi

apk="$mobile/android/app/build/outputs/apk/debug/app-debug.apk"
echo
if [ -f "$apk" ]; then
  echo "==> done ($method build): $apk"
  ls -la "$apk"
else
  echo "==> build finished but APK not found — check the Gradle output above." >&2
  exit 1
fi
