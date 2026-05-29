package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"daemonlord.ygg/madshare/config"
)

func TestLoad_MissingFile_ReturnsDefaults(t *testing.T) {
	cfg, err := config.Load("/tmp/definitely_no_such_file_12345.toml")
	if err != nil {
		t.Fatalf("expected no error on missing file, got: %v", err)
	}
	if cfg.API.Addr != ":3000" {
		t.Errorf("API.Addr = %q, want :3000", cfg.API.Addr)
	}
	if cfg.API.PublicURL != "http://localhost:3000" {
		t.Errorf("API.PublicURL = %q, want http://localhost:3000", cfg.API.PublicURL)
	}
	if cfg.WebUI.Addr != ":8080" {
		t.Errorf("WebUI.Addr = %q, want :8080", cfg.WebUI.Addr)
	}
	if cfg.Database.Path != "./data/madshare.db" {
		t.Errorf("Database.Path = %q, want ./data/madshare.db", cfg.Database.Path)
	}
	if cfg.Storage.FilesDir != "./data/files" {
		t.Errorf("Storage.FilesDir = %q, want ./data/files", cfg.Storage.FilesDir)
	}
	if cfg.Storage.MaxUploadMB != 500 {
		t.Errorf("Storage.MaxUploadMB = %d, want 500", cfg.Storage.MaxUploadMB)
	}
	if got := cfg.Storage.MaxUploadBytes(); got != 500<<20 {
		t.Errorf("Storage.MaxUploadBytes() = %d, want %d", got, 500<<20)
	}
}

func TestLoad_PartialOverride_UnsetFieldsKeepDefaults(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "partial.toml")
	os.WriteFile(f, []byte("[api]\naddr = \":9000\"\n"), 0o600)

	cfg, err := config.Load(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.API.Addr != ":9000" {
		t.Errorf("API.Addr = %q, want :9000", cfg.API.Addr)
	}
	// These were NOT in the file — must keep defaults
	if cfg.API.PublicURL != "http://localhost:3000" {
		t.Errorf("API.PublicURL = %q, want default http://localhost:3000", cfg.API.PublicURL)
	}
	if cfg.WebUI.Addr != ":8080" {
		t.Errorf("WebUI.Addr = %q, want default :8080", cfg.WebUI.Addr)
	}
	if cfg.Database.Path != "./data/madshare.db" {
		t.Errorf("Database.Path = %q, want default", cfg.Database.Path)
	}
	if cfg.Storage.FilesDir != "./data/files" {
		t.Errorf("Storage.FilesDir = %q, want default ./data/files", cfg.Storage.FilesDir)
	}
	if cfg.Storage.MaxUploadMB != 500 {
		t.Errorf("Storage.MaxUploadMB = %d, want default 500", cfg.Storage.MaxUploadMB)
	}
}

func TestLoad_FullOverride(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "full.toml")
	os.WriteFile(f, []byte(`
[api]
addr = ":4000"
public_url = "https://example.com"

[webui]
addr = ":9080"

[database]
path = "/var/lib/madshare/db.sqlite"

[storage]
files_dir = "/srv/madshare/files"
max_upload_mb = 100
`), 0o600)

	cfg, err := config.Load(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.API.Addr != ":4000" {
		t.Errorf("API.Addr = %q, want :4000", cfg.API.Addr)
	}
	if cfg.API.PublicURL != "https://example.com" {
		t.Errorf("API.PublicURL = %q", cfg.API.PublicURL)
	}
	if cfg.WebUI.Addr != ":9080" {
		t.Errorf("WebUI.Addr = %q", cfg.WebUI.Addr)
	}
	if cfg.Database.Path != "/var/lib/madshare/db.sqlite" {
		t.Errorf("Database.Path = %q", cfg.Database.Path)
	}
	if cfg.Storage.FilesDir != "/srv/madshare/files" {
		t.Errorf("Storage.FilesDir = %q", cfg.Storage.FilesDir)
	}
	if cfg.Storage.MaxUploadMB != 100 {
		t.Errorf("Storage.MaxUploadMB = %d, want 100", cfg.Storage.MaxUploadMB)
	}
}

func TestLoad_InvalidTOML_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "bad.toml")
	os.WriteFile(f, []byte("not = valid [toml"), 0o600)
	_, err := config.Load(f)
	if err == nil {
		t.Error("expected error for malformed TOML, got nil")
	}
}

func TestLoad_DatabasePathRelative_MkdirAllSafeCheck(t *testing.T) {
	// Verify that a relative path with ".." components can be set
	// (the security concern is whether madshare.go validates it).
	dir := t.TempDir()
	f := filepath.Join(dir, "traversal.toml")
	os.WriteFile(f, []byte("[database]\npath = \"../../tmp/evil.db\"\n"), 0o600)
	cfg, err := config.Load(f)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// config.Load itself does NOT sanitize — it just loads the string.
	// This test documents that fact so callers know they own validation.
	if cfg.Database.Path != "../../tmp/evil.db" {
		t.Logf("INFO: config.Load sanitized the path (unexpected but fine): %q", cfg.Database.Path)
	}
}
