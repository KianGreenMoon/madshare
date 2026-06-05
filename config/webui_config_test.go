package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"daemonlord.ygg/madshare/config"
)

func TestLoadWebUI_Defaults(t *testing.T) {
	cfg, err := config.LoadWebUI("/tmp/definitely_no_such_webui_98765.toml")
	if err != nil {
		t.Fatalf("expected no error on missing file, got: %v", err)
	}
	if cfg.Upload.DefaultParallelWorkers != 3 {
		t.Errorf("DefaultParallelWorkers = %d, want 3", cfg.Upload.DefaultParallelWorkers)
	}
	if cfg.Upload.MaxParallelWorkers != 10 {
		t.Errorf("MaxParallelWorkers = %d, want 10", cfg.Upload.MaxParallelWorkers)
	}
}

func TestLoadWebUI_MaxClamped(t *testing.T) {
	f := filepath.Join(t.TempDir(), "webui.toml")
	body := "[upload]\ndefault_parallel_workers = 8\nmax_parallel_workers = 2\n"
	if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := config.LoadWebUI(f)
	if err != nil {
		t.Fatalf("LoadWebUI: %v", err)
	}
	if cfg.Upload.DefaultParallelWorkers != 8 {
		t.Errorf("DefaultParallelWorkers = %d, want 8", cfg.Upload.DefaultParallelWorkers)
	}
	// max < default → max is raised to default.
	if cfg.Upload.MaxParallelWorkers != 8 {
		t.Errorf("MaxParallelWorkers = %d, want 8 (clamped up to default)", cfg.Upload.MaxParallelWorkers)
	}
}

func TestLoadWebUI_SubOneClamped(t *testing.T) {
	f := filepath.Join(t.TempDir(), "webui.toml")
	body := "[upload]\ndefault_parallel_workers = -5\nmax_parallel_workers = -1\n"
	if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := config.LoadWebUI(f)
	if err != nil {
		t.Fatalf("LoadWebUI: %v", err)
	}
	if cfg.Upload.DefaultParallelWorkers != 1 {
		t.Errorf("DefaultParallelWorkers = %d, want 1 (clamped)", cfg.Upload.DefaultParallelWorkers)
	}
	if cfg.Upload.MaxParallelWorkers != 1 {
		t.Errorf("MaxParallelWorkers = %d, want 1 (clamped)", cfg.Upload.MaxParallelWorkers)
	}
}

func TestDefaultUIConfig(t *testing.T) {
	cfg := config.DefaultUIConfig()
	if cfg.Upload.DefaultParallelWorkers != 3 || cfg.Upload.MaxParallelWorkers != 10 {
		t.Errorf("DefaultUIConfig = %+v, want {3, 10}", cfg.Upload)
	}
}
