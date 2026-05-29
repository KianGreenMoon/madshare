package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"daemonlord.ygg/madshare/api/storage"
	"daemonlord.ygg/madshare/database"
)

// ---- helpers ----------------------------------------------------------------

// buildUploadRequest creates a multipart POST request with the given filename,
// MIME type, and body content directed at /files/upload.
func buildUploadRequest(t *testing.T, fieldName, filename, contentType string, body []byte) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, fieldName, filename))
	h.Set("Content-Type", contentType)
	fw, err := mw.CreatePart(h)
	if err != nil {
		t.Fatalf("CreatePart: %v", err)
	}
	if _, err := fw.Write(body); err != nil {
		t.Fatalf("Write to form: %v", err)
	}
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/files/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

// testMaxUpload is the upload cap used by test handlers (mirrors the config
// default of 500 MiB).
const testMaxUpload = 500 << 20

// newTestHandler returns a handler wired to a temp-dir Local backend and a
// fresh in-memory DB. The DB is closed automatically when the test ends.
func newTestHandler(t *testing.T) (*handler, *database.DB, string) {
	t.Helper()
	base := t.TempDir()
	db, err := database.Open(":memory:")
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
	return h, db, base
}

// newTestServer wires a router whose file-serving directory matches the store's
// directory, so uploads and the file server read/write the same place.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	srv := httptest.NewServer(NewRouter(storage.NewLocal(dir), db, t.TempDir(), dir, testMaxUpload))
	t.Cleanup(srv.Close)
	return srv
}

// ---- Upload: normal cases ---------------------------------------------------

func TestUploadFile_HappyPath(t *testing.T) {
	h, db, _ := newTestHandler(t)
	data := []byte("hello audio")
	req := buildUploadRequest(t, "file", "song.mp3", "audio/mpeg", data)
	rr := httptest.NewRecorder()

	h.uploadFile(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", rr.Code, http.StatusCreated, rr.Body.String())
	}

	var resp map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["ok"] != true {
		t.Errorf("ok = %v, want true", resp["ok"])
	}
	if resp["existed"] != false {
		t.Errorf("existed = %v, want false", resp["existed"])
	}
	if resp["hash"] == "" || resp["hash"] == nil {
		t.Error("response missing hash")
	}
	// path field should no longer be sent in the success response.
	if _, ok := resp["path"]; ok {
		t.Error("response includes legacy 'path' field; clients should not construct paths")
	}

	// All three DB rows must exist.
	var (
		fileCount, uploadCount, metaCount int
	)
	db.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&fileCount)
	db.QueryRow(`SELECT COUNT(*) FROM file_uploads`).Scan(&uploadCount)
	db.QueryRow(`SELECT COUNT(*) FROM media_metadata`).Scan(&metaCount)
	if fileCount != 1 {
		t.Errorf("files rows = %d, want 1", fileCount)
	}
	if uploadCount != 1 {
		t.Errorf("file_uploads rows = %d, want 1", uploadCount)
	}
	if metaCount != 1 {
		t.Errorf("media_metadata rows = %d, want 1", metaCount)
	}
}

// TestUploadFile_SameHashSameFilename verifies the second upload of identical
// bytes + filename returns 200 existed=true and DOES NOT add a duplicate
// file_uploads row.
func TestUploadFile_SameHashSameFilename(t *testing.T) {
	h, db, _ := newTestHandler(t)
	data := []byte("deduplicate me")

	upload := func() *httptest.ResponseRecorder {
		req := buildUploadRequest(t, "file", "dup.mp3", "audio/mpeg", data)
		rr := httptest.NewRecorder()
		h.uploadFile(rr, req)
		return rr
	}

	first := upload()
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d", first.Code)
	}

	second := upload()
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, want 200; body: %s", second.Code, second.Body.String())
	}
	var resp map[string]any
	json.NewDecoder(second.Body).Decode(&resp)
	if resp["existed"] != true {
		t.Errorf("existed = %v, want true on dedupe", resp["existed"])
	}

	var uploadCount int
	db.QueryRow(`SELECT COUNT(*) FROM file_uploads`).Scan(&uploadCount)
	if uploadCount != 1 {
		t.Errorf("file_uploads rows = %d, want 1 (duplicate filename ignored)", uploadCount)
	}
}

