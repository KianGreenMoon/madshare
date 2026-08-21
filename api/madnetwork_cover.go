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
	"errors"
	"image"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	// The cover sanity decode below reads only the header, but needs the same
	// two formats registered that the variant pipeline accepts.
	_ "image/jpeg"
	_ "image/png"

	"github.com/go-chi/chi/v5"

	"daemonlord.ygg/madshare/database"
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

// ── The cover election (covers-federation M4) ────────────────────────────────

// coverBallot tallies cover claims for ONE album (or one merged track's album
// context) and elects a winner by the voices rule — the same election that
// orders tagsets and versions, applied to art. Claims are collected per
// (hash, ext) candidate with the node keys behind them; the count itself is
// BranchMap.Voices, the one place the sybil rule is written.
type coverBallot struct {
	order      []string // candidate keys, first-seen order (the determinism floor)
	candidates map[string]*coverCandidate
}

type coverCandidate struct {
	hash, ext string
	keys      []string
	self      bool
}

func (b *coverBallot) add(hash, ext, sourceKey string, self bool) {
	if hash == "" {
		return
	}
	if b.candidates == nil {
		b.candidates = map[string]*coverCandidate{}
	}
	k := hash + ext
	c := b.candidates[k]
	if c == nil {
		c = &coverCandidate{hash: hash, ext: ext}
		b.candidates[k] = c
		b.order = append(b.order, k)
	}
	if self {
		c.self = true
	} else if sourceKey != "" {
		c.keys = append(c.keys, sourceKey)
	}
}

// winner elects the candidate with the most voices; ties fall to the larger
// raw holder count, then to a self-held cover (this library's own choice), then
// to the lexically smallest hash — every step deterministic, so two requests
// paint the same art.
func (b *coverBallot) winner(branches database.BranchMap) (hash, ext string) {
	var best *coverCandidate
	bestVoices := -1
	for _, k := range b.order {
		c := b.candidates[k]
		v := branches.Voices(c.keys, c.self)
		switch {
		case v > bestVoices:
		case v < bestVoices:
			continue
		case len(c.keys) != len(best.keys):
			if len(c.keys) < len(best.keys) {
				continue
			}
		case c.self != best.self:
			if !c.self {
				continue
			}
		case c.hash >= best.hash:
			continue
		}
		best, bestVoices = c, v
	}
	if best == nil {
		return "", ""
	}
	return best.hash, best.ext
}

// ── The cover relay (covers-federation M4) ───────────────────────────────────

// madnetworkCover handles GET /api/madnetwork/cover/{hash}: the cover twin of
// stream/{hash} — fetch the original from whoever holds it, cache-through, and
// serve it. A cover this library owns (or already cached) short-circuits in
// EnsureBlob and never touches the network. v0 serves the original bytes: every
// upload boundary in the network caps covers at maxImageSize, so the blob is
// bounded, and thin clients downscale on their side; the on-relay variant step
// is deferred with the variants/cache work (see docs/plans/covers-federation.md
// M4). This does not touch the local-library rule that /api/albums serves only
// derived variants — a network cover is a different entity with its own cap.
func (h *handler) madnetworkCover(w http.ResponseWriter, r *http.Request) {
	hash := strings.ToLower(chi.URLParam(r, "hash"))
	if !isSHA256Hex(hash) {
		http.Error(w, "invalid hash", http.StatusBadRequest)
		return
	}
	t, err := h.ensureBlob(r.Context(), hash)
	if err != nil {
		if errors.Is(err, federation.ErrNoHolder) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "transfer unavailable", http.StatusServiceUnavailable)
		return
	}
	// Covers are small; unlike audio there is nothing to gain from streaming a
	// growing file, so wait the fetch out and serve complete bytes with native
	// Range/HEAD support.
	select {
	case <-t.Done():
	case <-r.Context().Done():
		return // client went away while the mesh converged
	}
	if t.Err() != nil {
		http.Error(w, "transfer failed", http.StatusBadGateway)
		return
	}
	if t.Size() > maxImageSize {
		http.Error(w, "cover exceeds the image size cap", http.StatusBadGateway)
		return
	}
	f, err := t.Open()
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	// The bytes decide the type — a relayed blob's name is a remote claim. Only
	// the two formats every cover ingress accepts are served; anything else is
	// the remote's fault, reported as such.
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	mimeType := http.DetectContentType(head[:n])
	if mimeType != "image/jpeg" && mimeType != "image/png" {
		http.Error(w, "not a servable image", http.StatusBadGateway)
		return
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Hash-addressed: the answer can never change, so let clients keep it.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeContent(w, r, "", info.ModTime(), f)
}
