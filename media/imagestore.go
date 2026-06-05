package media

import (
	"crypto/sha256"
	"encoding/hex"
)

// baseKeyLen is the number of hex characters of the SHA-256 used as a base_key.
// 16 hex chars = 64 bits, matching the legacy image key (api hashHex[:16]).
const baseKeyLen = 16

// BaseKey computes the 16-character hex prefix of the SHA-256 of data. The same
// image bytes always produce the same base_key.
//
// Collision posture: 16 hex chars = 64 bits. Birthday collision is ~50% only
// near 2^32 distinct covers — acceptable at this scale. Two *different* images
// sharing a prefix would silently overwrite each other's variant files and be
// treated as one job by EnqueueImageJob's base_key-keyed idempotency. This is
// an accepted trade-off, consistent with the legacy image key, not a bug.
func BaseKey(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:baseKeyLen]
}

// VariantPath returns the on-disk relative path (and /images/ URL suffix) for a
// variant: "<baseKey>/<variant><ext>" — e.g. "a3f1c8d2e4b7f901/small_crop.jpg".
// The separator is always "/"; on disk, callers join it under imagesDir.
func VariantPath(baseKey, variant, ext string) string {
	return baseKey + "/" + variant + ext
}

// VariantURL returns the public /images/ URL for a variant:
// "/images/<baseKey>/<variant><ext>".
func VariantURL(baseKey, variant, ext string) string {
	return "/images/" + VariantPath(baseKey, variant, ext)
}