// TestUploadFile_SameHashDifferentFilename verifies that re-uploading the same
// bytes under a new filename returns 200 existed=true and adds a second
// file_uploads row.
func TestUploadFile_SameHashDifferentFilename(t *testing.T) {
	h, db, _ := newTestHandler(t)
	data := []byte("same bytes different name")

	first := httptest.NewRecorder()
	h.uploadFile(first, buildUploadRequest(t, "file", "first.mp3", "audio/mpeg", data))
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d", first.Code)
	}

	second := httptest.NewRecorder()
	h.uploadFile(second, buildUploadRequest(t, "file", "second.mp3", "audio/mpeg", data))
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, want 200", second.Code)
	}
	var resp map[string]any
	json.NewDecoder(second.Body).Decode(&resp)
	if resp["existed"] != true {
		t.Errorf("existed = %v, want true", resp["existed"])
	}

	var uploadCount int
	db.QueryRow(`SELECT COUNT(*) FROM file_uploads`).Scan(&uploadCount)
	if uploadCount != 2 {
		t.Errorf("file_uploads rows = %d, want 2", uploadCount)
	}
}

// ---- Upload: error cases ---------------------------------------------------

func TestUploadFile_MissingFileField(t *testing.T) {
	h, _, _ := newTestHandler(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("irrelevant", "value")
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/files/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()

	h.uploadFile(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestUploadFile_NotMultipart(t *testing.T) {
	h, _, _ := newTestHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/files/upload", strings.NewReader("not multipart"))
	req.Header.Set("Content-Type", "text/plain")
	rr := httptest.NewRecorder()

	h.uploadFile(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

func TestUploadFile_InvalidMIMEType(t *testing.T) {
	h, _, _ := newTestHandler(t)
	req := buildUploadRequest(t, "file", "evil.sh", "application/x-sh", []byte("#!/bin/bash"))
	rr := httptest.NewRecorder()

	h.uploadFile(rr, req)

	if rr.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415", rr.Code)
	}
}

// TestUploadFile_VideoRejected guards the v0 decision to accept audio only.
func TestUploadFile_VideoRejected(t *testing.T) {
	for _, mime := range []string{"video/mp4", "video/webm"} {
		t.Run(mime, func(t *testing.T) {
			h, _, _ := newTestHandler(t)
			req := buildUploadRequest(t, "file", "clip.bin", mime, []byte("fake video"))
			rr := httptest.NewRecorder()
			h.uploadFile(rr, req)
			if rr.Code != http.StatusUnsupportedMediaType {
				t.Errorf("status = %d, want 415 for %s", rr.Code, mime)
			}
		})
	}
}

// TestUploadFile_PathTraversalFilename sends a filename with path separators.
// The stored blob must land inside baseDir.
func TestUploadFile_PathTraversalFilename(t *testing.T) {
	h, _, baseDir := newTestHandler(t)
	data := []byte("traversal payload")
	req := buildUploadRequest(t, "file", "../../../etc/track.mp3", "audio/mpeg", data)
	rr := httptest.NewRecorder()

	h.uploadFile(rr, req)

	if rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", rr.Code, rr.Body.String())
	}
	// The stored file's basename must be the sanitised name under baseDir.
	wantBase := "track.mp3"
	matches, _ := filepath.Glob(filepath.Join(baseDir, "*", wantBase))
	if len(matches) != 1 {
		t.Errorf("expected 1 sanitised %q under baseDir, found %d", wantBase, len(matches))
	}
}

// fakeRepo lets tests force GetFileByHash and InsertFile outcomes.
type fakeRepo struct {
	getResult    *database.File
	getErr       error
	insertErr    error
	recordErr    error
	insertCalls  int
	listFilesErr error

	deleteFilenames []string
	deleteFound     bool
	deleteErr       error
	deleteCalls     int

	fileRefs    []database.FileRef
	fileRefsErr error
}

func (f *fakeRepo) GetFileByHash(_ context.Context, _ string) (*database.File, error) {
	return f.getResult, f.getErr
}

func (f *fakeRepo) InsertFile(_ context.Context, file *database.File, _ *database.FileUpload, _ *database.MediaMetadata) error {
	f.insertCalls++
	if f.insertErr != nil {
		return f.insertErr
	}
	file.ID = 1
	return nil
}

func (f *fakeRepo) RecordUpload(_ context.Context, _ int64, _ string) error {
	return f.recordErr
}

func (f *fakeRepo) ListFiles(_ context.Context) ([]*database.FileListEntry, error) {
	return nil, f.listFilesErr
}

func (f *fakeRepo) ListArtists(_ context.Context) ([]*database.ArtistEntry, error) {
	return nil, nil
}

func (f *fakeRepo) ListAlbumsByArtist(_ context.Context, _ string) ([]*database.AlbumEntry, error) {
	return nil, nil
}

func (f *fakeRepo) ListTracksByAlbumArtist(_ context.Context, _, _ string) ([]*database.TrackEntry, error) {
	return nil, nil
}

func (f *fakeRepo) UpsertArtistImage(_ context.Context, _, _, _ string, _ int64) error {
	return nil
}

func (f *fakeRepo) UpsertAlbumImage(_ context.Context, _, _, _, _ string, _ int64) error {
	return nil
}

func (f *fakeRepo) GetArtistImage(_ context.Context, _ string) (string, string, bool, error) {
	return "", "", false, nil
}

func (f *fakeRepo) GetAlbumImage(_ context.Context, _, _ string) (string, string, bool, error) {
	return "", "", false, nil
}

func (f *fakeRepo) DeleteFileByHash(_ context.Context, _ string) ([]string, bool, error) {
	f.deleteCalls++
	return f.deleteFilenames, f.deleteFound, f.deleteErr
}

func (f *fakeRepo) ListFileRefs(_ context.Context) ([]database.FileRef, error) {
	return f.fileRefs, f.fileRefsErr
}

// TestUploadFile_InsertFailureLeavesOrphan verifies that when storage.Put
// succeeds but repo.InsertFile fails, the handler returns 500 and the blob
// remains on disk (the reconciler removes it later).
func TestUploadFile_InsertFailureLeavesOrphan(t *testing.T) {
	baseDir := t.TempDir()
	repo := &fakeRepo{insertErr: errors.New("simulated db failure")}
	h := &handler{
		storage:       storage.NewLocal(baseDir),
		repo:          repo,
		cacheDir:      t.TempDir(),
		maxUploadSize: testMaxUpload,
	}

	data := []byte("orphan me")
	req := buildUploadRequest(t, "file", "lost.mp3", "audio/mpeg", data)
	rr := httptest.NewRecorder()
	h.uploadFile(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	if repo.insertCalls != 1 {
		t.Errorf("InsertFile called %d times, want 1", repo.insertCalls)
	}

	// The blob must be on disk despite the DB failure.
	matches, _ := filepath.Glob(filepath.Join(baseDir, "*", "lost.mp3"))
	if len(matches) != 1 {
		t.Errorf("expected 1 orphan blob on disk, found %d", len(matches))
	}
}

// TestUploadFile_EmptyBody sends a multipart form with a zero-byte file.
func TestUploadFile_EmptyBody(t *testing.T) {
	h, _, _ := newTestHandler(t)
	req := buildUploadRequest(t, "file", "empty.mp3", "audio/mpeg", []byte{})
	rr := httptest.NewRecorder()

	h.uploadFile(rr, req)

	if rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
		t.Errorf("unexpected status %d for empty upload: %s", rr.Code, rr.Body.String())
	}
}

// CORS on error responses is now covered by TestCORS_OnErrorResponse, which
// exercises the corsMiddleware through the full router.

// TestUploadFile_ExceedsMaxUploadSize verifies the handler rejects a body
// larger than its configured maxUploadSize. The cap comes from config
// (storage.max_upload_mb) and is enforced via http.MaxBytesReader.
//
// uploadFile returns 400 for several distinct reasons (malformed multipart,
// missing field, invalid filename), so checking the status alone would pass
// even if size enforcement regressed. We therefore assert two things: the
// identical request SUCCEEDS under a generous cap (proving the body is
// otherwise valid), and FAILS with the size-specific message under a tiny cap.
func TestUploadFile_ExceedsMaxUploadSize(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 4096)

	// Control: the same well-formed request succeeds when the cap is generous.
	h, _, _ := newTestHandler(t)
	rrOK := httptest.NewRecorder()
	h.uploadFile(rrOK, buildUploadRequest(t, "file", "song.mp3", "audio/mpeg", body))
	if rrOK.Code != http.StatusCreated {
		t.Fatalf("control upload status = %d, want 201; body: %s", rrOK.Code, rrOK.Body.String())
	}

	// Same request, tiny cap: must be rejected specifically for size.
	h2, _, _ := newTestHandler(t)
	h2.maxUploadSize = 64
	rr := httptest.NewRecorder()
	h2.uploadFile(rr, buildUploadRequest(t, "file", "song.mp3", "audio/mpeg", body))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for body exceeding maxUploadSize", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "file too large") {
		t.Errorf("error body = %q, want it to mention the size limit", rr.Body.String())
	}
}

// TestWriteJSON_ContentType ensures writeJSON always sets Content-Type to JSON.
func TestWriteJSON_ContentType(t *testing.T) {
	rr := httptest.NewRecorder()
	writeJSON(rr, http.StatusOK, map[string]string{"key": "value"})

	ct := rr.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

// TestCORS_OnErrorResponse verifies the wildcard CORS header is set even on
// error responses written via http.Error — the corsMiddleware applies to every
// route, so cross-origin JS clients can read error bodies. A bad upload (no
// multipart body) exercises an http.Error path.
func TestCORS_OnErrorResponse(t *testing.T) {
	srv := newTestServer(t)

	resp, err := http.Post(srv.URL+"/files/upload", "text/plain", strings.NewReader("not multipart"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 400 {
		t.Fatalf("expected an error status, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin on error = %q, want *", got)
	}
}

// TestCORS_Preflight verifies an OPTIONS preflight request is answered with the
// CORS headers and no route 405.
func TestCORS_Preflight(t *testing.T) {
	srv := newTestServer(t)

	req, _ := http.NewRequest(http.MethodOptions, srv.URL+"/files/upload", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("preflight status = %d, want 204", resp.StatusCode)
	}
}

// TestNewRouter_Integration verifies NewRouter returns a working http.Handler.
func TestNewRouter_Integration(t *testing.T) {
	srv := newTestServer(t)

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz status = %d, want 200", resp.StatusCode)
	}
}

// ---- listFiles handler ------------------------------------------------------

// TestListFiles_Empty verifies GET /api/files returns an empty JSON array (not
// null) when no files have been uploaded.
func TestListFiles_Empty(t *testing.T) {
	h, _, _ := newTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/api/files", nil)
	rr := httptest.NewRecorder()

	h.listFiles(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	ct := rr.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var items []any
	if err := json.NewDecoder(rr.Body).Decode(&items); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Must be an array (len 0), never null.
	if len(items) != 0 {
		t.Errorf("items = %v, want empty array", items)
	}
}

// TestListFiles_ReturnsUploadedFiles uploads a file then checks that
// GET /api/files returns it with the expected fields.
func TestListFiles_ReturnsUploadedFiles(t *testing.T) {
	h, _, _ := newTestHandler(t)

	// Upload a file first.
	req := buildUploadRequest(t, "file", "track.mp3", "audio/mpeg", []byte("audio content"))
	rr := httptest.NewRecorder()
	h.uploadFile(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("upload status = %d; body: %s", rr.Code, rr.Body.String())
	}

	// Extract the hash from the upload response so we can check it in the list.
	var uploadResp map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&uploadResp); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	wantHash, _ := uploadResp["hash"].(string)

	// Now list.
	listReq := httptest.NewRequest(http.MethodGet, "/api/files", nil)
	listRR := httptest.NewRecorder()
	h.listFiles(listRR, listReq)

	if listRR.Code != http.StatusOK {
		t.Fatalf("list status = %d; body: %s", listRR.Code, listRR.Body.String())
	}

	type fileItem struct {
		ID       int64  `json:"id"`
		Hash     string `json:"hash"`
		Filename string `json:"filename"`
		MimeType string `json:"mime_type"`
		ByteSize int64  `json:"byte_size"`
		URL      string `json:"url"`
		Title    string `json:"title"`
		Artist   string `json:"artist"`
	}
	var items []fileItem
	if err := json.NewDecoder(listRR.Body).Decode(&items); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("len(items) = %d, want 1", len(items))
	}
	got := items[0]
	if got.Hash != wantHash {
		t.Errorf("hash = %q, want %q", got.Hash, wantHash)
	}
	if got.Filename != "track.mp3" {
		t.Errorf("filename = %q, want track.mp3", got.Filename)
	}
	if got.MimeType != "audio/mpeg" {
		t.Errorf("mime_type = %q, want audio/mpeg", got.MimeType)
	}
	if got.ByteSize <= 0 {
		t.Errorf("byte_size = %d, want > 0", got.ByteSize)
	}
	if got.URL == "" {
		t.Error("url field is empty")
	}
	if got.ID <= 0 {
		t.Errorf("id = %d, want > 0", got.ID)
	}
}

// TestListFiles_URLFormatMatchesFileServer verifies that the URL returned by
// listFiles actually matches the path where the file server will find the blob.
// A mismatch here means the download link is broken.
func TestListFiles_URLFormatMatchesFileServer(t *testing.T) {
	h, _, _ := newTestHandler(t)
	data := []byte("url test content")
	req := buildUploadRequest(t, "file", "song.mp3", "audio/mpeg", data)
	rr := httptest.NewRecorder()
	h.uploadFile(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("upload status = %d", rr.Code)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/api/files", nil)
	listRR := httptest.NewRecorder()
	h.listFiles(listRR, listReq)

	var items []map[string]any
	if err := json.NewDecoder(listRR.Body).Decode(&items); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("want 1 item, got %d", len(items))
	}

	url, _ := items[0]["url"].(string)
	hash, _ := items[0]["hash"].(string)
	wantURLPrefix := "/files/" + hash + "/"
	if !strings.HasPrefix(url, wantURLPrefix) {
		t.Errorf("url = %q, want prefix %q; download link is broken", url, wantURLPrefix)
	}
}

// TestListFiles_DBError verifies that a repository error returns 500.
func TestListFiles_DBError(t *testing.T) {
	baseDir := t.TempDir()
	repo := &fakeRepo{} // overriding ListFiles via embedding
	repo.listFilesErr = errors.New("simulated db failure")
	h := &handler{
		storage:       storage.NewLocal(baseDir),
		repo:          repo,
		cacheDir:      t.TempDir(),
		maxUploadSize: testMaxUpload,
	}

	req := httptest.NewRequest(http.MethodGet, "/api/files", nil)
	rr := httptest.NewRecorder()
	h.listFiles(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rr.Code)
	}
}

// TestListFiles_CORSHeader verifies the /api/files response carries the
// wildcard CORS header (applied by corsMiddleware at the router level).
func TestListFiles_CORSHeader(t *testing.T) {
	srv := newTestServer(t)

	resp, err := http.Get(srv.URL + "/api/files")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
	}
}

