package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/federation"
)

// Madnetwork direct transfer (federation F3, docs/architecture/federation.md):
// the cache-through streaming relay for thin clients and download-to-library
// through the review bucket. Both ride federation.Node.EnsureBlob — concurrent
// streams and downloads of the same hash join one fetch.

// ── Download job registry ────────────────────────────────────────────────────

// mnJob tracks one download-to-library staging run. Process-global (a
// package-level map, shared across per-listener handler instances) and keyed
// by content hash — a second download of the same hash joins the running job.
type mnJob struct {
	mu       sync.Mutex
	state    string // "transferring" | "staging" | "staged" | "approved" | "attached" | "failed"
	errText  string
	tagsetID int64
	transfer federation.Transfer
}

func (j *mnJob) set(state, errText string) {
	j.mu.Lock()
	j.state, j.errText = state, errText
	j.mu.Unlock()
}

var mnJobs = struct {
	sync.Mutex
	m map[string]*mnJob
}{m: map[string]*mnJob{}}

// ── Streaming relay ──────────────────────────────────────────────────────────

// madnetworkStream handles GET /api/madnetwork/stream/{hash}: relays a remote
// blob to the browser as cache-through streaming — bytes are served as they
// arrive from the friend while the complete file lands in the cache in
// parallel (never download-fully-then-play). A hash the local library already
// holds, or a finished cache file, is served directly with full Range support.
func (h *handler) madnetworkStream(w http.ResponseWriter, r *http.Request) {
	hash := strings.ToLower(chi.URLParam(r, "hash"))
	if !isSHA256Hex(hash) {
		http.Error(w, "invalid hash", http.StatusBadRequest)
		return
	}
	t, err := h.ensureBlob(r.Context(), hash)
	if err != nil {
		if errors.Is(err, federation.ErrNoHolder) {
			http.Error(w, "no friend holds this content", http.StatusNotFound)
			return
		}
		http.Error(w, "transfer unavailable", http.StatusServiceUnavailable)
		return
	}
	// Wait for the origin's response headers / first byte, so size and
	// filename are stable before we commit to response headers.
	if err := t.WaitFor(r.Context(), 0); err != nil && err != io.EOF {
		if r.Context().Err() != nil {
			return // client went away while the mesh converged
		}
		http.Error(w, "transfer failed", http.StatusBadGateway)
		return
	}

	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Completed (local blob, cache hit, or a fetch that just finished):
	// http.ServeContent gives full native Range support.
	select {
	case <-t.Done():
		if t.Err() != nil {
			http.Error(w, "transfer failed", http.StatusBadGateway)
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
		name := t.Filename()
		if name == "" {
			name = hash // no extension — ServeContent falls back to sniffing
		}
		http.ServeContent(w, r, name, info.ModTime(), f)
		return
	default:
	}

	h.serveGrowingTransfer(w, r, t)
}

// serveGrowingTransfer relays an in-flight transfer: the total is known (the
// origin's Content-Length), so ranges work — a range beyond the downloaded
// prefix simply waits for the sequential fetch to reach it.
func (h *handler) serveGrowingTransfer(w http.ResponseWriter, r *http.Request, t federation.Transfer) {
	total := t.Size()
	ctype := audioTypeForFilename(t.Filename())
	w.Header().Set("Content-Type", ctype)
	if total <= 0 {
		// Origin sent no length (shouldn't happen — ServeContent always sets
		// one): stream everything without range support.
		h.copyTransfer(r.Context(), w, t, 0, -1)
		return
	}
	w.Header().Set("Accept-Ranges", "bytes")
	start, end, partial, ok := parseByteRange(r.Header.Get("Range"), total)
	if !ok {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", total))
		http.Error(w, "range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	if partial {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		w.WriteHeader(http.StatusPartialContent)
	}
	h.copyTransfer(r.Context(), w, t, start, end)
}

// copyTransfer copies [start, end] (end < 0 = to EOF) from the transfer's
// growing file to the client, waiting for the fetch whenever it catches up.
func (h *handler) copyTransfer(ctx context.Context, w http.ResponseWriter, t federation.Transfer, start, end int64) {
	f, err := t.Open()
	if err != nil {
		return // headers may be written already — just drop the connection
	}
	defer f.Close()
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return
	}
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 64<<10)
	offset := start
	for end < 0 || offset <= end {
		if err := t.WaitFor(ctx, offset); err != nil {
			return // EOF (done), client gone, or the transfer failed midway
		}
		n := int64(len(buf))
		if avail := t.Available(offset); avail < n {
			n = avail
		}
		if end >= 0 && end-offset+1 < n {
			n = end - offset + 1
		}
		if n <= 0 {
			continue // offset briefly unavailable (e.g. a swarm→whole-file fallback); re-wait
		}
		rn, rerr := f.Read(buf[:n])
		if rn > 0 {
			if _, werr := w.Write(buf[:rn]); werr != nil {
				return
			}
			offset += int64(rn)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil && rerr != io.EOF {
			return
		}
	}
}

// parseByteRange parses a single-range Range header against a known total.
// No/foreign/multi-range headers serve the full entity (spec-permitted);
// a syntactically valid but unsatisfiable range reports !ok (416).
func parseByteRange(spec string, total int64) (start, end int64, partial, ok bool) {
	full := func() (int64, int64, bool, bool) { return 0, total - 1, false, true }
	spec = strings.TrimSpace(spec)
	if !strings.HasPrefix(spec, "bytes=") {
		return full()
	}
	spec = strings.TrimPrefix(spec, "bytes=")
	if strings.Contains(spec, ",") {
		return full() // multi-range unsupported — serve the full entity
	}
	dash := strings.Index(spec, "-")
	if dash < 0 {
		return full()
	}
	startStr, endStr := strings.TrimSpace(spec[:dash]), strings.TrimSpace(spec[dash+1:])
	if startStr == "" { // suffix form: the last N bytes
		n, err := strconv.ParseInt(endStr, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false, false
		}
		if n > total {
			n = total
		}
		return total - n, total - 1, true, true
	}
	s, err := strconv.ParseInt(startStr, 10, 64)
	if err != nil || s < 0 || s >= total {
		return 0, 0, false, false
	}
	e := total - 1
	if endStr != "" {
		e, err = strconv.ParseInt(endStr, 10, 64)
		if err != nil || e < s {
			return 0, 0, false, false
		}
		if e > total-1 {
			e = total - 1
		}
	}
	return s, e, true, true
}

// audioTypeForFilename maps a filename to its canonical audio MIME via the
// upload allow-list; unknown extensions degrade to octet-stream.
func audioTypeForFilename(name string) string {
	if mt, ok := acceptedAudioTypes[strings.ToLower(filepath.Ext(name))]; ok {
		return mt
	}
	return "application/octet-stream"
}

// ── Download to library ──────────────────────────────────────────────────────

// madnetworkDownload handles POST /api/madnetwork/download {hash}: fetch the
// blob from a friend and stage it into the library through the review bucket
// as the caller's draft — download = fetch + stage, never a silent publish
// (unless the admin enabled autoapprove_downloads). Bytes the library already
// holds skip the fetch: the remote tagset attaches as a new draft appearance
// of the same recording. Responds immediately; progress is polled via
// GET /api/madnetwork/transfers/{hash}.
func (h *handler) madnetworkDownload(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Hash string `json:"hash"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	hash := strings.ToLower(strings.TrimSpace(body.Hash))
	if !isSHA256Hex(hash) {
		http.Error(w, "invalid hash (want 64 hex chars)", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	entry, err := h.madnetwork.MadnetworkEntryForHash(ctx, hash)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	// No live claim is NOT a refusal when we already hold the bytes
	// (docs/architecture/madnetwork-cache.md). Materializing stages into the
	// review bucket exactly like an upload, and an upload takes its metadata from
	// the file's own tags — so a cached blob whose source has left the network
	// has everything it needs right here. Only a hash we neither hold nor can
	// find a holder for is a 404.
	if entry == nil && !h.holdsCached(hash) {
		http.Error(w, "no friend advertises this content", http.StatusNotFound)
		return
	}
	actor := actorID(ctx)

	// Bytes already on this node: no fetch — offer the remote tagset as a new
	// appearance draft on the held recording (same flow as a byte-dup upload).
	existing, err := h.repo.GetFileByHash(ctx, hash)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if existing != nil {
		if existing.DeletedAt.Valid {
			writeJSON(w, http.StatusConflict, map[string]any{
				"ok": false, "error": "this content is in the trash — restore it instead of re-downloading"})
			return
		}
		if entry == nil {
			// We hold the bytes and nobody describes them: there is no remote
			// appearance to offer, and the library's own already exists.
			h.evictCached(hash)
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": true, "existed": true, "attached": false})
			return
		}
		tid, created, err := h.attachRemoteTagset(ctx, existing.ID, actor, entry)
		if err != nil {
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
		h.evictCached(hash) // a stream may have cached what the library already holds
		h.audit(ctx, "madnetwork.download", hash, "bytes already held; appearance attached="+strconv.FormatBool(created))
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "existed": true, "attached": created, "tagset_id": tid})
		return
	}

	mnJobs.Lock()
	if job, ok := mnJobs.m[hash]; ok {
		job.mu.Lock()
		running := job.state == "transferring" || job.state == "staging"
		job.mu.Unlock()
		if running {
			mnJobs.Unlock()
			writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "started": true, "joined": true})
			return
		}
	}
	job := &mnJob{state: "transferring"}
	mnJobs.m[hash] = job
	mnJobs.Unlock()

	go h.runMadnetworkDownload(hash, entry, actor, job)
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "started": true})
}

// madnetworkTransferStatus handles GET /api/madnetwork/transfers/{hash}: the
// download job's state plus live transfer progress, for the page's polling.
func (h *handler) madnetworkTransferStatus(w http.ResponseWriter, r *http.Request) {
	hash := strings.ToLower(chi.URLParam(r, "hash"))
	if !isSHA256Hex(hash) {
		http.Error(w, "invalid hash", http.StatusBadRequest)
		return
	}
	mnJobs.Lock()
	job := mnJobs.m[hash]
	mnJobs.Unlock()
	if job == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": "none"})
		return
	}
	job.mu.Lock()
	resp := map[string]any{"ok": true, "state": job.state}
	if job.errText != "" {
		resp["error"] = job.errText
	}
	if job.tagsetID != 0 {
		resp["tagset_id"] = job.tagsetID
	}
	if t := job.transfer; t != nil {
		resp["progress"] = t.Progress()
		resp["size"] = t.Size()
	}
	job.mu.Unlock()
	writeJSON(w, http.StatusOK, resp)
}

// runMadnetworkDownload is the background staging goroutine: wait out the
// fetch (EnsureBlob verified the bytes against the content hash), land the
// blob in local storage, and insert it as the downloader's draft carrying the
// remote tagset text — or approved directly under autoapprove_downloads. The
// analysis pipeline then fingerprints the file locally (fpcalc) and resolves
// its recording — remote claims stay hints, local verification is the truth.
func (h *handler) runMadnetworkDownload(hash string, entry *federation.CatalogEntry, actor sql.NullInt64, job *mnJob) {
	ctx := context.Background()
	fail := func(err error) {
		log.Printf("madnetwork download %s: %v", hash, err)
		job.set("failed", err.Error())
	}
	// A panic in here would otherwise take the whole server down: chi's Recoverer
	// wraps the handler, and this runs on its own goroutine long after that
	// handler answered. One materialize going wrong must cost that materialize.
	defer func() {
		if r := recover(); r != nil {
			log.Printf("madnetwork download %s: panic: %v", hash, r)
			job.set("failed", "staging failed unexpectedly")
		}
	}()
	t, err := h.ensureBlob(ctx, hash)
	if err != nil {
		fail(err)
		return
	}
	job.mu.Lock()
	job.transfer = t
	job.mu.Unlock()
	<-t.Done()
	if err := t.Err(); err != nil {
		fail(err)
		return
	}
	job.set("staging", "")

	filename := sanitizeFilename(t.Filename())
	ext := strings.ToLower(filepath.Ext(filename))
	if _, ok := acceptedAudioTypes[ext]; !ok {
		// The transfer's name is unusable. For a blob already in the cache that
		// is the normal case, not a fault — a finished transfer is named after
		// its own path, which for a cache file is the hash — so ask the index,
		// which kept the origin's name across the restart.
		if indexed := h.cachedDownloadName(ctx, hash, ""); indexed != hash {
			if e := strings.ToLower(filepath.Ext(indexed)); acceptedAudioTypes[e] != "" {
				filename, ext = indexed, e
			}
		}
	}
	if _, ok := acceptedAudioTypes[ext]; !ok {
		// Ask the file what it is. This is the arm that carries the offline case:
		// a blob adopted into the index has no remembered name, and offline there
		// is no claim to fall back on either.
		if own := h.cachedStagingName(hash); own != "" {
			if e := strings.ToLower(filepath.Ext(own)); acceptedAudioTypes[e] != "" {
				filename, ext = own, e
			}
		}
	}
	if _, ok := acceptedAudioTypes[ext]; !ok {
		if entry == nil {
			// Nothing left to ask: no claim describes it, nothing remembered its
			// name, and its own header is not one we accept.
			fail(fmt.Errorf("this cached file is not a recognised audio format"))
			return
		}
		// Origin filename unusable — synthesize one from the tagset text and
		// the advertised codec so the staged file still looks sane.
		ext = extForCodec(entryRenditionCodec(entry, hash))
		if ext == "" {
			fail(fmt.Errorf("origin offers no usable filename or codec"))
			return
		}
		filename = sanitizeFilename(downloadBaseName(entry) + ext)
	}
	mimeType := acceptedAudioTypes[ext]

	// Late byte-dup check: the blob may have entered the library while the
	// fetch ran (e.g. a parallel upload of the same bytes).
	if existing, err := h.repo.GetFileByHash(ctx, hash); err == nil && existing != nil && !existing.DeletedAt.Valid {
		if entry == nil {
			h.evictCached(hash)
			job.set("attached", "")
			return
		}
		tid, _, aerr := h.attachRemoteTagset(ctx, existing.ID, actor, entry)
		if aerr != nil {
			fail(aerr)
			return
		}
		job.mu.Lock()
		job.tagsetID = tid
		job.mu.Unlock()
		h.evictCached(hash) // held locally already: same duplicate, same fix
		job.set("attached", "")
		return
	}

	f, err := t.Open()
	if err != nil {
		fail(err)
		return
	}
	now := time.Now().Unix()
	// With no claim to carry, the staged appearance takes its tags the way an
	// upload does — out of the file itself. Read before storage.Put, which
	// consumes the reader; extractTagsOrEmpty rewinds either way.
	//
	// The nil check has to come FIRST: entryToMetadata dereferences its argument,
	// so computing it and overwriting afterwards panics — and this runs in a
	// goroutine, where a panic takes the whole process down rather than one
	// request (chi's Recoverer only wraps the handler).
	var meta *database.MediaMetadata
	if entry != nil {
		meta = entryToMetadata(entry, now)
	} else {
		meta = tagsToMetadata(extractTagsOrEmpty(f, mimeType), now)
	}
	err = h.storage.Put(hash, filename, f)
	f.Close()
	if err != nil {
		fail(fmt.Errorf("store blob: %w", err))
		return
	}

	policy, err := h.madnetwork.GetMadnetworkPolicy(ctx)
	if err != nil {
		fail(err)
		return
	}
	reviewState := database.ReviewDraft
	if policy.AutoapproveDownloads || !h.authzEnabled {
		reviewState = database.ReviewApproved
	}
	file := &database.File{
		Hash:           hash,
		ByteSize:       t.Size(),
		MimeType:       mimeType,
		StorageBackend: "local",
		ObjectKey:      hash + "/" + filename,
		CreatedAt:      now,
		UploadedBy:     actor,
		ReviewState:    reviewState,
	}
	upload := &database.FileUpload{Filename: filename, UploadedAt: now}
	if err := h.repo.InsertFile(ctx, file, upload, meta); err != nil {
		log.Printf("orphan blob (madnetwork download): hash=%s err=%v", hash, err)
		fail(fmt.Errorf("record download: %w", err))
		return
	}
	if err := h.repo.EnqueueAnalysisJob(ctx, file.ID, now); err != nil {
		log.Printf("enqueue analysis job (madnetwork download): hash=%s err=%v", hash, err)
	} else if h.mediaPool != nil {
		h.mediaPool.Notify()
	}
	// The blob is in the library now, so the cache copy is a duplicate served
	// under a different rule — drop it (.issues/open-issues.md, "Cache seeding
	// overrides a recording's sharing scope"). Best-effort: the download
	// succeeded, and a stale cache entry is a leak to fix, not a reason to fail
	// the fetch. The startup sweep catches whatever this misses.
	h.evictCached(hash)

	h.audit(ctx, "madnetwork.download", hash, "staged as "+reviewState+": "+filename)
	if reviewState == database.ReviewApproved {
		h.repointRemotes(ctx) // the materialized blob may back remote playlist rows
		job.set("approved", "")
	} else {
		job.set("staged", "")
	}
}

// evictCached drops the download-cache copy of a blob the library holds, and
// swallows the outcome on purpose: every caller has already succeeded at the
// thing the user asked for, and a cache entry that outlives its eviction is
// picked up by the startup sweep (database.EvictCachedMadnetworkBlobs).
func (h *handler) evictCached(hash string) {
	if h.federation == nil {
		return
	}
	if err := h.federation.EvictCachedBlob(hash); err != nil {
		log.Printf("evict cached blob %s: %v", hash, err)
	}
	// The file is what the swarm reads, so it goes first; the index row only
	// describes it (docs/architecture/madnetwork-cache.md).
	h.dropCacheIndex(hash)
}

// attachRemoteTagset offers the remote entry as a draft appearance on a blob
// the library already holds; under autoapprove_downloads the draft approves
// itself (the doc's one-click path, minus the click).
func (h *handler) attachRemoteTagset(ctx context.Context, fileID int64, actor sql.NullInt64, entry *federation.CatalogEntry) (int64, bool, error) {
	now := time.Now().Unix()
	tid, created, err := h.repo.AttachDraftTagset(ctx, fileID, actor, entryToMetadata(entry, now), "")
	if err != nil || !created {
		return tid, created, err
	}
	policy, err := h.madnetwork.GetMadnetworkPolicy(ctx)
	if err != nil {
		return tid, created, err
	}
	if policy.AutoapproveDownloads {
		if _, err := h.repo.UpdateReviewState(ctx, tid, database.ReviewTransition{
			From: []string{database.ReviewDraft}, To: database.ReviewApproved, StampSubmittedAt: true,
		}); err != nil {
			return tid, created, err
		}
		h.repointRemotes(ctx)
	}
	return tid, created, nil
}

// entryToMetadata converts a cached catalog entry's tagset text into staging
// metadata — the downloaded appearance carries what the user saw and chose in
// the madnetwork view, not whatever tags the blob embeds (the suggestions
// panel can still read those during review).
func entryToMetadata(e *federation.CatalogEntry, now int64) *database.MediaMetadata {
	m := &database.MediaMetadata{
		Title:       e.Title,
		Artist:      nullString(e.Artist),
		AlbumArtist: nullString(e.AlbumArtist),
		Album:       nullString(e.Album),
		Genre:       nullString(e.Genre),
		ExtractedAt: now,
	}
	setNullable := func(dst *sql.NullInt64, src *int64) {
		if src != nil {
			*dst = sql.NullInt64{Int64: *src, Valid: true}
		}
	}
	setNullable(&m.Year, e.Year)
	setNullable(&m.TrackNumber, e.TrackNumber)
	setNullable(&m.DiscNumber, e.DiscNumber)
	return m
}

// entryRenditionCodec returns the advertised codec of the entry's rendition
// with the given hash.
func entryRenditionCodec(e *federation.CatalogEntry, hash string) string {
	for _, rd := range e.Renditions {
		if rd.Hash == hash {
			return rd.Codec
		}
	}
	return ""
}

// extForCodec maps an advertised codec to a canonical extension from the
// accepted-types allow-list (the reverse of what ffprobe reports).
func extForCodec(codec string) string {
	switch strings.ToLower(codec) {
	case "mp3":
		return ".mp3"
	case "flac":
		return ".flac"
	case "aac", "alac":
		return ".m4a"
	case "vorbis":
		return ".ogg"
	case "opus":
		return ".opus"
	case "wav", "pcm_s16le", "pcm_s24le":
		return ".wav"
	}
	return ""
}

// extForFileType maps the CONTAINER a tag reader recognised in the bytes
// (media.Tags.FileType) to a filename extension. The last resort for naming a
// blob whose filename was never recorded — see cachedStagingName.
func extForFileType(fileType string) string {
	switch strings.ToUpper(fileType) {
	case "MP3":
		return ".mp3"
	case "FLAC":
		return ".flac"
	case "M4A", "M4B", "M4P", "ALAC":
		return ".m4a"
	case "OGG":
		return ".ogg"
	}
	return ""
}

// cachedStagingName names a cached blob for staging when nothing else can: no
// usable transfer filename, nothing remembered in the index, and no node
// describing it. It asks the FILE what it is — the container from its own
// header, the stem from its own tags — and returns "" only when even that fails.
//
// This is what makes materializing work OFFLINE, which is the entire reason the
// button exists (docs/architecture/madnetwork-cache.md §Row actions). Every node
// that upgrades into the cache index adopts its existing blobs with no
// filename — the origin's name was never recorded before there was anywhere to
// put it — so without this, a whole cache would be unmaterializable on exactly
// the devices that have no other way to get the file.
func (h *handler) cachedStagingName(hash string) string {
	if h.cacheDir == "" {
		return ""
	}
	f, err := os.Open(filepath.Join(h.cacheDir, hash))
	if err != nil {
		return ""
	}
	defer f.Close()
	tags := extractTagsOrEmpty(f, "")
	ext := extForFileType(tags.FileType)
	if ext == "" {
		return ""
	}
	stem := strings.TrimSpace(tags.Title)
	if stem != "" && strings.TrimSpace(tags.Artist) != "" {
		stem = tags.Artist + " - " + stem
	}
	if stem == "" {
		stem = hash[:16] // untagged: the digest is a poor name, but it is a name
	}
	return sanitizeFilename(stem + ext)
}

// downloadBaseName builds a display filename stem from the entry text.
func downloadBaseName(e *federation.CatalogEntry) string {
	if e.Artist != "" {
		return e.Artist + " - " + e.Title
	}
	return e.Title
}

// removeMadnetworkJob is a test hook: forget a finished job so a fresh run can
// be observed.
func removeMadnetworkJob(hash string) {
	mnJobs.Lock()
	delete(mnJobs.m, hash)
	mnJobs.Unlock()
}
