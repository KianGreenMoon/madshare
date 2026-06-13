package api

import (
	"bytes"
	"context"
	"database/sql"
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

// TestUploadFile_RestoresSoftDeletedFile verifies that re-uploading a file
// whose hash is in the trash restores it to the library instead of being
// treated as a new upload. The response is 200 existed=true, deleted_at is
// cleared, and the file appears in ListFiles.
func TestUploadFile_RestoresSoftDeletedFile(t *testing.T) {
	h, db, _ := newTestHandler(t)
	data := []byte("soft deleted audio")

	// Initial upload.
	first := httptest.NewRecorder()
	h.uploadFile(first, buildUploadRequest(t, "file", "track.mp3", "audio/mpeg", data))
	if first.Code != http.StatusCreated {
		t.Fatalf("first upload status = %d", first.Code)
	}
	var firstResp map[string]any
	json.NewDecoder(first.Body).Decode(&firstResp)
	hash := firstResp["hash"].(string)

	// Soft-delete the file directly in the DB (simulates admin "Move to Trash").
	if _, _, err := db.SoftDeleteFileByHash(context.Background(), hash); err != nil {
		t.Fatalf("SoftDeleteFileByHash: %v", err)
	}

	// Re-upload the same bytes; must restore, not error.
	second := httptest.NewRecorder()
	h.uploadFile(second, buildUploadRequest(t, "file", "track.mp3", "audio/mpeg", data))
	if second.Code != http.StatusOK {
		t.Fatalf("re-upload status = %d, want 200; body: %s", second.Code, second.Body.String())
	}
	var resp map[string]any
	json.NewDecoder(second.Body).Decode(&resp)
	if resp["existed"] != true {
		t.Errorf("existed = %v, want true", resp["existed"])
	}

	// File must be live (deleted_at cleared).
	got, _ := db.GetFileByHash(context.Background(), hash)
	if got == nil {
		t.Fatal("file row gone after restore-via-reupload")
	}
	if got.DeletedAt.Valid {
		t.Errorf("DeletedAt still set after restore-via-reupload: %d", got.DeletedAt.Int64)
	}

	// File must appear in ListFiles.
	entries, _ := db.ListFiles(context.Background())
	var found bool
	for _, e := range entries {
		if e.Hash == hash {
			found = true
		}
	}
	if !found {
		t.Error("restored file not present in ListFiles after restore-via-reupload")
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

// TestUploadFile_ExtensionAuthoritative verifies the gate keys on the filename
// extension and persists the canonical MIME, so allowed audio whose declared
// part Content-Type is empty or application/octet-stream (common for FLAC/M4A/
// OPUS in browsers, and curl -F's default) is accepted, not 415'd, and stored
// with the right MIME. See docs/api/upload.md (Accepted types).
func TestUploadFile_ExtensionAuthoritative(t *testing.T) {
	cases := []struct {
		filename    string
		contentType string
		wantMIME    string
	}{
		{"track.flac", "", "audio/flac"},
		{"track.m4a", "application/octet-stream", "audio/mp4"},
		{"track.opus", "application/octet-stream", "audio/opus"},
		{"track.aac", "", "audio/aac"},
	}
	for _, tc := range cases {
		t.Run(tc.filename, func(t *testing.T) {
			h, db, _ := newTestHandler(t)
			req := buildUploadRequest(t, "file", tc.filename, tc.contentType, []byte("fake "+tc.filename))
			rr := httptest.NewRecorder()
			h.uploadFile(rr, req)
			if rr.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201; body: %s", rr.Code, rr.Body.String())
			}
			var got string
			if err := db.QueryRow(`SELECT mime_type FROM files`).Scan(&got); err != nil {
				t.Fatalf("read mime_type: %v", err)
			}
			if got != tc.wantMIME {
				t.Errorf("stored mime_type = %q, want %q", got, tc.wantMIME)
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

	libraryBytes    int64
	libraryBytesErr error

	deleteFilenames []string
	deleteFound     bool
	deleteErr       error
	deleteCalls     int

	fileRefs    []database.FileRef
	fileRefsErr error

	auditCalls   int
	lastAudit    string // "action|target"
	lastUploaded sql.NullInt64

	searchResult *database.SearchResults
	searchErr    error

	// Cover-variant queue stubs (Phase 1). Drive GetAlbumCoverStatus / counters.
	coverBaseKey   string
	coverExt       string
	coverReady     bool
	coverFound     bool
	coverClaimLost bool // SetAlbumCoverIfAbsent reports the row already existed
	enqueueCalls   int
	setCoverCalls  int

	// Base-metadata edit stubs (Phase 5).
	metaCalls     int
	lastMetaHash  string
	lastMetaPatch database.MetadataPatch
	metaResult    *database.MediaMetadata
	metaErr       error

	// Playlist stubs (UI roadmap Phase 5). playlistErr is returned by every
	// playlist method, so each error branch is one knob away.
	playlistErr       error
	playlistList      []*database.Playlist
	playlistGet       *database.Playlist
	playlistItems     []*database.PlaylistItemEntry
	playlistItemFound bool
	favoriteLiked     bool
	favoriteHashes    []string

	// Review-bucket stubs (moderation review). lastReviewState captures the
	// state InsertFile received; reviewInfoFound=false means "unknown hash"
	// (the blob gate passes unknowns through to the file server).
	lastReviewState   string
	reviewEntries     []*database.ReviewEntry
	reviewErr         error
	reviewUpdateFound bool
	reviewUpdateErr   error
	reviewUpdateCalls int
	lastReviewHash    string
	lastReviewTrans   database.ReviewTransition
	reviewInfoState   string
	reviewInfoOwner   sql.NullInt64
	reviewInfoDeleted bool
	reviewInfoFound   bool
	reviewInfoErr     error
	stageRestored     bool
	stageRestoredErr  error
	stageRestoreCalls int
}

func (f *fakeRepo) ListUploadsByUser(_ context.Context, _ int64) ([]*database.ReviewEntry, error) {
	return f.reviewEntries, f.reviewErr
}

func (f *fakeRepo) ListPendingReview(_ context.Context) ([]*database.ReviewEntry, error) {
	return f.reviewEntries, f.reviewErr
}

func (f *fakeRepo) UpdateReviewState(_ context.Context, hash string, t database.ReviewTransition) (bool, error) {
	f.reviewUpdateCalls++
	f.lastReviewHash = hash
	f.lastReviewTrans = t
	return f.reviewUpdateFound, f.reviewUpdateErr
}

func (f *fakeRepo) FileReviewInfo(_ context.Context, _ string) (string, sql.NullInt64, bool, bool, error) {
	return f.reviewInfoState, f.reviewInfoOwner, f.reviewInfoDeleted, f.reviewInfoFound, f.reviewInfoErr
}

func (f *fakeRepo) StageRestoredFile(_ context.Context, _ string, _ sql.NullInt64) (bool, error) {
	f.stageRestoreCalls++
	return f.stageRestored, f.stageRestoredErr
}

func (f *fakeRepo) DiscardOwnUpload(_ context.Context, _ string, _ int64) (bool, error) {
	return f.reviewUpdateFound, f.reviewUpdateErr
}

func (f *fakeRepo) RecordAudit(_ context.Context, actor sql.NullInt64, action, target, _ string) error {
	f.auditCalls++
	f.lastAudit = action + "|" + target
	return nil
}

func (f *fakeRepo) FileAccessibleByHash(_ context.Context, _ string) (bool, error) {
	return true, nil
}

func (f *fakeRepo) GetFileByHash(_ context.Context, _ string) (*database.File, error) {
	return f.getResult, f.getErr
}

func (f *fakeRepo) InsertFile(_ context.Context, file *database.File, _ *database.FileUpload, _ *database.MediaMetadata) error {
	f.insertCalls++
	if f.insertErr != nil {
		return f.insertErr
	}
	f.lastUploaded = file.UploadedBy
	f.lastReviewState = file.ReviewState
	file.ID = 1
	return nil
}

func (f *fakeRepo) RecordUpload(_ context.Context, _ int64, _ string) error {
	return f.recordErr
}

func (f *fakeRepo) ListFiles(_ context.Context) ([]*database.FileListEntry, error) {
	return nil, f.listFilesErr
}

func (f *fakeRepo) LibraryByteSize(_ context.Context) (int64, error) {
	return f.libraryBytes, f.libraryBytesErr
}

func (f *fakeRepo) ListArtists(_ context.Context) ([]*database.ArtistEntry, error) {
	return nil, nil
}

func (f *fakeRepo) ListAlbumsByArtistID(_ context.Context, _ int64) ([]*database.AlbumEntry, error) {
	return nil, nil
}

func (f *fakeRepo) ListTracksByAlbumID(_ context.Context, _ int64) ([]*database.TrackEntry, error) {
	return nil, nil
}

func (f *fakeRepo) ListFilesGuest(_ context.Context) ([]*database.FileListEntry, error) {
	return nil, f.listFilesErr
}

func (f *fakeRepo) ListArtistsGuest(_ context.Context) ([]*database.ArtistEntry, error) {
	return nil, nil
}

func (f *fakeRepo) ListAlbumsByArtistIDGuest(_ context.Context, _ int64) ([]*database.AlbumEntry, error) {
	return nil, nil
}

func (f *fakeRepo) ListTracksByAlbumIDGuest(_ context.Context, _ int64) ([]*database.TrackEntry, error) {
	return nil, nil
}

func (f *fakeRepo) UpsertArtistImage(_ context.Context, _ int64, _, _ string, _ int64) error {
	return nil
}

func (f *fakeRepo) UpsertAlbumImage(_ context.Context, _ int64, _, _ string, _ int64) error {
	return nil
}

func (f *fakeRepo) GetArtistImage(_ context.Context, _ int64) (string, string, bool, error) {
	return "", "", false, nil
}

func (f *fakeRepo) GetAlbumImage(_ context.Context, _ int64) (string, string, bool, error) {
	return "", "", false, nil
}

// Lookups resolve transparently (always found) so the Get*/cover fakes below
// remain the single knob for presence — the handler's not-found branch is
// exercised separately via those.
func (f *fakeRepo) LookupArtistID(_ context.Context, _ string) (int64, bool, error) {
	return 1, true, nil
}

func (f *fakeRepo) LookupAlbumID(_ context.Context, _, _ string) (int64, bool, error) {
	return 1, true, nil
}

func (f *fakeRepo) ResolveArtistID(_ context.Context, _ string) (int64, error) {
	return 1, nil
}

func (f *fakeRepo) ResolveAlbumID(_ context.Context, _, _ string) (int64, error) {
	return 1, nil
}

func (f *fakeRepo) RenameArtist(_ context.Context, _ int64, _ string) error {
	return nil
}

func (f *fakeRepo) RenameAlbum(_ context.Context, _ int64, _ string) error {
	return nil
}

func (f *fakeRepo) MergeArtists(_ context.Context, _, _ int64) error {
	return nil
}

func (f *fakeRepo) MergeAlbums(_ context.Context, _, _ int64) error {
	return nil
}

func (f *fakeRepo) MergeArtistsPreview(_ context.Context, fromID, intoID int64) (*database.MergePreview, error) {
	return &database.MergePreview{FromID: fromID, IntoID: intoID}, nil
}

func (f *fakeRepo) MergeAlbumsPreview(_ context.Context, fromID, intoID int64) (*database.MergePreview, error) {
	return &database.MergePreview{FromID: fromID, IntoID: intoID}, nil
}

func (f *fakeRepo) EnqueueImageJob(_ context.Context, _, _, _ string, _ int64) error {
	f.enqueueCalls++
	return nil
}

func (f *fakeRepo) ClaimImageJob(_ context.Context) (*database.ImageJob, error) {
	return nil, nil
}

func (f *fakeRepo) FinishImageJob(_ context.Context, _ int64, _ error) error {
	return nil
}

func (f *fakeRepo) ResetStaleJobs(_ context.Context) error {
	return nil
}

func (f *fakeRepo) SetAlbumCover(_ context.Context, _ int64, _, _, _, _ string, _ int64) error {
	f.setCoverCalls++
	return nil
}

func (f *fakeRepo) SetAlbumCoverIfAbsent(_ context.Context, _ int64, _, _, _, _ string, _ int64) (bool, error) {
	f.setCoverCalls++
	// Default: the claim succeeds (no existing cover). Tests exercising the
	// "lost the race / cover already present" path set coverClaimLost.
	return !f.coverClaimLost, nil
}

func (f *fakeRepo) GetAlbumCoverStatus(_ context.Context, _ int64) (string, string, bool, bool, error) {
	return f.coverBaseKey, f.coverExt, f.coverReady, f.coverFound, nil
}

func (f *fakeRepo) HasAlbumCover(_ context.Context, _ int64) (bool, error) {
	return f.coverFound, nil
}

func (f *fakeRepo) UpdateFileMetadata(_ context.Context, hash string, p database.MetadataPatch) (*database.MediaMetadata, error) {
	f.metaCalls++
	f.lastMetaHash = hash
	f.lastMetaPatch = p
	if f.metaErr != nil {
		return nil, f.metaErr
	}
	if f.metaResult != nil {
		return f.metaResult, nil
	}
	return &database.MediaMetadata{}, nil
}

func (f *fakeRepo) SoftDeleteFileByHash(_ context.Context, _ string) ([]string, bool, error) {
	f.deleteCalls++
	return f.deleteFilenames, f.deleteFound, f.deleteErr
}

func (f *fakeRepo) HardDeleteFileByHash(_ context.Context, _ string) ([]string, bool, error) {
	return f.deleteFilenames, f.deleteFound, f.deleteErr
}

func (f *fakeRepo) HardDeleteTrashedFileByHash(_ context.Context, _ string) ([]string, bool, error) {
	return f.deleteFilenames, f.deleteFound, f.deleteErr
}

func (f *fakeRepo) RestoreFileByHash(_ context.Context, _ string) (bool, error) {
	return true, nil
}

func (f *fakeRepo) GetTrashRestorePolicy(_ context.Context) (string, error) {
	return database.TrashReuploadRestores, nil
}

func (f *fakeRepo) ListTrashedFiles(_ context.Context) ([]*database.FileListEntry, error) {
	return nil, nil
}

func (f *fakeRepo) ListFileRefs(_ context.Context) ([]database.FileRef, error) {
	return f.fileRefs, f.fileRefsErr
}

func (f *fakeRepo) Search(_ context.Context, _ string) (*database.SearchResults, error) {
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	if f.searchResult != nil {
		return f.searchResult, nil
	}
	return &database.SearchResults{}, nil
}

func (f *fakeRepo) SearchGuest(_ context.Context, _ string) (*database.SearchResults, error) {
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	if f.searchResult != nil {
		return f.searchResult, nil
	}
	return &database.SearchResults{}, nil
}

// Playlist stubs (Phase 5). Handler-level playlist behavior is exercised in
// playlists_test.go via the playlist* knobs; the real logic is tested against
// SQLite in database/playlists_test.go.
func (f *fakeRepo) ListPlaylists(_ context.Context, _ int64) ([]*database.Playlist, error) {
	return f.playlistList, f.playlistErr
}

func (f *fakeRepo) EnsureFavoritesPlaylist(_ context.Context, _ int64) (int64, error) {
	return 1, f.playlistErr
}

func (f *fakeRepo) CreatePlaylist(_ context.Context, userID int64, name string, hashes []string) (*database.Playlist, error) {
	if f.playlistErr != nil {
		return nil, f.playlistErr
	}
	return &database.Playlist{ID: 2, UserID: userID, Name: name, Kind: database.PlaylistRegular, TrackCount: len(hashes)}, nil
}

func (f *fakeRepo) GetPlaylist(_ context.Context, _, _ int64) (*database.Playlist, []*database.PlaylistItemEntry, error) {
	if f.playlistErr != nil {
		return nil, nil, f.playlistErr
	}
	return f.playlistGet, f.playlistItems, nil
}

func (f *fakeRepo) RenamePlaylist(_ context.Context, _, _ int64, _ string) error {
	return f.playlistErr
}

func (f *fakeRepo) DeletePlaylist(_ context.Context, _, _ int64) error {
	return f.playlistErr
}

func (f *fakeRepo) AddPlaylistItemsByHash(_ context.Context, _, _ int64, hashes []string) (int, error) {
	if f.playlistErr != nil {
		return 0, f.playlistErr
	}
	return len(hashes), nil
}

func (f *fakeRepo) RemovePlaylistItem(_ context.Context, _, _, _ int64) (bool, error) {
	return f.playlistItemFound, f.playlistErr
}

func (f *fakeRepo) ReorderPlaylist(_ context.Context, _, _ int64, _ []int64) error {
	return f.playlistErr
}

func (f *fakeRepo) ToggleFavorite(_ context.Context, _ int64, _ string) (bool, error) {
	return f.favoriteLiked, f.playlistErr
}

func (f *fakeRepo) ListFavoriteHashes(_ context.Context, _ int64) ([]string, error) {
	return f.favoriteHashes, f.playlistErr
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

// TestReadImageUpload_MIMEWithParameters verifies a parameterized image
// Content-Type is accepted (mirrors the audio upload fix).
func TestReadImageUpload_MIMEWithParameters(t *testing.T) {
	h, _, _ := newTestHandler(t)
	req := buildImageRequest(t, "cover.png", "image/png; charset=binary", []byte("img"))
	_, mime, ext, err := h.readImageUpload(req)
	if err != nil {
		t.Fatalf("readImageUpload rejected parameterized MIME: %v", err)
	}
	if mime != "image/png" {
		t.Errorf("mime = %q, want image/png", mime)
	}
	if ext != ".png" {
		t.Errorf("ext = %q, want .png", ext)
	}
}

// TestReadImageUpload_CanonicalizesExtension verifies an uppercase or alternate
// extension (.JPG / .jpeg) is normalized to the canonical .jpg, since the worker
// and status API assume original.jpg / original.png.
func TestReadImageUpload_CanonicalizesExtension(t *testing.T) {
	for _, name := range []string{"PHOTO.JPG", "cover.jpeg"} {
		t.Run(name, func(t *testing.T) {
			h, _, _ := newTestHandler(t)
			req := buildImageRequest(t, name, "image/jpeg", []byte("img"))
			_, mime, ext, err := h.readImageUpload(req)
			if err != nil {
				t.Fatalf("readImageUpload rejected %q: %v", name, err)
			}
			if mime != "image/jpeg" {
				t.Errorf("mime = %q, want image/jpeg", mime)
			}
			if ext != ".jpg" {
				t.Errorf("ext = %q, want canonical .jpg", ext)
			}
		})
	}
}

func TestReadImageUpload_RejectsBadType(t *testing.T) {
	h, _, _ := newTestHandler(t)
	req := buildImageRequest(t, "evil.svg", "image/svg+xml", []byte("<svg/>"))
	if _, _, _, err := h.readImageUpload(req); err == nil {
		t.Error("readImageUpload accepted disallowed image type")
	}
}

// TestReadImageUpload_RejectsWebP verifies WebP is rejected up front (dropped
// from the allow-lists in Phase 3 because the variant worker cannot decode it).
func TestReadImageUpload_RejectsWebP(t *testing.T) {
	h, _, _ := newTestHandler(t)
	req := buildImageRequest(t, "cover.webp", "image/webp", []byte("RIFF....WEBP"))
	if _, _, _, err := h.readImageUpload(req); err == nil {
		t.Error("readImageUpload accepted WebP; it must be rejected")
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

// TestReadImageUpload_DisallowedExtensionRejected verifies that an allowed MIME
// type paired with a disallowed extension is rejected (image analogue of the
// audio MIME-bypass guard).
func TestReadImageUpload_DisallowedExtensionRejected(t *testing.T) {
	h, _, _ := newTestHandler(t)
	req := buildImageRequest(t, "evil.svg", "image/png", []byte("not really png"))
	if _, _, _, err := h.readImageUpload(req); err == nil {
		t.Error("readImageUpload accepted disallowed extension with allowed MIME type")
	}
}

// ---- search handler ---------------------------------------------------------

// newSearchHandler returns a handler wired to a fakeRepo configured for search
// tests, with authzEnabled=false so h.accessFilter always takes the unfiltered
// path (calls repo.Search, not repo.SearchGuest).
func newSearchHandler(repo *fakeRepo) *handler {
	return &handler{
		storage:       storage.NewLocal(os.TempDir()),
		repo:          repo,
		cacheDir:      os.TempDir(),
		maxUploadSize: testMaxUpload,
		authzEnabled:  false, // no auth → Search (unfiltered) path
	}
}

// TestSearch_EmptyQ_Returns200WithEmptyArrays exercises the handler's handling
// of an absent or empty "q" param — the DB layer short-circuits to empty
// results and the handler must encode three empty JSON arrays, never null.
func TestSearch_EmptyQ_Returns200WithEmptyArrays(t *testing.T) {
	repo := &fakeRepo{}
	h := newSearchHandler(repo)

	for _, url := range []string{"/api/search", "/api/search?q=", "/api/search?q=%20"} {
		t.Run(url, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, url, nil)
			rr := httptest.NewRecorder()
			h.search(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
			}

			var resp struct {
				Artists []any `json:"artists"`
				Albums  []any `json:"albums"`
				Tracks  []any `json:"tracks"`
			}
			if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			// All three arrays must be present and empty (not null).
			if resp.Artists == nil {
				t.Error("artists is null, want []")
			}
			if resp.Albums == nil {
				t.Error("albums is null, want []")
			}
			if resp.Tracks == nil {
				t.Error("tracks is null, want []")
			}
		})
	}
}

// TestSearch_ValidQ_Returns200WithResults verifies the JSON shape returned for a
// non-empty search result: artists/albums/tracks arrays with the expected fields.
func TestSearch_ValidQ_Returns200WithResults(t *testing.T) {
	year := int64(1969)
	repo := &fakeRepo{
		searchResult: &database.SearchResults{
			Artists: []*database.ArtistEntry{
				{Name: "The Beatles", TrackCount: 3, HasImage: false},
			},
			Albums: []*database.AlbumEntry{
				{Title: "Abbey Road", ArtistName: "The Beatles", TrackCount: 3},
			},
			Tracks: []*database.SearchTrackEntry{
				{
					ID:         1,
					Title:      "Come Together",
					ObjectKey:  "abc123/come_together.mp3",
					MimeType:   "audio/mpeg",
					ArtistName: "The Beatles",
					AlbumTitle: "Abbey Road",
				},
			},
		},
	}
	_ = year // silence unused-var lint; year used via AlbumEntry.Year below
	h := newSearchHandler(repo)

	req := httptest.NewRequest(http.MethodGet, "/api/search?q=Beatles", nil)
	rr := httptest.NewRecorder()
	h.search(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var resp struct {
		Artists []struct {
			Name       string `json:"name"`
			TrackCount int    `json:"track_count"`
			HasImage   bool   `json:"has_image"`
		} `json:"artists"`
		Albums []struct {
			Title      string `json:"title"`
			ArtistName string `json:"artist_name"`
			TrackCount int    `json:"track_count"`
		} `json:"albums"`
		Tracks []struct {
			ID         int64  `json:"id"`
			Title      string `json:"title"`
			URL        string `json:"url"`
			MimeType   string `json:"mime_type"`
			ArtistName string `json:"artist_name"`
			AlbumTitle string `json:"album_title"`
		} `json:"tracks"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(resp.Artists) != 1 {
		t.Fatalf("artists = %d, want 1", len(resp.Artists))
	}
	if resp.Artists[0].Name != "The Beatles" {
		t.Errorf("artists[0].name = %q, want The Beatles", resp.Artists[0].Name)
	}

	if len(resp.Albums) != 1 {
		t.Fatalf("albums = %d, want 1", len(resp.Albums))
	}
	if resp.Albums[0].Title != "Abbey Road" {
		t.Errorf("albums[0].title = %q, want Abbey Road", resp.Albums[0].Title)
	}

	if len(resp.Tracks) != 1 {
		t.Fatalf("tracks = %d, want 1", len(resp.Tracks))
	}
	tr := resp.Tracks[0]
	if tr.Title != "Come Together" {
		t.Errorf("tracks[0].title = %q, want Come Together", tr.Title)
	}
	if tr.URL != "/files/abc123/come_together.mp3" {
		t.Errorf("tracks[0].url = %q, want /files/abc123/come_together.mp3", tr.URL)
	}
	if tr.ArtistName != "The Beatles" {
		t.Errorf("tracks[0].artist_name = %q, want The Beatles", tr.ArtistName)
	}
	if tr.AlbumTitle != "Abbey Road" {
		t.Errorf("tracks[0].album_title = %q, want Abbey Road", tr.AlbumTitle)
	}
}

// TestSearch_QueryTooLong_Returns400 verifies the handler rejects queries longer
// than 200 characters with HTTP 400 "query too long".
func TestSearch_QueryTooLong_Returns400(t *testing.T) {
	repo := &fakeRepo{}
	h := newSearchHandler(repo)

	// Exactly 200 chars: must succeed (boundary — allowed).
	q200 := strings.Repeat("a", 200)
	reqOK := httptest.NewRequest(http.MethodGet, "/api/search?q="+q200, nil)
	rrOK := httptest.NewRecorder()
	h.search(rrOK, reqOK)
	if rrOK.Code != http.StatusOK {
		t.Errorf("q=200 chars: status = %d, want 200 (boundary should be allowed)", rrOK.Code)
	}

	// 201 chars: must be rejected.
	q201 := strings.Repeat("a", 201)
	req := httptest.NewRequest(http.MethodGet, "/api/search?q="+q201, nil)
	rr := httptest.NewRecorder()
	h.search(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("q=201 chars: status = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "query too long") {
		t.Errorf("body = %q, want it to mention query too long", rr.Body.String())
	}
}

// TestSearch_DBError_Returns500 verifies the handler returns 500 when the
// repository returns an error.
func TestSearch_DBError_Returns500(t *testing.T) {
	repo := &fakeRepo{searchErr: errors.New("simulated db failure")}
	h := newSearchHandler(repo)

	req := httptest.NewRequest(http.MethodGet, "/api/search?q=anything", nil)
	rr := httptest.NewRecorder()
	h.search(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 on DB error", rr.Code)
	}
}