// ---- Security: MIME bypass via filename extension ---------------------------

// TestUploadFile_DisallowedExtensionRejected verifies that files with dangerous
// extensions are rejected even when Content-Type claims an allowed audio type.
// This guards against stored XSS: without this check an attacker could upload
// evil.html with Content-Type: audio/mpeg and the file server would serve it
// as text/html.
func TestUploadFile_DisallowedExtensionRejected(t *testing.T) {
	cases := []struct {
		filename string
		mime     string
	}{
		{"evil.html", "audio/mpeg"},
		{"evil.js", "audio/mpeg"},
		{"evil.svg", "audio/ogg"},
		{"evil.php", "audio/flac"},
		{"noext", "audio/mpeg"},
	}
	for _, tc := range cases {
		t.Run(tc.filename, func(t *testing.T) {
			h, _, _ := newTestHandler(t)
			req := buildUploadRequest(t, "file", tc.filename, tc.mime, []byte("<script>alert(1)</script>"))
			rr := httptest.NewRecorder()
			h.uploadFile(rr, req)
			if rr.Code != http.StatusUnsupportedMediaType {
				t.Errorf("status = %d, want 415 for %q with %s", rr.Code, tc.filename, tc.mime)
			}
		})
	}
}

// TestUploadFile_ExtensionCaseInsensitive verifies that uppercase or mixed-case
// extensions are accepted. The check uses strings.ToLower so "SONG.MP3" must
// pass the same as "song.mp3".
func TestUploadFile_ExtensionCaseInsensitive(t *testing.T) {
	cases := []string{"SONG.MP3", "track.OGG", "album.FLAC", "clip.WAV", "file.M4A"}
	for _, filename := range cases {
		t.Run(filename, func(t *testing.T) {
			h, _, _ := newTestHandler(t)
			req := buildUploadRequest(t, "file", filename, "audio/mpeg", []byte("audio data"))
			rr := httptest.NewRecorder()
			h.uploadFile(rr, req)
			if rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
				t.Errorf("status = %d, want 2xx for uppercase extension %q", rr.Code, filename)
			}
		})
	}
}

