package config

import (
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
)

type Config struct {
	API      APIConfig      `toml:"api"`
	WebUI    WebUIConfig    `toml:"webui"`
	Database DatabaseConfig `toml:"database"`
	Storage  StorageConfig  `toml:"storage"`
}

type APIConfig struct {
	Addr      string `toml:"addr"`
	PublicURL string `toml:"public_url"`
}

type WebUIConfig struct {
	Addr string `toml:"addr"`
}

type DatabaseConfig struct {
	Path string `toml:"path"`
}

// MaxUploadMBLimit is the largest accepted value for max_upload_mb (1 TiB
// expressed in MiB). It is far below the point at which MaxUploadBytes would
// overflow int64, so MaxUploadBytes cannot wrap negative for any accepted
// config value.
const MaxUploadMBLimit = 1 << 20

type StorageConfig struct {
	// FilesDir is the directory where uploaded blobs are stored and served.
	FilesDir string `toml:"files_dir"`
	// MaxUploadMB caps the size of a single upload request body, in MiB. It is
	// distinct from the in-memory hashing threshold (storage.memBufferLimit),
	// above which an upload is spooled to the cache dir rather than buffered.
	MaxUploadMB int64 `toml:"max_upload_mb"`
}

// MaxUploadBytes returns the configured upload cap in bytes.
func (s StorageConfig) MaxUploadBytes() int64 {
	return s.MaxUploadMB << 20
}

func defaults() Config {
	return Config{
		API: APIConfig{
			Addr:      ":3000",
			PublicURL: "http://localhost:3000",
		},
		WebUI: WebUIConfig{
			Addr: ":8080",
		},
		Database: DatabaseConfig{
			Path: "./data/madshare.db",
		},
		Storage: StorageConfig{
			FilesDir:    "./data/files",
			MaxUploadMB: 500,
		},
	}
}

// Load reads the TOML config file at path. If the file does not exist the
// defaults are returned. Fields absent from the file keep their default values.
func Load(path string) (Config, error) {
	cfg := defaults()
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("config file %s: %w", path, err)
	}
	_, err := toml.DecodeFile(path, &cfg)
	return cfg, err
}
