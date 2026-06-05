package config

import (
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
)

// Built-in defaults for the UI upload controls, used when webui.toml is absent
// or leaves a field at its zero value.
const (
	defaultUploadParallelWorkers = 3
	defaultUploadMaxWorkers      = 10
)

// UIUploadConfig holds the upload-page worker controls. DefaultParallelWorkers
// is the slider's initial value; MaxParallelWorkers is the ceiling it can be
// raised to in the browser.
type UIUploadConfig struct {
	DefaultParallelWorkers int `toml:"default_parallel_workers" json:"default_parallel_workers"`
	MaxParallelWorkers     int `toml:"max_parallel_workers" json:"max_parallel_workers"`
}

// UIConfig is the parsed webui.toml. It is distinct from WebUIConfig, which is
// the [webui] section of the main config (the API-base override). UIConfig is
// served verbatim to the browser via GET /api/ui/config (hence the json tags).
type UIConfig struct {
	Upload UIUploadConfig `toml:"upload" json:"upload"`
}

// DefaultUIConfig returns a UIConfig populated with the built-in defaults. Used
// as the fallback when no webui.toml was loaded (e.g. the GET /api/ui/config
// handler running without a configured UIConfig).
func DefaultUIConfig() *UIConfig {
	c := &UIConfig{}
	c.clamp()
	return c
}

// LoadWebUI reads the webui.toml at path. If the file does not exist, built-in
// defaults are returned with no error. Returns an error only on parse failure.
// Out-of-range values are clamped (never fatal).
func LoadWebUI(path string) (*UIConfig, error) {
	cfg := &UIConfig{
		Upload: UIUploadConfig{
			DefaultParallelWorkers: defaultUploadParallelWorkers,
			MaxParallelWorkers:     defaultUploadMaxWorkers,
		},
	}
	if _, err := os.Stat(path); err != nil {
		if !os.IsNotExist(err) {
			return cfg, fmt.Errorf("webui config file %s: %w", path, err)
		}
		// Missing file: defaults are fine.
		cfg.clamp()
		return cfg, nil
	}
	if _, err := toml.DecodeFile(path, cfg); err != nil {
		return cfg, err
	}
	cfg.clamp()
	return cfg, nil
}

// clamp folds out-of-range upload-worker values back into a usable range. A
// field left at zero by the file falls back to its default; sub-1 values clamp
// to 1; a max below the default is raised to the default.
func (c *UIConfig) clamp() {
	if c.Upload.DefaultParallelWorkers == 0 {
		c.Upload.DefaultParallelWorkers = defaultUploadParallelWorkers
	}
	if c.Upload.MaxParallelWorkers == 0 {
		c.Upload.MaxParallelWorkers = defaultUploadMaxWorkers
	}
	if c.Upload.DefaultParallelWorkers < 1 {
		c.Upload.DefaultParallelWorkers = 1
	}
	if c.Upload.MaxParallelWorkers < 1 {
		c.Upload.MaxParallelWorkers = 1
	}
	if c.Upload.MaxParallelWorkers < c.Upload.DefaultParallelWorkers {
		c.Upload.MaxParallelWorkers = c.Upload.DefaultParallelWorkers
	}
}
