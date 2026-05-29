package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// TestMaxUploadBytes_NoOverflowAtLimit verifies the documented invariant:
// MaxUploadBytes stays positive for any value up to MaxUploadMBLimit, so an
// accepted config can never wrap int64 negative.
func TestMaxUploadBytes_NoOverflowAtLimit(t *testing.T) {
	s := config.StorageConfig{MaxUploadMB: config.MaxUploadMBLimit}
	if got := s.MaxUploadBytes(); got <= 0 {
		t.Errorf("MaxUploadBytes() at limit = %d, want positive", got)
	}
}

// TestLoad_Validation exercises config.Load's validation of storage and
// database fields, including the max_upload_mb bounds (now testable because
// validation lives in Load rather than in main()).
func TestLoad_Validation(t *testing.T) {
	cases := []struct {
		name string
		toml string
		// wantErrContains is a substring the returned error must contain. An
		// empty string means the config must load without error.
		wantErrContains string
	}{
		{"empty database path", "[database]\npath = \"\"\n", "database.path must not be empty"},
		{"empty files_dir", "[storage]\nfiles_dir = \"\"\n", "files_dir must not be empty"},
		{"zero max_upload_mb", "[storage]\nmax_upload_mb = 0\n", "max_upload_mb must be positive"},
		{"negative max_upload_mb", "[storage]\nmax_upload_mb = -1\n", "max_upload_mb must be positive"},
		{
			name:            "over-limit max_upload_mb",
			toml:            fmt.Sprintf("[storage]\nmax_upload_mb = %d\n", int64(config.MaxUploadMBLimit)+1),
			wantErrContains: "max_upload_mb must not exceed",
		},
		{
			name: "at-limit max_upload_mb accepted",
			toml: fmt.Sprintf("[storage]\nmax_upload_mb = %d\n", int64(config.MaxUploadMBLimit)),
		},
		{name: "valid override", toml: "[storage]\nmax_upload_mb = 100\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := filepath.Join(t.TempDir(), "c.toml")
			if err := os.WriteFile(f, []byte(tc.toml), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := config.Load(f)
			switch {
			case tc.wantErrContains == "":
				if err != nil {
					t.Errorf("Load(%q) = %v, want nil error", tc.toml, err)
				}
			case err == nil:
				t.Errorf("Load(%q) = nil error, want error containing %q", tc.toml, tc.wantErrContains)
			case !strings.Contains(err.Error(), tc.wantErrContains):
				t.Errorf("Load(%q) error = %q, want it to contain %q", tc.toml, err.Error(), tc.wantErrContains)
			}
		})
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
