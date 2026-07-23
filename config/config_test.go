package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
	if cfg.DataDir != "./data" {
		t.Errorf("DataDir = %q, want ./data", cfg.DataDir)
	}
	// database.path / files_dir are derived from data_dir via filepath.Join,
	// which canonicalises "./data" to "data" — the same path, resolved against CWD.
	if cfg.Database.Path != "data/madshare.db" {
		t.Errorf("Database.Path = %q, want data/madshare.db", cfg.Database.Path)
	}
	if cfg.Storage.FilesDir != "data/files" {
		t.Errorf("Storage.FilesDir = %q, want data/files", cfg.Storage.FilesDir)
	}
	if cfg.Storage.VariantsDir != "data/variants" {
		t.Errorf("Storage.VariantsDir = %q, want data/variants", cfg.Storage.VariantsDir)
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

func TestLoad_ImageWorkersAuto(t *testing.T) {
	// Unset image_processing_workers resolves to runtime.NumCPU() (>= 1).
	cfg, err := config.Load("/tmp/definitely_no_such_file_54321.toml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Storage.ImageProcessingWorkers != runtime.NumCPU() {
		t.Errorf("ImageProcessingWorkers = %d, want NumCPU=%d", cfg.Storage.ImageProcessingWorkers, runtime.NumCPU())
	}
}

func TestLoad_ImageWorkersExplicit(t *testing.T) {
	f := filepath.Join(t.TempDir(), "workers.toml")
	os.WriteFile(f, []byte("[storage]\nimage_processing_workers = 4\n"), 0o600)
	cfg, err := config.Load(f)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Storage.ImageProcessingWorkers != 4 {
		t.Errorf("ImageProcessingWorkers = %d, want 4", cfg.Storage.ImageProcessingWorkers)
	}
}

func TestLoad_ImageWorkersNegative(t *testing.T) {
	f := filepath.Join(t.TempDir(), "workers.toml")
	os.WriteFile(f, []byte("[storage]\nimage_processing_workers = -1\nserver_max_parallel_workers = -3\n"), 0o600)
	cfg, err := config.Load(f)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Storage.ImageProcessingWorkers != runtime.NumCPU() {
		t.Errorf("ImageProcessingWorkers = %d, want NumCPU=%d (negative → auto)", cfg.Storage.ImageProcessingWorkers, runtime.NumCPU())
	}
	if cfg.Storage.ServerMaxParallelWorkers != 0 {
		t.Errorf("ServerMaxParallelWorkers = %d, want 0 (negative → unlimited)", cfg.Storage.ServerMaxParallelWorkers)
	}
	// The clamps are surfaced as non-fatal warnings, not errors.
	if len(cfg.Warnings()) == 0 {
		t.Error("expected at least one warning for the clamped values, got none")
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

// ── data_dir derivation & overrides ──────────────────────────────────────────

func TestLoad_DataDir_DerivesPaths(t *testing.T) {
	f := filepath.Join(t.TempDir(), "datadir.toml")
	os.WriteFile(f, []byte("data_dir = \"/var/lib/madshare\"\n"+validListeners), 0o600)
	cfg, err := config.Load(f)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DataDir != "/var/lib/madshare" {
		t.Errorf("DataDir = %q, want /var/lib/madshare", cfg.DataDir)
	}
	if cfg.Database.Path != "/var/lib/madshare/madshare.db" {
		t.Errorf("Database.Path = %q, want derived under data_dir", cfg.Database.Path)
	}
	if cfg.Storage.FilesDir != "/var/lib/madshare/files" {
		t.Errorf("Storage.FilesDir = %q, want derived under data_dir", cfg.Storage.FilesDir)
	}
	if cfg.Storage.VariantsDir != "/var/lib/madshare/variants" {
		t.Errorf("Storage.VariantsDir = %q, want derived under data_dir", cfg.Storage.VariantsDir)
	}
}

// An explicit database.path / files_dir overrides the data_dir derivation
// independently — one can be derived while the other is set.
func TestLoad_DataDir_ExplicitPathsOverride(t *testing.T) {
	f := filepath.Join(t.TempDir(), "datadir.toml")
	os.WriteFile(f, []byte("data_dir = \"/var/lib/madshare\"\n"+validListeners+
		"[database]\npath = \"/srv/db/m.sqlite\"\n"), 0o600)
	cfg, err := config.Load(f)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Database.Path != "/srv/db/m.sqlite" {
		t.Errorf("Database.Path = %q, want explicit override", cfg.Database.Path)
	}
	// files_dir was not set, so it still derives from data_dir.
	if cfg.Storage.FilesDir != "/var/lib/madshare/files" {
		t.Errorf("Storage.FilesDir = %q, want derived under data_dir", cfg.Storage.FilesDir)
	}
}

// variants_dir overrides the data_dir derivation independently of files_dir.
func TestLoad_DataDir_ExplicitVariantsDirOverride(t *testing.T) {
	f := filepath.Join(t.TempDir(), "datadir.toml")
	os.WriteFile(f, []byte("data_dir = \"/var/lib/madshare\"\n"+validListeners+
		"[storage]\nvariants_dir = \"/srv/derived\"\n"), 0o600)
	cfg, err := config.Load(f)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Storage.VariantsDir != "/srv/derived" {
		t.Errorf("Storage.VariantsDir = %q, want explicit override", cfg.Storage.VariantsDir)
	}
	// files_dir was not set, so it still derives from data_dir.
	if cfg.Storage.FilesDir != "/var/lib/madshare/files" {
		t.Errorf("Storage.FilesDir = %q, want derived under data_dir", cfg.Storage.FilesDir)
	}
}

func TestConfig_LinksDir(t *testing.T) {
	cases := []struct{ dataDir, want string }{
		{"./data", "data/links"},
		{"/var/lib/madshare", "/var/lib/madshare/links"},
		{"/var/lib/madshare/", "/var/lib/madshare/links"},
	}
	for _, tc := range cases {
		c := config.Config{DataDir: tc.dataDir}
		if got := c.LinksDir(); got != tc.want {
			t.Errorf("LinksDir() with data_dir %q = %q, want %q", tc.dataDir, got, tc.want)
		}
	}
}

func TestSourcesConfig_SymlinkSourcesEnabled(t *testing.T) {
	if (config.SourcesConfig{}).SymlinkSourcesEnabled() {
		t.Error("empty symlink_roots should disable symlink sources")
	}
	if !(config.SourcesConfig{SymlinkRoots: []string{"/srv/music"}}).SymlinkSourcesEnabled() {
		t.Error("a configured root should enable symlink sources")
	}
}

func TestLoad_Sources_ValidRoots(t *testing.T) {
	dir := t.TempDir() // an existing absolute dir → no warning
	f := filepath.Join(t.TempDir(), "sources.toml")
	body := validListeners + "[sources]\nsymlink_roots = [\"" + dir + "\"]\n"
	if err := os.WriteFile(f, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(f)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Sources.SymlinkSourcesEnabled() {
		t.Error("symlink sources should be enabled")
	}
	for _, w := range cfg.Warnings() {
		if strings.Contains(w, "symlink_roots") {
			t.Errorf("unexpected warning for an existing root: %q", w)
		}
	}
}

func TestLoad_Sources_NonexistentRoot_Warns(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-there")
	f := filepath.Join(t.TempDir(), "sources.toml")
	body := validListeners + "[sources]\nsymlink_roots = [\"" + missing + "\"]\n"
	if err := os.WriteFile(f, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(f) // a missing root is an advisory, not fatal
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	found := false
	for _, w := range cfg.Warnings() {
		if strings.Contains(w, "symlink_roots") && strings.Contains(w, "does not exist") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a non-existent-root warning, got %v", cfg.Warnings())
	}
}

func TestLoad_Sources_InvalidRoot_Errors(t *testing.T) {
	cases := []struct{ name, root string }{
		{"relative", "srv/music"},
		{"not-clean", "/srv/music/"},
		{"dotdot", "/srv/../srv/music/.."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := filepath.Join(t.TempDir(), "sources.toml")
			body := validListeners + "[sources]\nsymlink_roots = [\"" + tc.root + "\"]\n"
			if err := os.WriteFile(f, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := config.Load(f); err == nil {
				t.Errorf("Load with root %q should fail validation", tc.root)
			}
		})
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
		{"empty data_dir", "data_dir = \"\"\n" + validListeners, "data_dir must not be empty"},
		// An explicit-but-empty database.path / files_dir is no longer an error:
		// it means "derive from data_dir", so these load cleanly.
		{name: "empty database path derives from data_dir", toml: validListeners + "[database]\npath = \"\"\n"},
		{name: "empty files_dir derives from data_dir", toml: validListeners + "[storage]\nfiles_dir = \"\"\n"},
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

func TestConfig_ReachableWindow_DefaultAndClamp(t *testing.T) {
	load := func(t *testing.T, body string) config.Config {
		t.Helper()
		f := filepath.Join(t.TempDir(), "w.toml")
		if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Load(f)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		return cfg
	}
	base := "[[listen]]\naddr=\"127.0.0.1\"\nport=3000\nserve=[\"api\"]\n"

	// Unset → default.
	if cfg := load(t, base); cfg.Federation.ReachableWindowSec != config.DefaultReachableWindowSec {
		t.Errorf("unset window = %d, want default %d", cfg.Federation.ReachableWindowSec, config.DefaultReachableWindowSec)
	}
	// A valid explicit value is kept.
	if cfg := load(t, base+"[federation]\nreachable_window_sec = 300\n"); cfg.Federation.ReachableWindowSec != 300 {
		t.Errorf("explicit window = %d, want 300", cfg.Federation.ReachableWindowSec)
	}
	// Below the floor → clamped up with a warning.
	cfg := load(t, base+"[federation]\nreachable_window_sec = 30\n")
	if cfg.Federation.ReachableWindowSec != config.MinReachableWindowSec {
		t.Errorf("too-small window = %d, want clamp to %d", cfg.Federation.ReachableWindowSec, config.MinReachableWindowSec)
	}
	var warned bool
	for _, w := range cfg.Warnings() {
		if strings.Contains(w, "reachable_window_sec") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("expected a reachable_window_sec clamp warning, got %v", cfg.Warnings())
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

// ── [webui].git_repo (the header GitRepo button) ─────────────────────────────

func TestLoad_GitRepoAbsent_UsesDefault(t *testing.T) {
	f := filepath.Join(t.TempDir(), "norepo.toml")
	os.WriteFile(f, []byte("[webui]\napi_base = \"\"\n"), 0o600)
	cfg, err := config.Load(f)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.WebUI.GitRepoURL(); got != config.DefaultGitRepoURL {
		t.Errorf("GitRepoURL() = %q, want default %q", got, config.DefaultGitRepoURL)
	}
}

func TestLoad_GitRepoEmpty_HidesButton(t *testing.T) {
	f := filepath.Join(t.TempDir(), "norepo.toml")
	os.WriteFile(f, []byte("[webui]\ngit_repo = \"\"\n"), 0o600)
	cfg, err := config.Load(f)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.WebUI.GitRepoURL(); got != "" {
		t.Errorf("GitRepoURL() = %q, want empty (button hidden)", got)
	}
}

func TestLoad_GitRepoCustom(t *testing.T) {
	f := filepath.Join(t.TempDir(), "repo.toml")
	os.WriteFile(f, []byte("[webui]\ngit_repo = \"https://git.example.org/me/fork\"\n"), 0o600)
	cfg, err := config.Load(f)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.WebUI.GitRepoURL(); got != "https://git.example.org/me/fork" {
		t.Errorf("GitRepoURL() = %q", got)
	}
	if len(cfg.Warnings()) != 0 {
		t.Errorf("unexpected warnings for an https URL: %v", cfg.Warnings())
	}
}

func TestLoad_GitRepoNonHTTP_Warns(t *testing.T) {
	f := filepath.Join(t.TempDir(), "repo.toml")
	os.WriteFile(f, []byte("[webui]\ngit_repo = \"git@github.com:me/fork.git\"\n"), 0o600)
	cfg, err := config.Load(f)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	warned := false
	for _, w := range cfg.Warnings() {
		if strings.Contains(w, "git_repo") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("expected a git_repo warning, got %v", cfg.Warnings())
	}
}

// ── [cors].allowed_origins ───────────────────────────────────────────────────

func TestLoad_CORS_ValidOrigins(t *testing.T) {
	f := filepath.Join(t.TempDir(), "cors.toml")
	os.WriteFile(f, []byte(validListeners+
		"[cors]\nallowed_origins = [\"https://ui.example\", \"http://localhost:5173\", \"*\"]\n"), 0o600)
	cfg, err := config.Load(f)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.CORS.AllowedOrigins) != 3 {
		t.Errorf("AllowedOrigins = %v, want 3 entries", cfg.CORS.AllowedOrigins)
	}
}

func TestLoad_CORS_InvalidOrigin_Errors(t *testing.T) {
	for _, bad := range []string{
		"ui.example",             // no scheme
		"https://ui.example/app", // has a path
		"https://ui.example?x=1", // has a query
		"ftp://ui.example",       // wrong scheme
		"https://",               // no host
	} {
		f := filepath.Join(t.TempDir(), "cors.toml")
		os.WriteFile(f, []byte(validListeners+"[cors]\nallowed_origins = [\""+bad+"\"]\n"), 0o600)
		if _, err := config.Load(f); err == nil {
			t.Errorf("Load with origin %q = nil error, want rejection", bad)
		}
	}
}

func TestLoad_CORS_WildcardPlusSpecific_Warns(t *testing.T) {
	f := filepath.Join(t.TempDir(), "cors.toml")
	os.WriteFile(f, []byte(validListeners+
		"[cors]\nallowed_origins = [\"*\", \"https://ui.example\"]\n"), 0o600)
	cfg, err := config.Load(f)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !anyContains(cfg.Warnings(), "redundant") {
		t.Errorf("want a wildcard+specific redundancy warning, got %v", cfg.Warnings())
	}
}

func TestLoad_CORS_APIBaseWithoutOrigins_Warns(t *testing.T) {
	f := filepath.Join(t.TempDir(), "cors.toml")
	os.WriteFile(f, []byte(validListeners+
		"[webui]\napi_base = \"https://media.example.org\"\n"), 0o600)
	cfg, err := config.Load(f)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !anyContains(cfg.Warnings(), "cors.allowed_origins is empty") {
		t.Errorf("want an api_base-without-cors warning, got %v", cfg.Warnings())
	}
}

func anyContains(ss []string, sub string) bool {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
