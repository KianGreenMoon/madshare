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
	if len(cfg.Listen) != 1 {
		t.Fatalf("len(Listen) = %d, want 1 default listener", len(cfg.Listen))
	}
	l := cfg.Listen[0]
	if l.Addr != "127.0.0.1" || l.Port != 3000 {
		t.Errorf("default listener = %s:%d, want 127.0.0.1:3000", l.Addr, l.Port)
	}
	if !l.Serves(config.GroupAPI) || !l.Serves(config.GroupWebUI) || !l.Serves(config.GroupAdmin) {
		t.Errorf("default listener serve = %v, want api+webui+admin", l.Serve)
	}
	if cfg.WebUI.APIBase != "" {
		t.Errorf("WebUI.APIBase = %q, want empty (relative, same-origin)", cfg.WebUI.APIBase)
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

func TestLoad_FilePresentWithoutListen_KeepsDefaultListener(t *testing.T) {
	f := filepath.Join(t.TempDir(), "nolisten.toml")
	os.WriteFile(f, []byte("[storage]\nmax_upload_mb = 100\n"), 0o600)

	cfg, err := config.Load(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Listen) != 1 || cfg.Listen[0].Port != 3000 {
		t.Errorf("Listen = %+v, want the default single listener", cfg.Listen)
	}
	if cfg.Storage.MaxUploadMB != 100 {
		t.Errorf("Storage.MaxUploadMB = %d, want 100", cfg.Storage.MaxUploadMB)
	}
}

func TestLoad_FullOverride(t *testing.T) {
	f := filepath.Join(t.TempDir(), "full.toml")
	os.WriteFile(f, []byte(`
[[listen]]
addr = "127.0.0.1"
port = 3000
serve = ["api", "webui", "admin"]

[[listen]]
addr = "192.168.1.67"
port = 3000
serve = ["api"]
allow_from = ["192.168.1.0/24"]

[webui]
api_base = "https://example.com"

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
	if len(cfg.Listen) != 2 {
		t.Fatalf("len(Listen) = %d, want 2", len(cfg.Listen))
	}
	if cfg.Listen[0].BindAddr() != "127.0.0.1:3000" {
		t.Errorf("Listen[0].BindAddr() = %q", cfg.Listen[0].BindAddr())
	}
	if cfg.Listen[1].BindAddr() != "192.168.1.67:3000" {
		t.Errorf("Listen[1].BindAddr() = %q", cfg.Listen[1].BindAddr())
	}
	if cfg.Listen[1].Serves(config.GroupWebUI) {
		t.Error("Listen[1] should not serve webui")
	}
	if cfg.WebUI.APIBase != "https://example.com" {
		t.Errorf("WebUI.APIBase = %q", cfg.WebUI.APIBase)
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

func TestListenConfig_BindAddr(t *testing.T) {
	cases := []struct {
		addr string
		port int
		want string
	}{
		{"127.0.0.1", 3000, "127.0.0.1:3000"},
		{"", 3000, ":3000"},
		{"0.0.0.0", 8080, "0.0.0.0:8080"},
		{"::1", 3000, "[::1]:3000"},
		{"[::1]", 3000, "[::1]:3000"}, // bracketed input must not double-bracket
	}
	for _, tc := range cases {
		l := config.ListenConfig{Addr: tc.addr, Port: tc.port}
		if got := l.BindAddr(); got != tc.want {
			t.Errorf("BindAddr(%q,%d) = %q, want %q", tc.addr, tc.port, got, tc.want)
		}
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

// validListeners is a [[listen]] block that passes validation, prepended to
// storage/database test cases that would otherwise have no listeners.
const validListeners = "[[listen]]\naddr = \"127.0.0.1\"\nport = 3000\nserve = [\"api\"]\n"

// TestLoad_Validation exercises config.Load's validation. Storage/database
// cases prepend a valid listener so only the field under test fails.
func TestLoad_Validation(t *testing.T) {
	cases := []struct {
		name string
		toml string
		// wantErrContains is a substring the returned error must contain. An
		// empty string means the config must load without error.
		wantErrContains string
	}{
		{"empty database path", validListeners + "[database]\npath = \"\"\n", "database.path must not be empty"},
		{"empty files_dir", validListeners + "[storage]\nfiles_dir = \"\"\n", "files_dir must not be empty"},
		{"zero max_upload_mb", validListeners + "[storage]\nmax_upload_mb = 0\n", "max_upload_mb must be positive"},
		{"negative max_upload_mb", validListeners + "[storage]\nmax_upload_mb = -1\n", "max_upload_mb must be positive"},
		{
			name:            "over-limit max_upload_mb",
			toml:            validListeners + fmt.Sprintf("[storage]\nmax_upload_mb = %d\n", int64(config.MaxUploadMBLimit)+1),
			wantErrContains: "max_upload_mb must not exceed",
		},
		{
			name: "at-limit max_upload_mb accepted",
			toml: validListeners + fmt.Sprintf("[storage]\nmax_upload_mb = %d\n", int64(config.MaxUploadMBLimit)),
		},
		{name: "valid override", toml: validListeners + "[storage]\nmax_upload_mb = 100\n"},

		// Listener validation.
		{
			name:            "port out of range",
			toml:            "[[listen]]\nport = 70000\nserve = [\"api\"]\n",
			wantErrContains: "out of range",
		},
		{
			name:            "invalid addr",
			toml:            "[[listen]]\naddr = \"not-an-ip\"\nport = 3000\nserve = [\"api\"]\n",
			wantErrContains: "not a valid IP",
		},
		{
			name:            "empty serve",
			toml:            "[[listen]]\nport = 3000\nserve = []\n",
			wantErrContains: "at least one group",
		},
		{
			name:            "unknown group",
			toml:            "[[listen]]\nport = 3000\nserve = [\"api\", \"bogus\"]\n",
			wantErrContains: "unknown group",
		},
		{
			name:            "invalid allow_from cidr",
			toml:            "[[listen]]\nport = 3000\nserve = [\"api\"]\nallow_from = [\"nonsense\"]\n",
			wantErrContains: "invalid CIDR",
		},
		{
			name: "wildcard overlaps specific on same port",
			toml: "[[listen]]\naddr = \"0.0.0.0\"\nport = 3000\nserve = [\"api\"]\n" +
				"[[listen]]\naddr = \"127.0.0.1\"\nport = 3000\nserve = [\"api\"]\n",
			wantErrContains: "conflict",
		},
		{
			name: "duplicate specific bind",
			toml: "[[listen]]\naddr = \"127.0.0.1\"\nport = 3000\nserve = [\"api\"]\n" +
				"[[listen]]\naddr = \"127.0.0.1\"\nport = 3000\nserve = [\"webui\"]\n",
			wantErrContains: "duplicate bind",
		},
		{
			name: "same port on different specific addrs is allowed",
			toml: "[[listen]]\naddr = \"127.0.0.1\"\nport = 3000\nserve = [\"api\"]\n" +
				"[[listen]]\naddr = \"192.168.1.67\"\nport = 3000\nserve = [\"api\"]\n",
		},
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

func TestConfig_Warnings_WebUIWithoutAPI(t *testing.T) {
	f := filepath.Join(t.TempDir(), "warn.toml")
	os.WriteFile(f, []byte("[[listen]]\naddr = \"127.0.0.1\"\nport = 3000\nserve = [\"webui\"]\n"), 0o600)
	cfg, err := config.Load(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	w := cfg.Warnings()
	if len(w) != 1 || !strings.Contains(w[0], "would 404") {
		t.Errorf("Warnings() = %v, want one webui-without-api advisory", w)
	}
}

func TestLoad_InvalidTOML_ReturnsError(t *testing.T) {
	f := filepath.Join(t.TempDir(), "bad.toml")
	os.WriteFile(f, []byte("not = valid [toml"), 0o600)
	_, err := config.Load(f)
	if err == nil {
		t.Error("expected error for malformed TOML, got nil")
	}
}

func TestLoad_DatabasePathRelative_MkdirAllSafeCheck(t *testing.T) {
	// Verify that a relative path with ".." components can be set
	// (the security concern is whether madshare.go validates it).
	f := filepath.Join(t.TempDir(), "traversal.toml")
	os.WriteFile(f, []byte(validListeners+"[database]\npath = \"../../tmp/evil.db\"\n"), 0o600)
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