// TestUploadFile_TrailingSpaceInExtensionRejected verifies that a filename with
// a trailing space after the extension (e.g. "song.mp3 ") is rejected.
// filepath.Ext returns ".mp3 " (with the space), which is not in allowedExtensions.
func TestUploadFile_TrailingSpaceInExtensionRejected(t *testing.T) {
	h, _, _ := newTestHandler(t)
	// The space is part of the filename; the multipart parser preserves it.
	req := buildUploadRequest(t, "file", "song.mp3 ", "audio/mpeg", []byte("audio data"))
	rr := httptest.NewRecorder()
	h.uploadFile(rr, req)
	if rr.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415 for filename with trailing space in extension", rr.Code)
	}
}

// TestUploadFile_DoubleExtensionAllowed documents that a double-extension filename
// like "evil.html.mp3" is accepted: filepath.Ext returns the last extension
// (".mp3"), which is in the allowlist. The file server serves it as audio/mpeg
// (not text/html), so stored XSS does not apply.
func TestUploadFile_DoubleExtensionAllowed(t *testing.T) {
	h, _, _ := newTestHandler(t)
	req := buildUploadRequest(t, "file", "evil.html.mp3", "audio/mpeg", []byte("<script>alert(1)</script>"))
	rr := httptest.NewRecorder()
	h.uploadFile(rr, req)
	// Must be accepted — the effective extension is .mp3.
	if rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 2xx for double-extension evil.html.mp3", rr.Code)
	}
}

