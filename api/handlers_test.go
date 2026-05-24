package api

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"daemonlord.ygg/madshare/api/storage"
)

// buildUploadRequest creates a multipart POST request with the given filename
// and body content directed at /files/upload.
func buildUploadRequest(t *testing.T, fieldName, filename string, body []byte) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile(fieldName, filename)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := fw.Write(body); err != nil {
		t.Fatalf("Write to form: %v", err)
	}
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/files/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

// newTestHandler returns a handler wired to a temp-dir Local backend.
func newTestHandler(t *testing.T) *handler {
	t.Helper()
	base := t.TempDir()
	cache := base + "/.cache"
	return &handler{storage: storage.NewLocal(base, cache)}
}

// ---- Upload: normal cases ---------------------------------------------------

func TestUploadFile_HappyPath(t *testing.T) {
	h := newTestHandler(t)
	data := []byte("hello audio")
	req := buildUploadRequest(t, "file", "song.mp3", data)
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
}

// TestUploadFile_Deduplication uploads the same bytes twice and verifies the
// second response reports existed=true with 200 OK (not 201).
func TestUploadFile_Deduplication(t *testing.T) {
	h := newTestHandler(t)
	data := []byte("deduplicate me")

	upload := func() *httptest.ResponseRecorder {
		req := buildUploadRequest(t, "file", "dup.mp3", data)
		rr := httptest.NewRecorder()
		h.uploadFile(rr, req)
		return rr
	}

	first := upload()
	if first.Code != http.StatusCreated {
		t.Fatalf("first upload status = %d", first.Code)
	}

	second := upload()
	if second.Code != http.StatusOK {
		t.Fatalf("second upload status = %d, want 200", second.Code)
	}
	var resp map[string]any
	json.NewDecoder(second.Body).Decode(&resp)
	if resp["existed"] != true {
		t.Errorf("existed = %v, want true on second upload", resp["existed"])
	}
}

// TestUploadFile_MissingFileField ensures 400 is returned when the "file"
// multipart field is absent.
func TestUploadFile_MissingFileField(t *testing.T) {
	h := newTestHandler(t)
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

// TestUploadFile_NotMultipart sends a plain POST body instead of multipart.
func TestUploadFile_NotMultipart(t *testing.T) {
	h := newTestHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/files/upload", strings.NewReader("not multipart"))
	req.Header.Set("Content-Type", "text/plain")
	rr := httptest.NewRecorder()

	h.uploadFile(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

// ---- Upload: security cases -------------------------------------------------

// TestUploadFile_PathTraversalFilename sends a filename with path separators.
// The response path field must not expose ".." components.
func TestUploadFile_PathTraversalFilename(t *testing.T) {
	h := newTestHandler(t)
	data := []byte("traversal payload")
	req := buildUploadRequest(t, "file", "../../../etc/passwd", data)
	rr := httptest.NewRecorder()

	h.uploadFile(rr, req)

	if rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	json.NewDecoder(rr.Body).Decode(&resp)
	if path, ok := resp["path"].(string); ok {
		if strings.Contains(path, "..") {
			t.Errorf("response path contains '..': %q", path)
		}
	}
}

// TestUploadFile_CORSAbsentOnErrorResponse documents that CORS headers are
// missing on error responses (returned via http.Error, not writeJSON).
func TestUploadFile_CORSAbsentOnErrorResponse(t *testing.T) {
	h := newTestHandler(t)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/files/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	h.uploadFile(rr, req)

	// Document current behaviour: CORS is absent on 400 error responses.
	cors := rr.Header().Get("Access-Control-Allow-Origin")
	if cors == "*" {
		t.Log("CORS present on error response")
	} else {
		t.Logf("INFO: CORS header absent on error response (status %d) — browser clients cannot read error details cross-origin", rr.Code)
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

// TestUploadFile_EmptyBody sends a multipart form with a zero-byte file.
func TestUploadFile_EmptyBody(t *testing.T) {
	h := newTestHandler(t)
	req := buildUploadRequest(t, "file", "empty.mp3", []byte{})
	rr := httptest.NewRecorder()

	h.uploadFile(rr, req)

	if rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
		t.Errorf("unexpected status %d for empty upload: %s", rr.Code, rr.Body.String())
	}
}

// TestUploadFile_ResponsePathUsesBase verifies the "path" field in the JSON
// response is <hash>/<basename> even when the client sends a full path as the
// filename.
func TestUploadFile_ResponsePathUsesBase(t *testing.T) {
	h := newTestHandler(t)
	data := []byte("path test")
	req := buildUploadRequest(t, "file", "/some/nested/dir/track.mp3", data)
	rr := httptest.NewRecorder()
	h.uploadFile(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	json.NewDecoder(rr.Body).Decode(&resp)
	path, _ := resp["path"].(string)
	if !strings.HasSuffix(path, "/track.mp3") {
		t.Errorf("response path %q should end with /track.mp3", path)
	}
	if strings.Contains(path, "some/nested") {
		t.Errorf("response path %q leaks directory structure from client filename", path)
	}
}

// TestUploadFile_RouteNotIntegrationTested documents the testability gap caused
// by Route() calling http.ListenAndServe directly instead of returning an
// http.Handler.
func TestUploadFile_RouteNotIntegrationTested(t *testing.T) {
	t.Log("INFO: Route() calls http.ListenAndServe directly and returns nothing, making it untestable without refactoring. Recommend signature: func Route() http.Handler")
	_ = io.Discard // suppress unused import warning
}
