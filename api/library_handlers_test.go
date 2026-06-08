package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/media"
	"github.com/go-chi/chi/v5"
)

// metadataReq builds a PATCH /api/files/{hash}/metadata request with a JSON body
// and the hash as a chi path param, for invoking updateFileMetadata directly.
func metadataReq(t *testing.T, hash, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPatch, "/api/files/"+hash+"/metadata", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return withChiParams(req, map[string]string{"hash": hash})
}

func TestUpdateFileMetadata_NotFound(t *testing.T) {
	h, _, _ := newTestHandler(t)
	rr := httptest.NewRecorder()
	h.updateFileMetadata(rr, metadataReq(t, "deadbeef", `{"title":"x"}`))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", rr.Code, rr.Body.String())
	}
}

func TestUpdateFileMetadata_InvalidJSON(t *testing.T) {
	h, _, _ := newTestHandler(t)
	rr := httptest.NewRecorder()
	h.updateFileMetadata(rr, metadataReq(t, "deadbeef", `{not json`))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rr.Code, rr.Body.String())
	}
}

func TestUpdateFileMetadata_UpdatesAndEchoes(t *testing.T) {
	h, db, _ := newTestHandler(t)
	ctx := context.Background()
	hash := "ab12000000000000000000000000000000000000000000000000000000000000"
	f := &database.File{
		Hash: hash, ByteSize: 1, MimeType: "audio/mpeg",
		StorageBackend: "local", ObjectKey: hash + "/s.mp3", CreatedAt: 1,
	}
	m := &database.MediaMetadata{ExtractedAt: 1}
	m.Title.String, m.Title.Valid = "Old", true
	if err := db.InsertFile(ctx, f, &database.FileUpload{Filename: "s.mp3", UploadedAt: 1}, m); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rr := httptest.NewRecorder()
	h.updateFileMetadata(rr, metadataReq(t, hash, `{"title":"New Title","artist":"New Artist"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	json.NewDecoder(rr.Body).Decode(&resp)
	if resp["title"] != "New Title" || resp["artist"] != "New Artist" {
		t.Errorf("echo = %v, want title=New Title artist=New Artist", resp)
	}
	back, err := db.UpdateFileMetadata(ctx, hash, database.MetadataPatch{})
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if back.Title.String != "New Title" {
		t.Errorf("persisted title = %q, want New Title", back.Title.String)
	}
}

// withChiParams attaches chi URL params to a request so a handler reached via
// chi.URLParam can be invoked directly (without spinning up a router).
func withChiParams(req *http.Request, params map[string]string) *http.Request {
	rctx := chi.NewRouteContext()
	for k, v := range params {
		rctx.URLParams.Add(k, v)
	}
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// albumImageReq builds a POST album-cover request: an "image" multipart part,
// the album as a chi path param, and the artist as a query param.
func albumImageReq(t *testing.T, artist, album, filename, contentType string, body []byte) *http.Request {
	t.Helper()
	req := buildImageRequest(t, filename, contentType, body)
	if artist != "" {
		q := req.URL.Query()
		q.Set("artist", artist)
		req.URL.RawQuery = q.Encode()
	}
	return withChiParams(req, map[string]string{"album": album})
}

// ---- Phase 3: manual album cover upload -------------------------------------

// TestUploadAlbumImage_EnqueuesJob posts a JPEG cover and asserts the original
// lands under <imagesDir>/<base_key>/original.jpg, a single variant job is
// enqueued, the album_images row carries base_key/source_ext, and the response
// reports processing:true.
func TestUploadAlbumImage_EnqueuesJob(t *testing.T) {
	h, db, base := newTestHandler(t)
	img := []byte("\xFF\xD8\xFF\xE0 jpeg cover bytes")

	rr := httptest.NewRecorder()
	h.uploadAlbumImage(rr, albumImageReq(t, "Pink Floyd", "Dark Side", "cover.jpg", "image/jpeg", img))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]any
	json.NewDecoder(rr.Body).Decode(&resp)
	if resp["ok"] != true || resp["processing"] != true {
		t.Errorf("response = %v, want ok:true processing:true", resp)
	}

	baseKey := media.BaseKey(img)
	wantPath := filepath.Join(base, "images", baseKey, "original.jpg")
	if _, err := os.Stat(wantPath); err != nil {
		t.Errorf("original not stored at %s: %v", wantPath, err)
	}

	albumID, ok, err := db.LookupAlbumID(context.Background(), "Pink Floyd", "Dark Side")
	if err != nil || !ok {
		t.Fatalf("LookupAlbumID: found=%v err=%v", ok, err)
	}
	gotKey, gotExt, ready, found, err := db.GetAlbumCoverStatus(context.Background(), albumID)
	if err != nil || !found {
		t.Fatalf("GetAlbumCoverStatus: found=%v err=%v", found, err)
	}
	if gotKey != baseKey || gotExt != ".jpg" {
		t.Errorf("stored cover = (%q,%q), want (%q,.jpg)", gotKey, gotExt, baseKey)
	}
	if ready {
		t.Error("variants_ready = true before the worker ran, want false")
	}
	if got := countPendingImageJobs(t, h); got != 1 {
		t.Errorf("pending image jobs = %d, want 1", got)
	}
}

// TestUploadAlbumImage_OverwritesExisting verifies a manual upload replaces the
// current cover (explicit beats embedded / earlier): the album_images base_key
// is updated to the new image and the new original is written.
func TestUploadAlbumImage_OverwritesExisting(t *testing.T) {
	h, db, base := newTestHandler(t)
	ctx := context.Background()
	imgA := []byte("\xFF\xD8\xFF\xE0 first cover")
	imgB := []byte("\xFF\xD8\xFF\xE0 second different cover")

	first := httptest.NewRecorder()
	h.uploadAlbumImage(first, albumImageReq(t, "Artist", "Album", "a.jpg", "image/jpeg", imgA))
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d; body: %s", first.Code, first.Body.String())
	}

	second := httptest.NewRecorder()
	h.uploadAlbumImage(second, albumImageReq(t, "Artist", "Album", "b.jpg", "image/jpeg", imgB))
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d; body: %s", second.Code, second.Body.String())
	}

	// The cover row must now point at the second image.
	albumID, ok, err := db.LookupAlbumID(ctx, "Artist", "Album")
	if err != nil || !ok {
		t.Fatalf("LookupAlbumID: found=%v err=%v", ok, err)
	}
	gotKey, _, _, found, err := db.GetAlbumCoverStatus(ctx, albumID)
	if err != nil || !found {
		t.Fatalf("GetAlbumCoverStatus: found=%v err=%v", found, err)
	}
	if want := media.BaseKey(imgB); gotKey != want {
		t.Errorf("base_key = %q, want %q (cover should be overwritten)", gotKey, want)
	}
	// Both originals exist on disk (the old one becomes a harmless orphan).
	if _, err := os.Stat(filepath.Join(base, "images", media.BaseKey(imgB), "original.jpg")); err != nil {
		t.Errorf("new original missing: %v", err)
	}
}

// TestUploadAlbumImage_SameImageTwiceIdempotentJob verifies re-uploading the
// identical cover does not double-queue: the per-base_key enqueue is idempotent.
func TestUploadAlbumImage_SameImageTwiceIdempotentJob(t *testing.T) {
	h, _, _ := newTestHandler(t)
	img := []byte("\xFF\xD8\xFF\xE0 same cover bytes")

	for i := 0; i < 2; i++ {
		rr := httptest.NewRecorder()
		h.uploadAlbumImage(rr, albumImageReq(t, "Artist", "Album", "cover.jpg", "image/jpeg", img))
		if rr.Code != http.StatusOK {
			t.Fatalf("upload %d status = %d; body: %s", i, rr.Code, rr.Body.String())
		}
	}
	if got := countPendingImageJobs(t, h); got != 1 {
		t.Errorf("pending image jobs = %d, want 1 (idempotent enqueue per base_key)", got)
	}
}

// TestUploadAlbumImage_RejectsWebP verifies the manual album endpoint rejects
// WebP up front (dropped from the allow-lists in Phase 3).
func TestUploadAlbumImage_RejectsWebP(t *testing.T) {
	h, _, _ := newTestHandler(t)
	rr := httptest.NewRecorder()
	h.uploadAlbumImage(rr, albumImageReq(t, "Artist", "Album", "cover.webp", "image/webp", []byte("RIFF....WEBP")))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for WebP cover", rr.Code)
	}
}

// TestUploadArtistImage_StoresFlatKey verifies the artist cover path still uses
// the flat <base_key><ext> object key (artist variants are deferred) and stores
// it under imagesDir.
func TestUploadArtistImage_StoresFlatKey(t *testing.T) {
	h, db, base := newTestHandler(t)
	img := []byte("\x89PNG\r\n artist image")

	req := buildImageRequest(t, "artist.png", "image/png", img)
	req = withChiParams(req, map[string]string{"artist": "Pink Floyd"})
	rr := httptest.NewRecorder()
	h.uploadArtistImage(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rr.Code, rr.Body.String())
	}

	artistID, ok, err := db.LookupArtistID(context.Background(), "Pink Floyd")
	if err != nil || !ok {
		t.Fatalf("LookupArtistID: found=%v err=%v", ok, err)
	}
	objectKey, mimeType, found, err := db.GetArtistImage(context.Background(), artistID)
	if err != nil || !found {
		t.Fatalf("GetArtistImage: found=%v err=%v", found, err)
	}
	wantKey := media.BaseKey(img) + ".png"
	if objectKey != wantKey {
		t.Errorf("object_key = %q, want flat %q", objectKey, wantKey)
	}
	if mimeType != "image/png" {
		t.Errorf("mime = %q, want image/png", mimeType)
	}
	if _, err := os.Stat(filepath.Join(base, "images", wantKey)); err != nil {
		t.Errorf("artist image not stored at flat path: %v", err)
	}
}

// ---- Phase 5: rename handlers -----------------------------------------------

// renameArtistReq builds a POST /api/artists/{artist}/rename request with the
// current name as a chi param and the new name in the JSON body.
func renameArtistReq(current, newName string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/artists/x/rename",
		strings.NewReader(`{"name":`+jsonString(newName)+`}`))
	return withChiParams(req, map[string]string{"artist": current})
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestRenameArtist_Handler_HappyPath(t *testing.T) {
	h, db, _ := newTestHandler(t)
	ctx := context.Background()
	if _, err := db.ResolveAlbumID(ctx, "Old Name", "Album"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rr := httptest.NewRecorder()
	h.renameArtist(rr, renameArtistReq("Old Name", "New Name"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	if _, found, _ := db.LookupArtistID(ctx, "New Name"); !found {
		t.Error("artist not renamed")
	}
	if _, found, _ := db.LookupArtistID(ctx, "Old Name"); found {
		t.Error("old name still resolves")
	}
}

func TestRenameArtist_Handler_Conflict(t *testing.T) {
	h, db, _ := newTestHandler(t)
	ctx := context.Background()
	if _, err := db.ResolveArtistID(ctx, "Artist A"); err != nil {
		t.Fatalf("seed A: %v", err)
	}
	if _, err := db.ResolveArtistID(ctx, "Artist B"); err != nil {
		t.Fatalf("seed B: %v", err)
	}

	rr := httptest.NewRecorder()
	h.renameArtist(rr, renameArtistReq("Artist A", "Artist B"))
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body: %s", rr.Code, rr.Body.String())
	}
}

func TestRenameArtist_Handler_NotFound(t *testing.T) {
	h, _, _ := newTestHandler(t)
	rr := httptest.NewRecorder()
	h.renameArtist(rr, renameArtistReq("Nobody", "Someone"))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", rr.Code, rr.Body.String())
	}
}

func TestRenameArtist_Handler_EmptyNameRejected(t *testing.T) {
	h, _, _ := newTestHandler(t)
	rr := httptest.NewRecorder()
	h.renameArtist(rr, renameArtistReq("Whoever", "   "))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rr.Code, rr.Body.String())
	}
}

func TestRenameAlbum_Handler_HappyPath(t *testing.T) {
	h, db, _ := newTestHandler(t)
	ctx := context.Background()
	if _, err := db.ResolveAlbumID(ctx, "Pink Floyd", "Old Title"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/albums/Old%20Title/rename?artist=Pink+Floyd",
		strings.NewReader(`{"title":"New Title"}`))
	req = withChiParams(req, map[string]string{"album": "Old Title"})
	rr := httptest.NewRecorder()
	h.renameAlbum(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	if _, found, _ := db.LookupAlbumID(ctx, "Pink Floyd", "New Title"); !found {
		t.Error("album not renamed")
	}
}
