package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"daemonlord.ygg/madshare/api/storage"
	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/media"
)

// ---- ID3v2.3 fixture builder ------------------------------------------------
//
// dhowden/tag is read-only, so Phase 2 tests synthesise a minimal but valid
// ID3v2.3 MP3 carrying TALB (album), TPE2 (album artist), and an APIC (cover)
// frame. tag.ReadFrom parses metadata from the leading ID3 tag alone, so no
// real audio frames are needed.

// id3v23Frame wraps a frame body in an ID3v2.3 frame header (ID, 4-byte
// big-endian size, 2 flag bytes). Note: v2.3 frame sizes are plain big-endian,
// unlike the synchsafe tag-header size.
func id3v23Frame(id string, body []byte) []byte {
	out := append([]byte{}, id...)
	n := len(body)
	out = append(out, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	out = append(out, 0x00, 0x00) // flags
	return append(out, body...)
}

// id3v23TextFrame builds an ISO-8859-1 text frame (encoding byte 0x00).
func id3v23TextFrame(id, text string) []byte {
	return id3v23Frame(id, append([]byte{0x00}, text...))
}

// id3v23APICFrame builds an APIC frame: encoding, null-terminated MIME type,
// picture type (front cover), empty description, then the raw picture bytes.
func id3v23APICFrame(mimeType string, pic []byte) []byte {
	body := []byte{0x00} // encoding: ISO-8859-1
	body = append(body, mimeType...)
	body = append(body, 0x00) // MIME terminator
	body = append(body, 0x03) // picture type: cover (front)
	body = append(body, 0x00) // empty description terminator
	body = append(body, pic...)
	return id3v23Frame("APIC", body)
}

// buildMP3Tags assembles an ID3v2.3-tagged MP3 with the given album (TALB),
// album artist (TPE2), track artist (TPE1), and cover (APIC). Empty strings or
// a nil pic omit the corresponding frame.
func buildMP3Tags(album, albumArtist, artist, picMIME string, pic []byte) []byte {
	var frames []byte
	if album != "" {
		frames = append(frames, id3v23TextFrame("TALB", album)...)
	}
	if albumArtist != "" {
		frames = append(frames, id3v23TextFrame("TPE2", albumArtist)...)
	}
	if artist != "" {
		frames = append(frames, id3v23TextFrame("TPE1", artist)...)
	}
	if pic != nil {
		frames = append(frames, id3v23APICFrame(picMIME, pic)...)
	}

	hdr := []byte{'I', 'D', '3', 0x03, 0x00, 0x00} // "ID3", v2.3.0, no flags
	n := len(frames)
	// Tag size is a 28-bit synchsafe integer (7 bits per byte).
	hdr = append(hdr, byte((n>>21)&0x7f), byte((n>>14)&0x7f), byte((n>>7)&0x7f), byte(n&0x7f))
	return append(hdr, frames...)
}

// buildMP3WithCover is the common case: album + album artist (TPE2) + cover.
func buildMP3WithCover(album, albumArtist, picMIME string, pic []byte) []byte {
	return buildMP3Tags(album, albumArtist, "", picMIME, pic)
}

// fakeJPEG is a stand-in cover blob. Phase 2 only stores the embedded bytes and
// queues a job; it never decodes here (that is the worker's job, tested in
// imageproc). The leading bytes are a JPEG SOI so the MIME stays plausible.
var fakeJPEG = append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, []byte("madshare-test-cover")...)

// ---- helpers ----------------------------------------------------------------

func jsonBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&m); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return m
}

