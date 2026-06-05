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

// buildMP3WithCover assembles an ID3v2.3-tagged MP3. Empty album/artist or nil
// pic omit the corresponding frame.
func buildMP3WithCover(album, albumArtist, picMIME string, pic []byte) []byte {
	var frames []byte
	if album != "" {
		frames = append(frames, id3v23TextFrame("TALB", album)...)
	}
	if albumArtist != "" {
		frames = append(frames, id3v23TextFrame("TPE2", albumArtist)...)
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
	has, err := db.HasAlbumCover(context.Background(), "Pink Floyd", "Dark Side")
	if err != nil {
		t.Fatalf("HasAlbumCover: %v", err)
	}
	if !has {
		t.Error("no album_images row created for the uploaded album")
	}

	// Exactly one pending variant job.
	var pending int
	db.QueryRow(`SELECT COUNT(*) FROM image_processing_jobs WHERE status='pending'`).Scan(&pending)
	if pending != 1 {
		t.Errorf("pending image jobs = %d, want 1", pending)
	}

	// The original cover file was written under imagesDir.
	baseKey := media.BaseKey(fakeJPEG)
	want := filepath.Join(base, "images", baseKey, "original.jpg")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("original cover not written at %s: %v", want, err)
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

// TestUploadFile_FillIfMissing_SkipsExisting pre-seeds an album cover, then
// uploads a track with embedded art for the same album. The existing cover must
// not be overwritten and no new job enqueued.
func TestUploadFile_FillIfMissing_SkipsExisting(t *testing.T) {
	h, db, _ := newTestHandler(t)
	ctx := context.Background()

	// Pre-existing (e.g. manually uploaded) cover.
	if err := db.SetAlbumCover(ctx, "Pink Floyd", "Animals", "deadbeefdeadbeef", ".png", "deadbeefdeadbeef/original.png", "image/png", 1); err != nil {
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
	baseKey, _, _, found, err := db.GetAlbumCoverStatus(ctx, "Pink Floyd", "Animals")
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
		storage:       storage.NewLocal(base),
		repo:          db,
		cacheDir:      t.TempDir(),
		imagesDir:     filepath.Join(base, "images"),
		maxUploadSize: testMaxUpload,
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
	var rows int
	db.QueryRow(`SELECT COUNT(*) FROM album_images WHERE album_artist='Pink Floyd' AND album_title='The Wall'`).Scan(&rows)
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
	if has, _ := db.HasAlbumCover(context.Background(), "Pink Floyd", "Meddle"); has {
		t.Error("album cover row created for unsupported embedded format")
	}
	if got := countPendingImageJobs(t, h); got != 0 {
		t.Errorf("pending jobs = %d, want 0", got)
	}
}
