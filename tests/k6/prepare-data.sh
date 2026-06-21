#!/bin/sh
# prepare-data.sh — generate tests/k6/config/audio-manifest.json from the audio
# files in TEST_AUDIO_DIR. k6 cannot list a directory at runtime, so the upload
# (and delete) cases read exactly the files named in that manifest. The manifest
# is gitignored (it carries your local audio names); only the committed
# audio-manifest.json.example ships. Run this after dropping audio into
# TEST_AUDIO_DIR, or whenever you swap fixtures.
#
# Usage:
#   ./prepare-data.sh [AUDIO_DIR]
#
# AUDIO_DIR precedence: $1 arg > $TEST_AUDIO_DIR env > <repo>/test_data.
# Prefer an absolute path: manifest entries are stored relative to AUDIO_DIR, so
# this MUST be the same directory k6 later reads as TEST_AUDIO_DIR. (The env's
# committed default is resolved by k6 relative to its script files, not the
# shell — so if you only sourced .env, pass the dir explicitly here.)
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
out="$script_dir/config/audio-manifest.json"

audio_dir=${1:-${TEST_AUDIO_DIR:-"$script_dir/../../test_data"}}

if ! cd -- "$audio_dir" 2>/dev/null; then
  echo "prepare-data.sh: audio dir not found: $audio_dir" >&2
  echo "  pass one explicitly: ./prepare-data.sh /path/to/audio" >&2
  exit 1
fi
audio_dir=$(pwd) # normalize to absolute for the summary

tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT

# Accepted extensions mirror the MIME map in lib/data.js — anything else would be
# uploaded as application/octet-stream and rejected by the server's ext gate.
find . -type f \( \
  -iname '*.mp3' -o -iname '*.flac' -o -iname '*.m4a' -o -iname '*.mp4' -o \
  -iname '*.ogg' -o -iname '*.opus' -o -iname '*.aac' -o -iname '*.wav' \
  \) -print 2>/dev/null | sed 's|^\./||' | LC_ALL=C sort >"$tmp"

count=$(wc -l <"$tmp" | tr -d ' ')

# Emit a JSON array, escaping backslash and double-quote in each path. (Filenames
# with embedded newlines are not supported.)
awk '
  { gsub(/\\/, "\\\\"); gsub(/"/, "\\\""); items[NR] = $0 }
  END {
    print "["
    for (i = 1; i <= NR; i++) printf "  \"%s\"%s\n", items[i], (i < NR ? "," : "")
    print "]"
  }
' "$tmp" >"$out"

echo "prepare-data.sh: wrote $count file(s) to ${out#"$script_dir/"}"
echo "  scanned: $audio_dir"
if [ "$count" -eq 0 ]; then
  echo "  (no audio found — upload/delete cases will no-op)" >&2
fi
