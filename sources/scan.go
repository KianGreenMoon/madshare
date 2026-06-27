package sources

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/media"
)

// logf and unixNow are indirected so tests can silence/inject them; production
// uses the stdlib log and wall clock.
var (
	logf    = log.Printf
	unixNow = func() int64 { return time.Now().Unix() }
)

// scan is the background worker launched by Add. It walks realRoot, references
// each accepted audio file by a symlink in the 'links' storage, and ingests a
// catalog row — never touching the external originals. It records a scan summary
// and flips the source's status to active (or error) when done, then releases the
// single scan slot. It must be the sole caller that clears m.running.
func (m *Manager) scan(id, realRoot string, actor sql.NullInt64) {
	defer m.wg.Done()
	defer func() {
		m.mu.Lock()
		m.running = false
		m.mu.Unlock()
	}()

	ctx := context.Background()
	var sum ScanSummary

	walkErr := filepath.WalkDir(realRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable entry (permissions, a vanished file) is logged and
			// skipped — one bad subtree must not abort the whole scan.
			logf("sources: scan %s: walk %s: %v", id, path, err)
			return nil
		}
		if d.IsDir() {
			return nil
		}
		mime, ok := m.accepted[strings.ToLower(filepath.Ext(d.Name()))]
		if !ok {
			return nil // not an accepted audio file; ignore (not counted)
		}
		m.ingestOne(ctx, id, path, mime, actor, &sum)
		return nil
	})

	status := database.SourceStatusActive
	if walkErr != nil {
		// WalkDir only returns non-nil when the root itself is unreadable (our
		// walkFn always returns nil) — treat that as a failed scan.
		logf("sources: scan %s: %v", id, walkErr)
		status = database.SourceStatusError
	}

	summaryJSON, _ := json.Marshal(sum)
	if err := m.store.FinishDataSourceScan(ctx, id, status, string(summaryJSON), m.clock()); err != nil {
		logf("sources: scan %s: finish: %v", id, err)
	}
	logf("sources: scan %s done: scanned=%d linked=%d skipped=%d failed=%d status=%s",
		id, sum.Scanned, sum.Linked, sum.Skipped, sum.Failed, status)
}

// ingestOne references one audio file: hash it, skip if the links storage already
// has it, else create the symlink and a catalog row (reusing an existing files
// row for the same hash). It attributes the file to sourceID on every path —
// linked, reused, or skipped — so Refresh re-affirms attribution and Remove can
// reason about it (migration 023). It updates the running summary counts. All
// errors are per-file (logged, counted as Failed) so the scan continues.
func (m *Manager) ingestOne(ctx context.Context, sourceID, path, mime string, actor sql.NullInt64, sum *ScanSummary) {
	sum.Scanned++

	hash, size, err := hashFile(path)
	if err != nil {
		logf("sources: hash %s: %v", path, err)
		sum.Failed++
		return
	}

	// One link per hash: if the links storage already references this content,
	// a prior scan already linked and cataloged it — don't overwrite. Still
	// attribute it to this source (a hash may live under two source roots).
	if has, err := m.linker.Has(hash); err != nil {
		logf("sources: links probe %s: %v", hash, err)
		sum.Failed++
		return
	} else if has {
		sum.Skipped++
		m.attributeByHash(ctx, sourceID, hash)
		return
	}

	filename := filepath.Base(path)
	existing, err := m.store.GetFileByHash(ctx, hash)
	if err != nil {
		logf("sources: lookup %s: %v", hash, err)
		sum.Failed++
		return
	}

	created, err := m.linker.Link(hash, filename, path)
	if err != nil {
		logf("sources: link %s: %v", hash, err)
		sum.Failed++
		return
	}
	if !created {
		// A racing writer created the link between Has and Link; treat as skipped.
		sum.Skipped++
		return
	}

	// Content already in the catalog (e.g. a prior local upload): keep the
	// existing row and just record the new filename. The link is a resilient
	// duplicate; the resolver still serves local first.
	if existing != nil {
		if err := m.store.RecordUpload(ctx, existing.ID, filename); err != nil {
			logf("sources: record upload %s: %v", hash, err)
		}
		m.attribute(ctx, sourceID, existing.ID)
		sum.Linked++
		return
	}

	now := m.clock()
	tags := readTags(path, mime)
	f := &database.File{
		Hash:           hash,
		ByteSize:       size,
		MimeType:       mime,
		StorageBackend: database.StorageBackendLinks,
		ObjectKey:      hash + "/" + filename,
		LinkTarget:     sql.NullString{String: path, Valid: true},
		CreatedAt:      now,
		UploadedBy:     actor,
		// Symlink imports land approved: they come from an admin behind the
		// allow-list, not the public upload staging flow (data-sources decision).
		ReviewState: database.ReviewApproved,
	}
	upload := &database.FileUpload{Filename: filename, UploadedAt: now}
	meta := tagsToMetadata(tags, now)

	if err := m.store.InsertFile(ctx, f, upload, meta); err != nil {
		logf("sources: insert %s: %v", hash, err)
		// Roll back the just-created link so a re-scan can retry cleanly rather
		// than skipping a hash that has no catalog row.
		if rmErr := m.linker.Remove(hash); rmErr != nil {
			logf("sources: rollback link %s: %v", hash, rmErr)
		}
		sum.Failed++
		return
	}

	// Attribute the freshly inserted file to this source (migration 023).
	m.attribute(ctx, sourceID, f.ID)

	// Enqueue ingest analysis (ffprobe tech columns + fpcalc fingerprint →
	// recording resolution), exactly like an upload. Best-effort: the startup
	// backfill re-enqueues anything missed.
	if err := m.store.EnqueueAnalysisJob(ctx, f.ID, now); err != nil {
		logf("sources: enqueue analysis %s: %v", hash, err)
	} else if m.notify != nil {
		m.notify.Notify()
	}

	// Read-once-derive the album cover (sidecar or embedded) into owned variants,
	// when this is the album's first cover. Best-effort; never fails the import.
	m.maybeSaveCover(ctx, path, tags, now)

	sum.Linked++
}