// Acceptance of parameterized MIME types is now asserted by
// TestUploadFile_MIMEWithParameters.

// TestUploadFile_TraversalFilenameProducesCorrectURL verifies that a filename
// containing path-traversal components ("../../../music/track.mp3") is
// sanitized to its base component ("track.mp3") by the Go standard library's
// mime/multipart parser (which calls filepath.Base on every part filename per
// RFC 7578 §4.2). Both the stored blob path and the download URL are safe.
func TestUploadFile_TraversalFilenameProducesCorrectURL(t *testing.T) {
	h, _, _ := newTestHandler(t)
	data := []byte("traversal url test")
	req := buildUploadRequest(t, "file", "../../../music/track.mp3", "audio/mpeg", data)
	rr := httptest.NewRecorder()
	h.uploadFile(rr, req)

	if rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
		t.Fatalf("upload status = %d: %s", rr.Code, rr.Body.String())
	}

	// Now list files and check the URL.
	listReq := httptest.NewRequest(http.MethodGet, "/api/files", nil)
	listRR := httptest.NewRecorder()
	h.listFiles(listRR, listReq)

	var items []map[string]any
	if err := json.NewDecoder(listRR.Body).Decode(&items); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("want 1 item, got %d", len(items))
	}

	url, _ := items[0]["url"].(string)
	hash, _ := items[0]["hash"].(string)

	// Go's multipart parser applies filepath.Base to every filename (RFC 7578 §4.2),
	// so header.Filename == "track.mp3" (not the raw "../../../music/track.mp3").
	// The ObjectKey and download URL therefore resolve correctly.
	wantURL := "/files/" + hash + "/track.mp3"
	if url != wantURL {
		t.Errorf("url = %q, want %q — Go multipart sanitization may have changed",
			url, wantURL)
	}
}

