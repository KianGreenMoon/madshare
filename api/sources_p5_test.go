package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/storages"
)

// seedLinkedFile creates a real external original, symlinks it into the links
// storage, and inserts a links-backed (approved) catalog row. Returns the hash.
func seedLinkedFile(t *testing.T, h *handler, linker *storages.Linker, name, content string) (hash, external string) {
	t.Helper()
	hash = sha256OfSourcesTest(content)
	external = filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(external, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := linker.Link(hash, name, external); err != nil {
		t.Fatalf("link: %v", err)
	}
	f := &database.File{
		Hash: hash, ByteSize: int64(len(content)), MimeType: "audio/flac",
		StorageBackend: database.StorageBackendLinks, ObjectKey: hash + "/" + name,
		LinkTarget: sql.NullString{String: external, Valid: true},
		CreatedAt:  1, ReviewState: database.ReviewApproved,
	}
	if err := h.repo.InsertFile(context.Background(), f, &database.FileUpload{Filename: name}, nil); err != nil {
		t.Fatalf("insert linked file: %v", err)
	}
	return hash, external
}

// Hard-deleting a links import must unlink only the symlink, leaving the external
// original byte-intact — the data-sources safety invariant.
func TestAdminTrashHardDelete_LinksUnlinksOnly(t *testing.T) {
	h, db, _ := newTestHandler(t)
	linksDir := t.TempDir()
	h.linker = storages.NewLinker(linksDir)

	hash, external := seedLinkedFile(t, h, h.linker, "song.flac", "external audio bytes")

	// Soft-delete (trash), then read the trashed appearance's id from the lens
	// and hard-delete it by tagset id (recording-tagsets P7c: permanent delete is
	// tagset-addressed — one blob can host several trashed appearances).
	trashAppearancesOf(t, db, hash)
	tagsetID := trashedTagsetID(t, h, hash)

	rr := httptest.NewRecorder()
	h.tagsetHardDelete(rr, paramRequest(http.MethodDelete,
		"/api/admin/tagsets/"+strconv.FormatInt(tagsetID, 10), "tagsetID", strconv.FormatInt(tagsetID, 10), ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	json.NewDecoder(rr.Body).Decode(&resp)
	if n, _ := resp["blobs_removed"].(float64); n != 1 {
		t.Errorf("blobs_removed = %v, want 1", resp["blobs_removed"])
	}

	// DB row gone.
	if got, _ := db.GetFileByHash(context.Background(), hash); got != nil {
		t.Error("files row still present after hard delete")
	}
	// The symlink (hash dir) is gone.
	if _, err := os.Stat(filepath.Join(linksDir, "audio", hash)); !os.IsNotExist(err) {
		t.Errorf("links hash dir still present: %v", err)
	}
	// The external original is byte-intact — the invariant.
	if data, err := os.ReadFile(external); err != nil || string(data) != "external audio bytes" {
		t.Errorf("external original disturbed: data=%q err=%v", data, err)
	}
}

// storageStats reports links bytes as external_bytes, separate from LibraryBytes.
func TestAdminStorageStats_ExternalBytes(t *testing.T) {
	h, _, _ := newTestHandler(t)
	linksDir := t.TempDir()
	h.linker = storages.NewLinker(linksDir)
	seedLinkedFile(t, h, h.linker, "a.flac", "12345") // 5 bytes
	seedLinkedFile(t, h, h.linker, "b.flac", "678")   // 3 bytes

	rr := httptest.NewRecorder()
	h.adminStorageStats(rr, httptest.NewRequest(http.MethodGet, "/api/admin/storage", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		LibraryBytes  uint64 `json:"library_bytes"`
		ExternalBytes uint64 `json:"external_bytes"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ExternalBytes != 8 {
		t.Errorf("external_bytes = %d, want 8", resp.ExternalBytes)
	}
	// External bytes are NOT folded into the on-disk library footprint.
	if resp.LibraryBytes != 0 {
		t.Errorf("library_bytes = %d, want 0 (external bytes excluded)", resp.LibraryBytes)
	}
}

// GET /api/admin/sources reports links health (count / broken / external bytes).
func TestAdminSourcesList_LinksHealth(t *testing.T) {
	h, mgr := newSourcesHandler(t, t.TempDir())
	linksDir := t.TempDir()
	h.linker = storages.NewLinker(linksDir)
	seedLinkedFile(t, h, h.linker, "ok.flac", "live")
	// A dangling link: target removed after linking.
	_, gone := seedLinkedFile(t, h, h.linker, "gone.flac", "to-be-removed")
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	_ = mgr

	rr := httptest.NewRecorder()
	h.adminSourcesList(rr, httptest.NewRequest(http.MethodGet, "/api/admin/sources", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var resp struct {
		Links struct {
			Count         int    `json:"count"`
			Broken        int    `json:"broken"`
			ExternalBytes uint64 `json:"external_bytes"`
		} `json:"links"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Links.Count != 2 || resp.Links.Broken != 1 {
		t.Errorf("links health = %+v, want count=2 broken=1", resp.Links)
	}
	if resp.Links.ExternalBytes != uint64(len("live")) {
		t.Errorf("external_bytes = %d, want %d (only the live link)", resp.Links.ExternalBytes, len("live"))
	}
}

// trashedTagsetID reads the appearance id of the (single) trashed row from the
// Trash Appearances lens — the lens is the only place that names it.
func trashedTagsetID(t *testing.T, h *handler, hash string) int64 {
	t.Helper()
	rr := httptest.NewRecorder()
	h.adminTrashList(rr, httptest.NewRequest(http.MethodGet, "/api/admin/trash", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("trash list status = %d", rr.Code)
	}
	var env struct {
		Items []struct {
			TagsetID int64  `json:"tagset_id"`
			Hash     string `json:"hash"`
		} `json:"items"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&env); err != nil {
		t.Fatalf("decode trash list: %v", err)
	}
	for _, it := range env.Items {
		if it.Hash == hash {
			return it.TagsetID
		}
	}
	t.Fatalf("no trashed appearance for hash %s", hash)
	return 0
}
