#!/usr/bin/env bash
# Build the Madshare Android debug APK on aarch64 (Asahi) via an x86_64 container.
#
# The Capacitor scaffolding (cap add/sync) runs natively on the host's aarch64
# Node — fast, and keeps node_modules' arch correct. Only the Gradle build, whose
# aapt2 is x86_64-only, runs inside the amd64 container under qemu.
#
# See ../README.md and docs/architecture/android-app.md.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
mobile=$(cd "$here/.." && pwd)
img=madshare-android-build

cd "$mobile"

echo "==> native scaffold (host aarch64 node)"
[ -d node_modules ] || npm install
[ -d android ] || npx cap add android
npx cap sync android
rm -f android/local.properties   # let the container's ANDROID_SDK_ROOT win

echo "==> host capability check"
if [ "$(uname -m)" = "aarch64" ] && [ "$(getconf PAGE_SIZE)" = "16384" ]; then
  cat >&2 <<'EOF'

This is a 16 KB-page aarch64 host (e.g. Asahi). x86 emulation cannot run the
Android toolchain here: qemu can't map x86 libstdc++ onto 16 KB pages, and FEX
needs muvm's 4 KB microVM. Build on an x86_64 machine or CI instead — copy this
mobile/ tree there and run this script, or natively:

  npm install && npx cap add android && cd android && ./gradlew assembleDebug

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

echo "==> assemble debug APK (emulated — the first build is slow)"
podman run --rm --platform linux/amd64 \
  -v "$mobile":/app:z \
  -v madshare-gradle-cache:/root/.gradle \
  -w /app/android \
  "$img" ./gradlew --no-daemon assembleDebug

apk="$mobile/android/app/build/outputs/apk/debug/app-debug.apk"
echo
if [ -f "$apk" ]; then
  echo "==> done: $apk"
  ls -la "$apk"
else
  echo "==> build finished but APK not found — check the Gradle output above." >&2
  exit 1
fi