// ---- Fix: MIME type with parameters -----------------------------------------

// TestUploadFile_MIMEWithParameters verifies a Content-Type carrying parameters
// (e.g. "audio/mpeg; charset=utf-8") is accepted: the handler parses the media
// type with mime.ParseMediaType before checking the allow-list.
func TestUploadFile_MIMEWithParameters(t *testing.T) {
	h, _, _ := newTestHandler(t)
	req := buildUploadRequest(t, "file", "song.mp3", "audio/mpeg; charset=utf-8", []byte("audio data"))
	rr := httptest.NewRecorder()
	h.uploadFile(rr, req)
	if rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 2xx for parameterized MIME type; body: %s", rr.Code, rr.Body.String())
	}
}

// ---- Fix: Windows-path filename sanitization --------------------------------

// TestSanitizeFilename covers Unix and Windows path stripping plus degenerate
// inputs that must collapse to the empty string.
func TestSanitizeFilename(t *testing.T) {
	cases := []struct{ in, want string }{
		{"song.mp3", "song.mp3"},
		{`C:\Users\evil.mp3`, "evil.mp3"},
		{`\\server\share\track.mp3`, "track.mp3"},
		{"../../etc/passwd", "passwd"},
		{"dir/sub/track.flac", "track.flac"},
		{"", ""},
		{".", ""},
		{"..", ""},
		{"/", ""},
	}
	for _, tc := range cases {
		if got := sanitizeFilename(tc.in); got != tc.want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestUploadFile_WindowsPathFilenameProducesCleanURL verifies that a Windows
// absolute-path filename is reduced to its base name in the stored ObjectKey
// and download URL, rather than embedding backslashes (filepath.Base alone
// does not strip backslashes on Linux).
func TestUploadFile_WindowsPathFilenameProducesCleanURL(t *testing.T) {
	h, _, _ := newTestHandler(t)
	req := buildUploadRequest(t, "file", `C:\Users\evil.mp3`, "audio/mpeg", []byte("win path test"))
	rr := httptest.NewRecorder()
	h.uploadFile(rr, req)
	if rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
		t.Fatalf("upload status = %d: %s", rr.Code, rr.Body.String())
	}

	listReq := httptest.NewRequest(http.MethodGet, "/api/files", nil)
	listRR := httptest.NewRecorder()
	h.listFiles(listRR, listReq)

	var items []map[string]any
	if err := json.NewDecoder(listRR.Body).Decode(&items); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("want 1 item, got %d", len(items))
	}
	url, _ := items[0]["url"].(string)
	hash, _ := items[0]["hash"].(string)
	wantURL := "/files/" + hash + "/evil.mp3"
	if url != wantURL {
		t.Errorf("url = %q, want %q (backslashes should be stripped)", url, wantURL)
	}
}

