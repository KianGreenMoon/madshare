package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"daemonlord.ygg/madshare/api/storage"
	"daemonlord.ygg/madshare/config"
	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/media"
	"github.com/go-chi/chi/v5"
)

// newImageTestServer builds an httptest server over a real in-memory DB with the
// given UIConfig wired, returning the server and the DB for seeding.
func newImageTestServer(t *testing.T, ui *config.UIConfig) (*httptest.Server, *database.DB) {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	r := chi.NewRouter()
	RegisterAPI(r, Deps{
		Store:         storage.NewLocal(dir),
		Repo:          db,
		CacheDir:      t.TempDir(),
		FilesDir:      dir,
		MaxUploadSize: testMaxUpload,
		UIConfig:      ui,
	})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, db
}

func TestGetAlbumImageStatus_WithCover(t *testing.T) {
	srv, db := newImageTestServer(t, nil)
	const (
		artist  = "Artist"
		album   = "Album"
		baseKey = "abcdef0123456789"
	)
	objectKey := media.VariantPath(baseKey, media.VariantOriginal, ".jpg")
	albumID, err := db.ResolveAlbumID(context.Background(), artist, album)
	if err != nil {
		t.Fatalf("resolve album id: %v", err)
	}
	if err := db.SetAlbumCover(context.Background(), albumID, baseKey, ".jpg", objectKey, "image/jpeg", 1000); err != nil {
		t.Fatalf("set album cover: %v", err)
	}

	resp, err := http.Get(srv.URL + "/api/albums/" + album + "/image/status?artist=" + artist)
	if err != nil {
		t.Fatalf("GET status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status code = %d, want 200", resp.StatusCode)
	}

	var body struct {
		HasCover      bool              `json:"has_cover"`
		VariantsReady bool              `json:"variants_ready"`
		BaseKey       string            `json:"base_key"`
		SourceExt     string            `json:"source_ext"`
		Variants      map[string]string `json:"variants"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.HasCover {
		t.Error("has_cover = false, want true")
	}
	if body.VariantsReady {
		t.Error("variants_ready = true, want false (worker has not run)")
	}
	if body.BaseKey != baseKey {
		t.Errorf("base_key = %q, want %q", body.BaseKey, baseKey)
	}
	if len(body.Variants) != len(media.AllVariants) {
		t.Fatalf("variants count = %d, want %d", len(body.Variants), len(media.AllVariants))
	}
	if got, want := body.Variants[media.VariantSmallCrop], media.VariantURL(baseKey, media.VariantSmallCrop, ".jpg"); got != want {
		t.Errorf("small_crop URL = %q, want %q", got, want)
	}
}

func TestGetAlbumImageStatus_NoCover(t *testing.T) {
	srv, _ := newImageTestServer(t, nil)
	resp, err := http.Get(srv.URL + "/api/albums/Nope/image/status?artist=Nobody")
	if err != nil {
		t.Fatalf("GET status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status code = %d, want 200", resp.StatusCode)
	}
	var body struct {
		HasCover bool              `json:"has_cover"`
		BaseKey  string            `json:"base_key"`
		Variants map[string]string `json:"variants"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.HasCover {
		t.Error("has_cover = true, want false")
	}
	if body.BaseKey != "" {
		t.Errorf("base_key = %q, want empty", body.BaseKey)
	}
	if len(body.Variants) != 0 {
		t.Errorf("variants = %v, want empty", body.Variants)
	}
}

func TestGetUIConfig_Wired(t *testing.T) {
	ui := &config.UIConfig{Upload: config.UIUploadConfig{DefaultParallelWorkers: 5, MaxParallelWorkers: 20}}
	srv, _ := newImageTestServer(t, ui)
	resp, err := http.Get(srv.URL + "/api/ui/config")
	if err != nil {
		t.Fatalf("GET ui config: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status code = %d, want 200", resp.StatusCode)
	}
	var body config.UIConfig
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Upload.DefaultParallelWorkers != 5 || body.Upload.MaxParallelWorkers != 20 {
		t.Errorf("upload config = %+v, want {5, 20}", body.Upload)
	}
}

func TestGetUIConfig_FallsBackToDefaults(t *testing.T) {
	srv, _ := newImageTestServer(t, nil) // no UIConfig wired
	resp, err := http.Get(srv.URL + "/api/ui/config")
	if err != nil {
		t.Fatalf("GET ui config: %v", err)
	}
	defer resp.Body.Close()
	var body config.UIConfig
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Upload.DefaultParallelWorkers != 3 || body.Upload.MaxParallelWorkers != 10 {
		t.Errorf("default upload config = %+v, want {3, 10}", body.Upload)
	}
}

// TestGetUIConfig_AcceptedAudio verifies the endpoint surfaces the server's
// accepted-audio allow-list (acceptedAudioTypes) so the upload page's type
// precheck and outgoing Content-Type stay in lockstep with the server.
func TestGetUIConfig_AcceptedAudio(t *testing.T) {
	srv, _ := newImageTestServer(t, nil)
	resp, err := http.Get(srv.URL + "/api/ui/config")
	if err != nil {
		t.Fatalf("GET ui config: %v", err)
	}
	defer resp.Body.Close()
	var body struct {
		AcceptedAudio map[string]string `json:"accepted_audio"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.AcceptedAudio) != len(acceptedAudioTypes) {
		t.Errorf("accepted_audio has %d entries, want %d", len(body.AcceptedAudio), len(acceptedAudioTypes))
	}
	if body.AcceptedAudio[".flac"] != "audio/flac" {
		t.Errorf(".flac => %q, want audio/flac", body.AcceptedAudio[".flac"])
	}
	if body.AcceptedAudio[".m4a"] != "audio/mp4" {
		t.Errorf(".m4a => %q, want audio/mp4", body.AcceptedAudio[".m4a"])
	}
}
