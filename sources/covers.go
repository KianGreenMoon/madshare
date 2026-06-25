package sources

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"daemonlord.ygg/madshare/media"
)

// maxCoverSize caps a derived cover's source bytes, matching the upload path's
// maxImageSize. A linked file could carry a huge embedded picture or sit beside a
// large sidecar; without the cap we would write it verbatim to the owned tree.
const maxCoverSize = 10 << 20 // 10 MB

// sidecarCoverNames are the cover-image filenames looked for next to an audio
// file, in priority order. Lowercase only — the common ripped-library convention;
// a differently-cased file falls back to embedded art.
var sidecarCoverNames = []string{
	"cover.jpg", "cover.jpeg", "cover.png",
	"folder.jpg", "folder.jpeg", "folder.png",
	"front.jpg", "front.jpeg", "front.png",
}

// maybeSaveCover fills a newly linked file's album cover when the album has none
// yet, decoding the art once into an owned source original (no links/images tree
// — covers are always owned, read-once-derive). It prefers a sidecar image in the
// file's directory over the embedded picture. A no-op when cover extraction is
// disabled (WithCovers not called) or the file lacks album context. All failures
// are logged and swallowed — a cover problem must never fail the import.
func (m *Manager) maybeSaveCover(ctx context.Context, audioPath string, tags *media.Tags, now int64) {
	if m.sourceImagesDir == "" {
		return // covers disabled (P3 / tests)
	}
	artist := effectiveAlbumArtist(tags)
	if tags.Album == "" || artist == "" {
		return // need album+artist to attach a cover
	}

	// Resolve the album (InsertFile already created it for this track, so this is
	// effectively a lookup) and skip the decode entirely if it already has a cover.
	albumID, err := m.store.ResolveAlbumID(ctx, artist, tags.Album)
	if err != nil {
		logf("sources: cover resolve album %q/%q: %v", artist, tags.Album, err)
		return
	}
	if has, err := m.store.HasAlbumCover(ctx, albumID); err != nil || has {
		return
	}

	data, mime, ok := m.findCover(audioPath, tags)
	if !ok {
		return
	}
	ext, ok := mimeToExt(mime)
	if !ok {
		return // unsupported format (e.g. webp/gif) — skip, don't enqueue
	}
	if len(data) > maxCoverSize {
		logf("sources: cover for %q is %d bytes (> %d cap); skipping", audioPath, len(data), maxCoverSize)
		return
	}

	// Store the decoded bytes once as an owned source original under
	// <files_dir>/images/<image_hash>/original<ext> (a regenerate seed, never
	// served); the variant worker derives the served variants from it.
	imageHash := media.ImageHash(data)
	objectKey := media.VariantPath(imageHash, media.VariantOriginal, ext)
	destPath := filepath.Join(m.sourceImagesDir, objectKey)
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		logf("sources: cover mkdir %s: %v", filepath.Dir(destPath), err)
		return
	}
	if err := os.WriteFile(destPath, data, 0o644); err != nil {
		logf("sources: cover write %s: %v", destPath, err)
		return
	}

	inserted, err := m.store.SetAlbumCoverIfAbsent(ctx, albumID, imageHash, ext, objectKey, mime, now)
	if err != nil {
		logf("sources: cover claim album %d: %v", albumID, err)
		return
	}
	if !inserted {
		// Another track of this album claimed the cover first; our write is a
		// harmless orphan (reclaimed by ReconcileImageOrphans). Nothing to enqueue.
		return
	}
	subjectKey := artist + "\x1f" + tags.Album
	if err := m.store.EnqueueImageJob(ctx, "album", subjectKey, imageHash, now); err != nil {
		logf("sources: cover enqueue job %s: %v", imageHash, err)
		return
	}
	if m.imagePool != nil {
		m.imagePool.Notify()
	}
}

// findCover returns the cover bytes + MIME for an audio file: a sidecar image in
// its directory (preferred), else the embedded picture from the tags. ok is false
// when neither is present.
func (m *Manager) findCover(audioPath string, tags *media.Tags) (data []byte, mime string, ok bool) {
	dir := filepath.Dir(audioPath)
	for _, name := range sidecarCoverNames {
		p := filepath.Join(dir, name)
		info, err := os.Stat(p)
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxCoverSize {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			logf("sources: read sidecar %s: %v", p, err)
			continue
		}
		return b, sidecarMIME(name), true
	}
	if tags.CoverImage != nil {
		return tags.CoverImage.Data, tags.CoverImage.MIMEType, true
	}
	return nil, "", false
}

// sidecarMIME maps a sidecar filename's extension to its image MIME type.
func sidecarMIME(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png":
		return "image/png"
	default:
		return "image/jpeg"
	}
}

// effectiveAlbumArtist returns the artist used to group an album cover: the album
// artist when set, otherwise the track artist (mirrors the library grouping rule
// and the upload path's helper of the same name).
func effectiveAlbumArtist(t *media.Tags) string {
	if t.AlbumArtist != "" {
		return t.AlbumArtist
	}
	return t.Artist
}

// mimeToExt maps a canonical image MIME type to its file extension. Only the two
// formats the variant pipeline can process are accepted; everything else returns
// ok=false so callers skip it rather than queue a job the worker would only fail.
func mimeToExt(mimeType string) (string, bool) {
	switch mimeType {
	case "image/jpeg":
		return ".jpg", true
	case "image/png":
		return ".png", true
	}
	return "", false
}