// attribute records that sourceID references fileID (best-effort: a failure is
// logged, never aborts the scan — a missed attribution just leaves the file out
// of this source's removal/keep accounting, the safe direction).
func (m *Manager) attribute(ctx context.Context, sourceID string, fileID int64) {
	if err := m.store.AttributeSourceFile(ctx, sourceID, fileID); err != nil {
		logf("sources: attribute %s/%d: %v", sourceID, fileID, err)
	}
}

// attributeByHash attributes the catalog row for hash to sourceID (used on the
// skip path, where we have only the hash). A missing row is a no-op.
func (m *Manager) attributeByHash(ctx context.Context, sourceID, hash string) {
	f, err := m.store.GetFileByHash(ctx, hash)
	if err != nil {
		logf("sources: attribute lookup %s: %v", hash, err)
		return
	}
	if f != nil {
		m.attribute(ctx, sourceID, f.ID)
	}
}

// hashFile streams the SHA-256 of the file at path and returns the digest and
// byte size. The file is only read, never modified.
func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// readTags extracts audio tags from the file, degrading to empty tags on any
// error (an untagged or unreadable file still imports, it just carries no
// metadata) — mirroring the upload path's extractTagsOrEmpty.
func readTags(path, mime string) *media.Tags {
	f, err := os.Open(path)
	if err != nil {
		return &media.Tags{}
	}
	defer f.Close()
	tags, err := media.ExtractTags(f, mime)
	if err != nil {
		logf("sources: tag extraction %s: %v", path, err)
		return &media.Tags{}
	}
	return tags
}

// tagsToMetadata maps media.Tags onto database.MediaMetadata (empty strings and
// zero ints become NULL). Title may be empty here; InsertFile fills the required
// non-empty value from the filename (migration 016). This mirrors api's private
// helper of the same name; kept local to avoid an api→sources import cycle.
func tagsToMetadata(t *media.Tags, extractedAt int64) *database.MediaMetadata {
	return &database.MediaMetadata{
		Title:       t.Title,
		Artist:      nullString(t.Artist),
		Album:       nullString(t.Album),
		AlbumArtist: nullString(t.AlbumArtist),
		Genre:       nullString(t.Genre),
		Composer:    nullString(t.Composer),
		Comment:     nullString(t.Comment),
		TagFormat:   nullString(t.TagFormat),
		Year:        nullInt(t.Year),
		TrackNumber: nullInt(t.TrackNumber),
		TrackTotal:  nullInt(t.TrackTotal),
		DiscNumber:  nullInt(t.DiscNumber),
		ExtractedAt: extractedAt,
	}
}

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func nullInt(i int) sql.NullInt64 { return sql.NullInt64{Int64: int64(i), Valid: i != 0} }