// ---- Fix: directory listing disabled & nosniff header -----------------------

// TestFileServer_NoDirectoryListing verifies that requesting a directory under
// /files/ returns 404 rather than an HTML index of hash dirs and filenames.
func TestFileServer_NoDirectoryListing(t *testing.T) {
	srv := newTestServer(t)

	// Upload a file so a hash directory exists on disk.
	body := buildUploadBody(t, "file", "song.mp3", "audio/mpeg", []byte("dir listing test"))
	upResp, err := http.Post(srv.URL+"/files/upload", body.contentType, body.reader)
	if err != nil {
		t.Fatal(err)
	}
	upResp.Body.Close()

	// The root files directory must not list its contents.
	resp, err := http.Get(srv.URL + "/files/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /files/ status = %d, want 404 (directory listing must be disabled)", resp.StatusCode)
	}
}

// TestFileServer_NosniffHeader verifies the file server sets
// X-Content-Type-Options: nosniff on served files.
func TestFileServer_NosniffHeader(t *testing.T) {
	srv := newTestServer(t)

	body := buildUploadBody(t, "file", "song.mp3", "audio/mpeg", []byte("nosniff test"))
	upResp, err := http.Post(srv.URL+"/files/upload", body.contentType, body.reader)
	if err != nil {
		t.Fatal(err)
	}
	var up map[string]any
	json.NewDecoder(upResp.Body).Decode(&up)
	upResp.Body.Close()
	hash, _ := up["hash"].(string)

	resp, err := http.Get(srv.URL + "/files/" + hash + "/song.mp3")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

// uploadBody bundles a multipart request body with its content type for use
// with http.Post against a test server.
type uploadBody struct {
	reader      *bytes.Reader
	contentType string
}

func buildUploadBody(t *testing.T, fieldName, filename, contentType string, body []byte) uploadBody {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, fieldName, filename))
	h.Set("Content-Type", contentType)
	fw, err := mw.CreatePart(h)
	if err != nil {
		t.Fatalf("CreatePart: %v", err)
	}
	if _, err := fw.Write(body); err != nil {
		t.Fatalf("Write to form: %v", err)
	}
	mw.Close()
	return uploadBody{reader: bytes.NewReader(buf.Bytes()), contentType: mw.FormDataContentType()}
}