// countPendingImageJobs returns the number of pending image_processing_jobs,
// reading through the handler's real *database.DB repo (newTestHandler wiring).
func countPendingImageJobs(t *testing.T, h *handler) int {
	t.Helper()
	db, ok := h.repo.(*database.DB)
	if !ok {
		t.Fatalf("handler repo is %T, want *database.DB", h.repo)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM image_processing_jobs WHERE status='pending'`).Scan(&n); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	return n
}

// ---- Phase 2: embedded cover extraction -------------------------------------

// TestUploadFile_ExtractsCover uploads an MP3 with embedded cover art and album
// tags. The album cover row and a pending variant job must be created, and the
// response must report cover_found + cover_processing.
func TestUploadFile_ExtractsCover(t *testing.T) {
	h, db, base := newTestHandler(t)
	mp3 := buildMP3WithCover("Dark Side", "Pink Floyd", "image/jpeg", fakeJPEG)

	rr := httptest.NewRecorder()
	h.uploadFile(rr, buildUploadRequest(t, "file", "track01.mp3", "audio/mpeg", mp3))
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rr.Code, rr.Body.String())
	}

	resp := jsonBody(t, rr)
	if resp["cover_found"] != true {
		t.Errorf("cover_found = %v, want true", resp["cover_found"])
	}
	if resp["cover_processing"] != true {
		t.Errorf("cover_processing = %v, want true", resp["cover_processing"])
	}

	// album_images row exists for the album.
	if !albumHasCover(t, db, "Pink Floyd", "Dark Side") {
		t.Error("no album_images row created for the uploaded album")
	}

	// Exactly one pending variant job.
	var pending int
	db.QueryRow(`SELECT COUNT(*) FROM image_processing_jobs WHERE status='pending'`).Scan(&pending)
	if pending != 1 {
		t.Errorf("pending image jobs = %d, want 1", pending)
	}

	// The original cover file was written under imagesDir.
	baseKey := media.ImageHash(fakeJPEG)
	want := filepath.Join(base, "images", baseKey, "original.jpg")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("original cover not written at %s: %v", want, err)
	}

	// album / artist are echoed for the upload page's grouping & cover targeting.
	if resp["album"] != "Dark Side" {
		t.Errorf("album = %v, want Dark Side", resp["album"])
	}
	if resp["artist"] != "Pink Floyd" {
		t.Errorf("artist = %v, want Pink Floyd (effective album artist)", resp["artist"])
	}
}

// TestUploadFile_NoCoverOnDedup uploads the same file twice; the cover must be
// processed only on first ingest, never re-processed for the duplicate.
func TestUploadFile_NoCoverOnDedup(t *testing.T) {
	h, _, _ := newTestHandler(t)
	mp3 := buildMP3WithCover("Wish You Were Here", "Pink Floyd", "image/jpeg", fakeJPEG)

	first := httptest.NewRecorder()
	h.uploadFile(first, buildUploadRequest(t, "file", "a.mp3", "audio/mpeg", mp3))
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d; body: %s", first.Code, first.Body.String())
	}
	if got := countPendingImageJobs(t, h); got != 1 {
		t.Fatalf("after first upload pending jobs = %d, want 1", got)
	}

	second := httptest.NewRecorder()
	h.uploadFile(second, buildUploadRequest(t, "file", "a.mp3", "audio/mpeg", mp3))
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, want 200 (dedup); body: %s", second.Code, second.Body.String())
	}
	resp := jsonBody(t, second)
	if resp["existed"] != true {
		t.Errorf("existed = %v, want true on dedup", resp["existed"])
	}
	if resp["cover_found"] != false || resp["cover_processing"] != false {
		t.Errorf("dedup cover flags = (%v,%v), want (false,false)", resp["cover_found"], resp["cover_processing"])
	}
	if got := countPendingImageJobs(t, h); got != 1 {
		t.Errorf("after dedup pending jobs = %d, want 1 (no new job)", got)
	}
}

// albumHasCover reports whether the (artist, album) entity has a cover row,
// resolving the name to its album id first. A missing entity means no cover.
func albumHasCover(t *testing.T, db *database.DB, artist, album string) bool {
	t.Helper()
	id, found, err := db.LookupAlbumID(context.Background(), artist, album)
	if err != nil {
		t.Fatalf("LookupAlbumID: %v", err)
	}
	if !found {
		return false
	}
	has, err := db.HasAlbumCover(context.Background(), id)
	if err != nil {
		t.Fatalf("HasAlbumCover: %v", err)
	}
	return has
}

// TestUploadFile_FillIfMissing_SkipsExisting pre-seeds an album cover, then
// uploads a track with embedded art for the same album. The existing cover must
// not be overwritten and no new job enqueued.
func TestUploadFile_FillIfMissing_SkipsExisting(t *testing.T) {
	h, db, _ := newTestHandler(t)
	ctx := context.Background()

	// Pre-existing (e.g. manually uploaded) cover. Create the album entity first
	// (a manual cover upload would do this via ResolveAlbumID).
	albumID, err := db.ResolveAlbumID(ctx, "Pink Floyd", "Animals")
	if err != nil {
		t.Fatalf("resolve album: %v", err)
	}
	if err := db.SetAlbumCover(ctx, albumID, "deadbeefdeadbeef", ".png", "deadbeefdeadbeef/original.png", "image/png", 1); err != nil {
		t.Fatalf("seed cover: %v", err)
	}

	mp3 := buildMP3WithCover("Animals", "Pink Floyd", "image/jpeg", fakeJPEG)
	rr := httptest.NewRecorder()
	h.uploadFile(rr, buildUploadRequest(t, "file", "pigs.mp3", "audio/mpeg", mp3))
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d; body: %s", rr.Code, rr.Body.String())
	}
	resp := jsonBody(t, rr)
	// Embedded art was present (cover_found) but the album already had a cover,
	// so nothing was processed.
	if resp["cover_found"] != true {
		t.Errorf("cover_found = %v, want true", resp["cover_found"])
	}
	if resp["cover_processing"] != false {
		t.Errorf("cover_processing = %v, want false (existing cover wins)", resp["cover_processing"])
	}

	// No job enqueued, and the original base_key is untouched.
	if got := countPendingImageJobs(t, h); got != 0 {
		t.Errorf("pending jobs = %d, want 0 (fill-if-missing must not enqueue)", got)
	}
	baseKey, _, _, found, err := db.GetAlbumCoverStatus(ctx, albumID)
	if err != nil || !found {
		t.Fatalf("GetAlbumCoverStatus: found=%v err=%v", found, err)
	}
	if baseKey != "deadbeefdeadbeef" {
		t.Errorf("base_key = %q, want the pre-existing cover (not overwritten)", baseKey)
	}
}

// TestUploadFile_NoCoverWhenAlbumEmpty uploads a file with embedded art but no
// album tag. With no album to key the cover on, nothing is stored or enqueued.
func TestUploadFile_NoCoverWhenAlbumEmpty(t *testing.T) {
	h, _, _ := newTestHandler(t)
	// Album omitted; artist present.
	mp3 := buildMP3WithCover("", "Pink Floyd", "image/jpeg", fakeJPEG)

	rr := httptest.NewRecorder()
	h.uploadFile(rr, buildUploadRequest(t, "file", "untitled.mp3", "audio/mpeg", mp3))
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d; body: %s", rr.Code, rr.Body.String())
	}
	resp := jsonBody(t, rr)
	if resp["cover_found"] != false {
		t.Errorf("cover_found = %v, want false (no album tag)", resp["cover_found"])
	}
	if resp["cover_processing"] != false {
		t.Errorf("cover_processing = %v, want false", resp["cover_processing"])
	}
	if got := countPendingImageJobs(t, h); got != 0 {
		t.Errorf("pending jobs = %d, want 0", got)
	}
}

// TestUploadFile_ConcurrentSameAlbum_OneCover uploads several distinct tracks of
// the same album at the same time, each carrying a DIFFERENT embedded cover. The
// fill-if-missing rule must resolve the race atomically: exactly one cover row,
// exactly one variant job, and exactly one upload reporting cover_processing —
// no overwrites, no double-queue. (This is the case flagged in review: the cheap
// HasAlbumCover pre-check is not enough; SetAlbumCoverIfAbsent's atomic
// INSERT ... ON CONFLICT is what makes it correct.)
func TestUploadFile_ConcurrentSameAlbum_OneCover(t *testing.T) {
	// Use an on-disk DB so the connection pool is unbounded (WAL, multi-conn);
	// :memory: pins SetMaxOpenConns(1) and would serialise the writers, hiding
	// the very race this test exists to cover.
	base := t.TempDir()
	db, err := database.Open(filepath.Join(t.TempDir(), "race.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	h := &handler{
		storage:         storage.NewLocal(base),
		repo:            db,
		spoolDir:        t.TempDir(),
		imagesDir:       filepath.Join(base, "images"),
		sourceImagesDir: filepath.Join(base, "images"),
		maxUploadSize:   testMaxUpload,
	}
	const tracks = 8

	type result struct {
		code       int
		processing bool
	}
	results := make([]result, tracks)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < tracks; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Same album+artist, but a per-track distinct cover → distinct base_key,
			// so without the atomic claim each would try to write a row and a job.
			pic := append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, []byte(fmt.Sprintf("cover-%d", i))...)
			mp3 := buildMP3WithCover("The Wall", "Pink Floyd", "image/jpeg", pic)
			fn := fmt.Sprintf("track%02d.mp3", i)
			rr := httptest.NewRecorder()
			<-start // release all goroutines together
			h.uploadFile(rr, buildUploadRequest(t, "file", fn, "audio/mpeg", mp3))
			resp := jsonBody(t, rr)
			proc, _ := resp["cover_processing"].(bool)
			results[i] = result{code: rr.Code, processing: proc}
		}(i)
	}
	close(start)
	wg.Wait()

	processingCount := 0
	for i, r := range results {
		if r.code != http.StatusCreated {
			t.Errorf("track %d: status = %d, want 201", i, r.code)
		}
		if r.processing {
			processingCount++
		}
	}
	if processingCount != 1 {
		t.Errorf("cover_processing true count = %d, want exactly 1", processingCount)
	}

	// Exactly one album_images row and one pending job survive the race.
	albumID, found, err := db.LookupAlbumID(context.Background(), "Pink Floyd", "The Wall")
	if err != nil || !found {
		t.Fatalf("LookupAlbumID: found=%v err=%v", found, err)
	}
	var rows int
	db.QueryRow(`SELECT COUNT(*) FROM album_images WHERE album_id = ?`, albumID).Scan(&rows)
	if rows != 1 {
		t.Errorf("album_images rows = %d, want 1", rows)
	}
	if got := countPendingImageJobs(t, h); got != 1 {
		t.Errorf("pending image jobs = %d, want 1", got)
	}
}

// TestUploadFile_UnsupportedEmbeddedFormatSkipped covers an embedded cover whose
// MIME type the variant pipeline cannot process (e.g. GIF): it is skipped
// cleanly rather than queuing a doomed job.
func TestUploadFile_UnsupportedEmbeddedFormatSkipped(t *testing.T) {
	h, db, _ := newTestHandler(t)
	mp3 := buildMP3WithCover("Meddle", "Pink Floyd", "image/gif", []byte("GIF89a-fake"))

	rr := httptest.NewRecorder()
	h.uploadFile(rr, buildUploadRequest(t, "file", "echoes.mp3", "audio/mpeg", mp3))
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d; body: %s", rr.Code, rr.Body.String())
	}
	resp := jsonBody(t, rr)
	// cover_found reflects that embedded art with album+artist context exists;
	// cover_processing is false because the format is unsupported.
	if resp["cover_found"] != true {
		t.Errorf("cover_found = %v, want true", resp["cover_found"])
	}
	if resp["cover_processing"] != false {
		t.Errorf("cover_processing = %v, want false (unsupported format)", resp["cover_processing"])
	}
	if albumHasCover(t, db, "Pink Floyd", "Meddle") {
		t.Error("album cover row created for unsupported embedded format")
	}
	if got := countPendingImageJobs(t, h); got != 0 {
		t.Errorf("pending jobs = %d, want 0", got)
	}
}

// TestUploadFile_OversizedEmbeddedCoverSkipped verifies an embedded cover larger
// than the manual-upload cap (maxImageSize) is skipped: no file written, no row,
// no job. Guards against disk-write amplification via a crafted oversized APIC.
func TestUploadFile_OversizedEmbeddedCoverSkipped(t *testing.T) {
	h, db, base := newTestHandler(t)
	// One byte over the cap; valid JPEG SOI prefix so MIME stays plausible.
	huge := make([]byte, maxImageSize+1)
	copy(huge, []byte{0xFF, 0xD8, 0xFF, 0xE0})
	mp3 := buildMP3WithCover("Obscured", "Pink Floyd", "image/jpeg", huge)

	rr := httptest.NewRecorder()
	h.uploadFile(rr, buildUploadRequest(t, "file", "clouds.mp3", "audio/mpeg", mp3))
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d; body: %s", rr.Code, rr.Body.String())
	}
	resp := jsonBody(t, rr)
	if resp["cover_found"] != true {
		t.Errorf("cover_found = %v, want true (art was present)", resp["cover_found"])
	}
	if resp["cover_processing"] != false {
		t.Errorf("cover_processing = %v, want false (over size cap)", resp["cover_processing"])
	}
	if albumHasCover(t, db, "Pink Floyd", "Obscured") {
		t.Error("album cover row created for oversized embedded cover")
	}
	if got := countPendingImageJobs(t, h); got != 0 {
		t.Errorf("pending jobs = %d, want 0", got)
	}
	// Nothing should have been written under imagesDir.
	if entries, _ := os.ReadDir(filepath.Join(base, "images")); len(entries) != 0 {
		t.Errorf("imagesDir has %d entries, want 0 (no oversized cover written)", len(entries))
	}
}

// TestUploadFile_ArtistFallbackFromTPE1 verifies the cover is keyed on the track
// artist (TPE1) when no album artist (TPE2) is present — exercising
// effectiveAlbumArtist's fallback directly.
func TestUploadFile_ArtistFallbackFromTPE1(t *testing.T) {
	h, db, _ := newTestHandler(t)
	// Album + track artist only; no album artist.
	mp3 := buildMP3Tags("Summer", "", "Lone Artist", "image/jpeg", fakeJPEG)

	rr := httptest.NewRecorder()
	h.uploadFile(rr, buildUploadRequest(t, "file", "solo.mp3", "audio/mpeg", mp3))
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d; body: %s", rr.Code, rr.Body.String())
	}
	resp := jsonBody(t, rr)
	if resp["cover_processing"] != true {
		t.Errorf("cover_processing = %v, want true", resp["cover_processing"])
	}
	// Cover must be keyed on the track artist (the TPE1 fallback), not "".
	if !albumHasCover(t, db, "Lone Artist", "Summer") {
		t.Error("cover not keyed on track artist via TPE1 fallback")
	}
}

// TestUploadFile_ConcurrentSameAlbumIdenticalArt_OneFile is the common real
// case: all tracks of an album embed identical cover art (same base_key). The
// concurrent fill-if-missing must still settle on one row + one job, and because
// the bytes are identical there must be exactly one original file on disk (no
// orphans).
func TestUploadFile_ConcurrentSameAlbumIdenticalArt_OneFile(t *testing.T) {
	base := t.TempDir()
	db, err := database.Open(filepath.Join(t.TempDir(), "identical.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	h := &handler{
		storage:         storage.NewLocal(base),
		repo:            db,
		spoolDir:        t.TempDir(),
		imagesDir:       filepath.Join(base, "images"),
		sourceImagesDir: filepath.Join(base, "images"),
		maxUploadSize:   testMaxUpload,
	}

	const tracks = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < tracks; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Same album AND same cover bytes across all tracks → one base_key.
			// Append per-track trailing bytes so each file hashes distinctly
			// (otherwise identical tags+cover would dedup to a single upload).
			mp3 := append(buildMP3WithCover("Division Bell", "Pink Floyd", "image/jpeg", fakeJPEG),
				[]byte(fmt.Sprintf("\x00audio-%d", i))...)
			fn := fmt.Sprintf("track%02d.mp3", i)
			rr := httptest.NewRecorder()
			<-start
			h.uploadFile(rr, buildUploadRequest(t, "file", fn, "audio/mpeg", mp3))
			if rr.Code != http.StatusCreated {
				t.Errorf("track %d: status = %d, want 201", i, rr.Code)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	albumID, found, err := db.LookupAlbumID(context.Background(), "Pink Floyd", "Division Bell")
	if err != nil || !found {
		t.Fatalf("LookupAlbumID: found=%v err=%v", found, err)
	}
	var rows int
	db.QueryRow(`SELECT COUNT(*) FROM album_images WHERE album_id = ?`, albumID).Scan(&rows)
	if rows != 1 {
		t.Errorf("album_images rows = %d, want 1", rows)
	}
	if got := countPendingImageJobs(t, h); got != 1 {
		t.Errorf("pending image jobs = %d, want 1", got)
	}
	// Exactly one base_key dir holding exactly the one original file — identical
	// art means no per-track orphans.
	baseKey := media.ImageHash(fakeJPEG)
	dirs, _ := os.ReadDir(filepath.Join(base, "images"))
	if len(dirs) != 1 || dirs[0].Name() != baseKey {
		t.Errorf("imagesDir dirs = %v, want exactly [%s]", dirNames(dirs), baseKey)
	}
	if len(dirs) == 1 {
		files, _ := os.ReadDir(filepath.Join(base, "images", baseKey))
		if len(files) != 1 {
			t.Errorf("base_key dir has %d files, want 1 (original only)", len(files))
		}
	}
}

func dirNames(es []os.DirEntry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Name()
	}
	return out
}

// ---- Phase 4: upload concurrency limit (429) --------------------------------

// TestUploadFile_ServerLimitReturns429 pre-saturates the global cap, then asserts
// the next upload is rejected with 429 and the upload_limit JSON contract.
func TestUploadFile_ServerLimitReturns429(t *testing.T) {
	h, _, _ := newTestHandler(t)
	h.limiter = NewUploadLimiter(1, 0) // global cap 1
	if err := h.limiter.Acquire("someone-else"); err != nil {
		t.Fatalf("pre-saturate: %v", err)
	}

	rr := httptest.NewRecorder()
	h.uploadFile(rr, buildUploadRequest(t, "file", "song.mp3", "audio/mpeg", []byte("audio")))

	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body: %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Retry-After"); got == "" {
		t.Error("missing Retry-After header on 429")
	}
	resp := jsonBody(t, rr)
	if resp["code"] != "upload_limit" {
		t.Errorf("code = %v, want upload_limit", resp["code"])
	}
	if resp["error"] != "server upload limit reached" {
		t.Errorf("error = %v, want server upload limit reached", resp["error"])
	}
}

// TestUploadFile_UserLimitReturns429 pre-saturates the per-user cap for the
// anonymous ("") key (tests run without auth), then asserts the next upload from
// the same identity is rejected with the user-limit message.
func TestUploadFile_UserLimitReturns429(t *testing.T) {
	h, _, _ := newTestHandler(t)
	h.limiter = NewUploadLimiter(0, 1) // per-user cap 1
	if err := h.limiter.Acquire(""); err != nil {
		t.Fatalf("pre-saturate: %v", err)
	}

	rr := httptest.NewRecorder()
	h.uploadFile(rr, buildUploadRequest(t, "file", "song.mp3", "audio/mpeg", []byte("audio")))

	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body: %s", rr.Code, rr.Body.String())
	}
	resp := jsonBody(t, rr)
	if resp["code"] != "upload_limit" {
		t.Errorf("code = %v, want upload_limit", resp["code"])
	}
	if resp["error"] != "user upload limit reached" {
		t.Errorf("error = %v, want user upload limit reached", resp["error"])
	}
}

// TestUploadFile_LimiterReleasedAfterUpload verifies the slot is released after a
// successful upload, so a serverMax=1 limiter admits sequential uploads.
func TestUploadFile_LimiterReleasedAfterUpload(t *testing.T) {
	h, _, _ := newTestHandler(t)
	h.limiter = NewUploadLimiter(1, 0)

	for i := 0; i < 3; i++ {
		rr := httptest.NewRecorder()
		body := []byte(fmt.Sprintf("audio-%d", i)) // distinct bytes → not dedup
		h.uploadFile(rr, buildUploadRequest(t, "file", fmt.Sprintf("s%d.mp3", i), "audio/mpeg", body))
		if rr.Code != http.StatusCreated {
			t.Fatalf("upload %d status = %d, want 201 (slot must be released between uploads); body: %s", i, rr.Code, rr.Body.String())
		}
	}
}
