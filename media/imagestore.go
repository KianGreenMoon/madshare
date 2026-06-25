package media

import (
	"crypto/sha256"
	"encoding/hex"
)

// ImageHash computes the full 64-character hex SHA-256 of an image's bytes. It
// keys an owned cover: identical covers (uploaded or embedded) hash the same, so
// they share one stored source original and one derived variant set across every
// album/artist that uses them. The original source is stored at
// <files_dir>/images/<ImageHash>/original<ext> (a regenerate seed, never served);
// its derived variants at <variants_dir>/images/<ImageHash>/<recipe><ext> (served
// at /images). See docs/architecture/variants.md.
//
// This supersedes the historical 16-char base_key: the full digest is the
// content-addressed source→derivative link, mirroring the audio model.
func ImageHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// VariantPath returns the on-disk relative path (and /images/ URL suffix) for a
// variant: "<hash>/<variant><ext>" — e.g.
// "e3b0c44298fc1c14…/large_crop.jpg". The separator is always "/"; on disk,
// callers join it under the images dir.
func VariantPath(hash, variant, ext string) string {
	return hash + "/" + variant + ext
}

// VariantURL returns the public /images/ URL for a derived variant:
// "/images/<hash>/<variant><ext>". Only derived variants are served there; the
// source original (VariantOriginal) lives under <files_dir>/images and is never
// exposed — pass a DerivedVariants name, not VariantOriginal.
func VariantURL(hash, variant, ext string) string {
	return "/images/" + VariantPath(hash, variant, ext)
}