// ---- Image upload path: same MIME/ext robustness as audio -------------------

// buildImageRequest builds a multipart POST request with an "image" field.
func buildImageRequest(t *testing.T, filename, contentType string, body []byte) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="image"; filename="%s"`, filename))
	h.Set("Content-Type", contentType)
	fw, err := mw.CreatePart(h)
	if err != nil {
		t.Fatalf("CreatePart: %v", err)
	}
	fw.Write(body)
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/artists/Foo/image", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

// TestSaveImageUpload_MIMEWithParameters verifies a parameterized image
// Content-Type is accepted (mirrors the audio upload fix).
func TestSaveImageUpload_MIMEWithParameters(t *testing.T) {
	h, _, _ := newTestHandler(t)
	req := buildImageRequest(t, "cover.png", "image/png; charset=binary", []byte("img"))
	key, mime, err := h.saveImageUpload(req)
	if err != nil {
		t.Fatalf("saveImageUpload rejected parameterized MIME: %v", err)
	}
	if mime != "image/png" {
		t.Errorf("mime = %q, want image/png", mime)
	}
	if !strings.HasSuffix(key, ".png") {
		t.Errorf("key = %q, want .png suffix", key)
	}
}

// TestSaveImageUpload_UppercaseExtension verifies an uppercase extension is
// accepted and normalized (cameras commonly produce ".JPG").
func TestSaveImageUpload_UppercaseExtension(t *testing.T) {
	h, _, _ := newTestHandler(t)
	req := buildImageRequest(t, "PHOTO.JPG", "image/jpeg", []byte("img"))
	key, mime, err := h.saveImageUpload(req)
	if err != nil {
		t.Fatalf("saveImageUpload rejected uppercase extension: %v", err)
	}
	if mime != "image/jpeg" {
		t.Errorf("mime = %q, want image/jpeg", mime)
	}
	if !strings.HasSuffix(key, ".jpg") {
		t.Errorf("key = %q, want normalized .jpg suffix", key)
	}
}

// TestSaveImageUpload_RejectsBadType verifies a disallowed MIME type is rejected.
// TestSaveImageUpload_StoresUnderImagesDir verifies the saved blob lands in the
// handler's imagesDir (the "images" subdir of files_dir), not a hardcoded path.
func TestSaveImageUpload_StoresUnderImagesDir(t *testing.T) {
	h, _, base := newTestHandler(t)
	req := buildImageRequest(t, "cover.png", "image/png", []byte("img"))
	key, _, err := h.saveImageUpload(req)
	if err != nil {
		t.Fatalf("saveImageUpload: %v", err)
	}
	want := filepath.Join(base, "images", key)
	if _, err := os.Stat(want); err != nil {
		t.Errorf("image not stored under files_dir/images: stat %s: %v", want, err)
	}
}

func TestSaveImageUpload_RejectsBadType(t *testing.T) {
	h, _, _ := newTestHandler(t)
	req := buildImageRequest(t, "evil.svg", "image/svg+xml", []byte("<svg/>"))
	if _, _, err := h.saveImageUpload(req); err == nil {
		t.Error("saveImageUpload accepted disallowed image type")
	}
}

// TestSanitizeFilename_RejectsControlChars verifies NUL/control characters
// collapse to the empty string (clean 400 instead of a later os.Create 500).
func TestSanitizeFilename_RejectsControlChars(t *testing.T) {
	for _, in := range []string{"song\x00.mp3", "a\tb.mp3", "x\x7f.mp3"} {
		if got := sanitizeFilename(in); got != "" {
			t.Errorf("sanitizeFilename(%q) = %q, want \"\"", in, got)
		}
	}
}

// TestSaveImageUpload_DisallowedExtensionRejected verifies that an allowed MIME
// type paired with a disallowed extension is rejected (image analogue of the
// audio MIME-bypass guard).
func TestSaveImageUpload_DisallowedExtensionRejected(t *testing.T) {
	h, _, _ := newTestHandler(t)
	req := buildImageRequest(t, "evil.svg", "image/png", []byte("not really png"))
	if _, _, err := h.saveImageUpload(req); err == nil {
		t.Error("saveImageUpload accepted disallowed extension with allowed MIME type")
	}
}
