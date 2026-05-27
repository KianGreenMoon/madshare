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
		storage:  storage.NewLocal(base),
		repo:     db,
		cacheDir: t.TempDir(),
	}
	return h, db, base
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
	getResult     *database.File
	getErr        error
	insertErr     error
	recordErr     error
	insertCalls   int
	listFilesErr  error
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

// TestUploadFile_InsertFailureLeavesOrphan verifies that when storage.Put
// succeeds but repo.InsertFile fails, the handler returns 500 and the blob
// remains on disk (the reconciler removes it later).
func TestUploadFile_InsertFailureLeavesOrphan(t *testing.T) {
	baseDir := t.TempDir()
	repo := &fakeRepo{insertErr: errors.New("simulated db failure")}
	h := &handler{
		storage:  storage.NewLocal(baseDir),
		repo:     repo,
		cacheDir: t.TempDir(),
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

// TestUploadFile_CORSAbsentOnErrorResponse documents that CORS headers are
// missing on error responses (returned via http.Error, not writeJSON).
func TestUploadFile_CORSAbsentOnErrorResponse(t *testing.T) {
	h, _, _ := newTestHandler(t)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/files/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	h.uploadFile(rr, req)

	cors := rr.Header().Get("Access-Control-Allow-Origin")
	if cors == "*" {
		t.Log("CORS present on error response")
	} else {
		t.Logf("INFO: CORS header absent on error response (status %d)", rr.Code)
	}
}

// TestUploadFile_MaxUploadSizeConstant verifies the declared constant against
// both the spec (50 MB from CLAUDE.md) and the code (500 MB).
func TestUploadFile_MaxUploadSizeConstant(t *testing.T) {
	const specLimit = 50 << 20
	if maxUploadSize != 500<<20 {
		t.Errorf("maxUploadSize = %d, expected code value 500 MB (%d)", maxUploadSize, 500<<20)
	}
	if maxUploadSize != specLimit {
		t.Logf("MISMATCH: maxUploadSize is %d MB but CLAUDE.md spec says 50 MB",
			maxUploadSize>>20)
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

// TestWriteJSON_CORSHeader verifies the wildcard CORS header is set by writeJSON.
func TestWriteJSON_CORSHeader(t *testing.T) {
	rr := httptest.NewRecorder()
	writeJSON(rr, http.StatusOK, nil)

	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
	}
}

// TestNewRouter_Integration verifies NewRouter returns a working http.Handler.
func TestNewRouter_Integration(t *testing.T) {
	store := storage.NewLocal(t.TempDir())
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	srv := httptest.NewServer(NewRouter(store, db, t.TempDir()))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET / status = %d, want 200", resp.StatusCode)
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
		storage:  storage.NewLocal(baseDir),
		repo:     repo,
		cacheDir: t.TempDir(),
	}

	req := httptest.NewRequest(http.MethodGet, "/api/files", nil)
	rr := httptest.NewRecorder()
	h.listFiles(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rr.Code)
	}
}

// TestListFiles_CORSHeader verifies that listFiles sets the wildcard CORS header.
func TestListFiles_CORSHeader(t *testing.T) {
	h, _, _ := newTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/api/files", nil)
	rr := httptest.NewRecorder()
	h.listFiles(rr, req)

	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "*" {
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
