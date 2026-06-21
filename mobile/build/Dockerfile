# x86_64 Android build toolchain for cross-building the Capacitor APK on aarch64
# (Asahi / Apple-Silicon Linux). Background: the *only* x86_64-only binary in an
# Android build is aapt2 — but it is dynamically linked, and this host's x86
# emulation (FEX + muvm behind a binfmt dispatcher) does not run it headless or
# in containers. Running the whole Gradle build inside this amd64 image instead
# sidesteps every cross-arch issue: the static, F-flagged qemu-x86_64 handler
# executes it with a consistent x86_64 rootfs and no -L hacks.
#
# Built and run under podman with --platform linux/amd64. See build-apk.sh.
FROM --platform=linux/amd64 docker.io/library/eclipse-temurin:17-jdk-jammy

ENV ANDROID_SDK_ROOT=/opt/android-sdk \
    ANDROID_HOME=/opt/android-sdk \
    DEBIAN_FRONTEND=noninteractive
ENV PATH="$PATH:/opt/android-sdk/cmdline-tools/latest/bin:/opt/android-sdk/platform-tools"

# Capacitor 6 targets compileSdk 34 / build-tools 34.0.0 / JDK 17.
ARG CMDLINE_TOOLS=commandlinetools-linux-11076708_latest.zip
ARG ANDROID_PLATFORM=android-34
ARG BUILD_TOOLS=34.0.0

RUN apt-get update \
 && apt-get install -y --no-install-recommends curl unzip ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir -p "$ANDROID_SDK_ROOT/cmdline-tools" \
 && curl -fsSL "https://dl.google.com/android/repository/${CMDLINE_TOOLS}" -o /tmp/cmdtools.zip \
 && unzip -q /tmp/cmdtools.zip -d /tmp/cmdtools \
 && mv /tmp/cmdtools/cmdline-tools "$ANDROID_SDK_ROOT/cmdline-tools/latest" \
 && rm -rf /tmp/cmdtools.zip /tmp/cmdtools \
 && yes | sdkmanager --licenses >/dev/null \
 && sdkmanager --install "platform-tools" "platforms;${ANDROID_PLATFORM}" "build-tools;${BUILD_TOOLS}" >/dev/null

WORKDIR /app
