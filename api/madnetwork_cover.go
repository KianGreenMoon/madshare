package api

// Covers over the madnetwork, M3 (docs/plans/covers-federation.md): a
// download-to-library brings its album's cover along.
//
// This is the embedded-art rule (maybeSaveEmbeddedCover) applied to a second
// source of art: where an upload carries a cover inside the file, a download
// carries a cover CLAIM inside the catalog entry — the origin album's
// cover_hash. The claim is redeemed the way every madnetwork claim is: fetch
// the bytes by hash (EnsureBlob verifies them against that hash, and the hash
// IS media.ImageHash's keying, one sha256 for both worlds), then let the local
// pipeline derive variants as if the cover had been uploaded here.
//
// It runs at download/attach time, not at approval — deliberately symmetric
// with uploads, whose embedded art claims the album while the track is still a
// draft. And it is fill-if-missing only: SetAlbumCoverIfAbsent never replaces
// art this library already chose.

import (
	"bytes"
	"context"
	"image"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	// The cover sanity decode below reads only the header, but needs the same
	// two formats registered that the variant pipeline accepts.
	_ "image/jpeg"
	_ "image/png"

	"daemonlord.ygg/madshare/federation"
	"daemonlord.ygg/madshare/media"
)

// coverExtForFormat maps image.DecodeConfig's format name to the extension the
// cover store files originals under. Sniffed from the bytes rather than trusted
// from the claim: a mislabelled original would otherwise claim the album's one
// cover row with a file the variant worker can never process, wedging
// fill-if-missing for that album forever.
func coverExtForFormat(format string) string {
	switch format {
	case "jpeg":
		return ".jpg"
	case "png":
		return ".png"
	}
	return ""
}

// maybeFetchRemoteCover redeems a downloaded entry's cover claim: fetch the
// original by hash from whoever holds it, land it in the image store, claim the
// album cover if the album still has none, and queue variant generation.
//
// Designed to run in the background (`go h.maybeFetchRemoteCover(entry)`) and
// to fail soft at every step — a missing or broken cover must never cost the
// download it rode in on, so nothing here reports an error to anybody; it logs
// and returns. Racing callers are settled where the upload path settles them:
// SetAlbumCoverIfAbsent's atomic claim.
func (h *handler) maybeFetchRemoteCover(entry *federation.CatalogEntry) {
	// Same reason as runMadnetworkDownload: this runs on its own goroutine,
	// where a panic would take the process down rather than one request.
	defer func() {
		if r := recover(); r != nil {
			log.Printf("madnetwork cover: panic: %v", r)
		}
	}()
	if h.federation == nil || entry == nil || entry.CoverHash == "" || entry.Album == "" {
		return
	}
	if !isSHA256Hex(entry.CoverHash) {
		return // remote input — only a real content hash may reach the fetch path
	}
	// The claimed ext pre-filters formats the pipeline cannot process (WebP et
	// al) before any bytes move; the stored ext still comes from the sniff.
	if _, ok := allowedImageExtensions[strings.ToLower(entry.CoverExt)]; !ok {
		return
	}
	artist := entry.AlbumArtist
	if artist == "" {
		artist = entry.Artist
	}
	if artist == "" {
		return // no grouping identity — same bar the embedded path sets
	}
	ctx := context.Background()
	albumID, err := h.repo.ResolveAlbumID(ctx, artist, entry.Album)
	if err != nil {
		log.Printf("madnetwork cover: resolve album: %v", err)
		return
	}
	// Cheap pre-check before any network I/O; the atomic claim below is what
	// actually holds under races.
	if has, err := h.repo.HasAlbumCover(ctx, albumID); err != nil || has {
		return
	}
	t, err := h.ensureBlob(ctx, entry.CoverHash)
	if err != nil {
		log.Printf("madnetwork cover %s: fetch: %v", entry.CoverHash[:8], err)
		return
	}
	<-t.Done()
	if err := t.Err(); err != nil {
		log.Printf("madnetwork cover %s: transfer: %v", entry.CoverHash[:8], err)
		return
	}
	// The same ceiling every other cover ingress enforces (maxImageSize).
	if t.Size() > maxImageSize {
		log.Printf("madnetwork cover %s: %d bytes exceeds %d cap; skipping", entry.CoverHash[:8], t.Size(), maxImageSize)
		return
	}
	f, err := t.Open()
	if err != nil {
		log.Printf("madnetwork cover %s: open: %v", entry.CoverHash[:8], err)
		return
	}
	data := make([]byte, t.Size())
	_, err = io.ReadFull(f, data)
	f.Close()
	if err != nil {
		log.Printf("madnetwork cover %s: read: %v", entry.CoverHash[:8], err)
		return
	}
	// EnsureBlob verified data against CoverHash; what is still only claimed is
	// that the bytes are a decodable image. Sniff the header before claiming
	// the row (see coverExtForFormat for what a wrong claim would cost).
	_, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		log.Printf("madnetwork cover %s: not a decodable image: %v", entry.CoverHash[:8], err)
		return
	}
	ext := coverExtForFormat(format)
	if ext == "" {
		return
	}
	objectKey := media.VariantPath(entry.CoverHash, media.VariantOriginal, ext)
	destPath := filepath.Join(h.sourceImagesDir, objectKey)
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		log.Printf("madnetwork cover: mkdir %s: %v", filepath.Dir(destPath), err)
		return
	}
	if err := os.WriteFile(destPath, data, 0o644); err != nil {
		log.Printf("madnetwork cover: write %s: %v", destPath, err)
		return
	}
	now := time.Now().Unix()
	inserted, err := h.repo.SetAlbumCoverIfAbsent(ctx, albumID, entry.CoverHash, ext,
		objectKey, allowedImageExtensions[ext], now)
	if err != nil {
		log.Printf("madnetwork cover: claim album cover: %v", err)
		return
	}
	if !inserted {
		return // somebody else's art won the album while we fetched — theirs stands
	}
	if err := h.repo.EnqueueImageJob(ctx, "album", artist+"\x1f"+entry.Album, entry.CoverHash, now); err != nil {
		log.Printf("madnetwork cover: enqueue job: %v", err)
		return
	}
	if h.imagePool != nil {
		h.imagePool.Notify()
	}
	// The original is library-owned now; the mesh-cache copy is the same
	// duplicate an audio download leaves, dropped for the same reason.
	h.evictCached(entry.CoverHash)
	log.Printf("madnetwork cover %s: attached to album %d (%s — %s)", entry.CoverHash[:8], albumID, artist, entry.Album)
}
