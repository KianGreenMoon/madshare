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
