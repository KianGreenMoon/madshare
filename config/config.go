package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// Route group tokens accepted in a listener's `serve` list. See
// docs/architecture/listeners-and-config.md for what each group covers.
const (
	GroupAPI   = "api"   // /, /api/* (non-admin), /files/*, /images/*
	GroupWebUI = "webui" // /, /cmus, /static/*
	GroupAdmin = "admin" // /api/admin/* and the /admin page
)

// knownGroups is the set of valid `serve` tokens.
var knownGroups = map[string]bool{
	GroupAPI:   true,
	GroupWebUI: true,
	GroupAdmin: true,
}

type Config struct {
	Listen   []ListenConfig `toml:"listen"`
	WebUI    WebUIConfig    `toml:"webui"`
	Database DatabaseConfig `toml:"database"`
	Storage  StorageConfig  `toml:"storage"`
	Auth     AuthConfig     `toml:"auth"`
}

// AuthConfig holds the first-run admin bootstrap credential. The password is
// consumed only when the users table is empty (see docs/architecture/auth.md
// §3.4); afterwards it is ignored. Prefer the MADSHARE_INITIAL_ADMIN_PASSWORD
// environment variable over writing the password into the config file.
type AuthConfig struct {
	InitialAdminUser     string `toml:"initial_admin_user"`
	InitialAdminPassword string `toml:"initial_admin_password"`
}

// InitialAdminPasswordEnv is the environment variable that overrides
// [auth].initial_admin_password when set.
const InitialAdminPasswordEnv = "MADSHARE_INITIAL_ADMIN_PASSWORD"

// ListenConfig describes one bound socket and the route groups served on it.
type ListenConfig struct {
	// Addr is the bind address: "" or "0.0.0.0"/"::" for all interfaces, or a
	// specific IP. IPv6 literals may be given with or without brackets.
	Addr string `toml:"addr"`
	// Port is the TCP port (1..65535).
	Port int `toml:"port"`
	// Serve is the non-empty subset of route groups mounted on this listener.
	Serve []string `toml:"serve"`
	// AllowFrom optionally restricts accepted requests to these source CIDRs;
	// empty means no source filtering. Not a substitute for authentication.
	AllowFrom []string `toml:"allow_from"`
}

// BindAddr returns the host:port string for net.Listen, normalising any
// bracketed IPv6 literal so JoinHostPort does not double-bracket it.
func (l ListenConfig) BindAddr() string {
	host := strings.Trim(l.Addr, "[]")
	return net.JoinHostPort(host, strconv.Itoa(l.Port))
}

// Serves reports whether this listener mounts the given route group.
func (l ListenConfig) Serves(group string) bool {
	return slices.Contains(l.Serve, group)
}

// WebUIConfig carries web-UI-specific settings. APIBase is the absolute API
// origin injected into the page (meta[name="api-url"]); leave it empty for the
// bundled, same-origin server so the front-end uses relative URLs. Set it only
// for a separately deployed UI pointing at a remote backend.
type WebUIConfig struct {
	APIBase string `toml:"api_base"`
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
		// Default: a single loopback listener serving the full stack. Loopback
		// is the safe default while the admin surface is unauthenticated.
		Listen: []ListenConfig{
			{
				Addr:  "127.0.0.1",
				Port:  3000,
				Serve: []string{GroupAPI, GroupWebUI, GroupAdmin},
			},
		},
		Database: DatabaseConfig{
			Path: "./data/madshare.db",
		},
		Storage: StorageConfig{
			FilesDir:    "./data/files",
			MaxUploadMB: 500,
		},
		Auth: AuthConfig{
			InitialAdminUser: "admin",
		},
	}
}

// Load reads the TOML config file at path. If the file does not exist the
// defaults are returned. Fields absent from the file keep their default values.
// The resulting config is validated before it is returned, so callers can rely
// on the returned Config being usable.
func Load(path string) (Config, error) {
	cfg := defaults()
	if _, err := os.Stat(path); err != nil {
		if !os.IsNotExist(err) {
			return cfg, fmt.Errorf("config file %s: %w", path, err)
		}
		// Missing file: fall through with defaults, which still get validated.
	} else {
		// A present file fully replaces the default listener list rather than
		// appending to it, so the user's [[listen]] entries are authoritative.
		cfg.Listen = nil
		if _, err := toml.DecodeFile(path, &cfg); err != nil {
			return cfg, err
		}
		if cfg.Listen == nil {
			// File present but defined no [[listen]] — restore the default.
			cfg.Listen = defaults().Listen
		}
	}
	// The env var, when set, overrides the file's initial admin password so the
	// secret need not live in the config file at all.
	if pw := os.Getenv(InitialAdminPasswordEnv); pw != "" {
		cfg.Auth.InitialAdminPassword = pw
	}
	if err := cfg.validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// validate checks that the config holds usable values. It is run by Load on
// every successfully-parsed config (including the all-defaults case). It
// returns only hard errors; soft advisories are reported by Warnings.
func (c Config) validate() error {
	if c.Database.Path == "" {
		return errors.New("config: database.path must not be empty")
	}
	if c.Storage.FilesDir == "" {
		return errors.New("config: storage.files_dir must not be empty")
	}
	if c.Storage.MaxUploadMB <= 0 {
		return errors.New("config: storage.max_upload_mb must be positive")
	}
	if c.Storage.MaxUploadMB > MaxUploadMBLimit {
		return fmt.Errorf("config: storage.max_upload_mb must not exceed %d", MaxUploadMBLimit)
	}
	return c.validateListeners()
}

func (c Config) validateListeners() error {
	if len(c.Listen) == 0 {
		return errors.New("config: at least one [[listen]] entry is required")
	}
	// seenKey maps a normalised bind key to the original addr for error text.
	wildcardPorts := map[int]bool{}
	specificKeys := map[string]bool{}
	for i, l := range c.Listen {
		if l.Port < 1 || l.Port > 65535 {
			return fmt.Errorf("config: listen[%d].port %d out of range 1..65535", i, l.Port)
		}
		host := strings.Trim(l.Addr, "[]")
		wildcard := host == ""
		if host != "" {
			ip := net.ParseIP(host)
			if ip == nil {
				return fmt.Errorf("config: listen[%d].addr %q is not a valid IP address", i, l.Addr)
			}
			wildcard = ip.IsUnspecified() // 0.0.0.0 or ::
		}
		if len(l.Serve) == 0 {
			return fmt.Errorf("config: listen[%d].serve must list at least one group", i)
		}
		for _, g := range l.Serve {
			if !knownGroups[g] {
				return fmt.Errorf("config: listen[%d].serve has unknown group %q (valid: %s, %s, %s)",
					i, g, GroupAPI, GroupWebUI, GroupAdmin)
			}
		}
		for _, cidr := range l.AllowFrom {
			if _, _, err := net.ParseCIDR(cidr); err != nil {
				return fmt.Errorf("config: listen[%d].allow_from has invalid CIDR %q: %w", i, cidr, err)
			}
		}
		// Overlap detection: a wildcard bind claims the whole port, so it
		// conflicts with any other listener on that port (e.g. 0.0.0.0 vs a
		// loopback bind). Specific addresses conflict only on an exact repeat.
		if wildcard {
			if wildcardPorts[l.Port] || hasAnyOnPort(specificKeys, l.Port) {
				return fmt.Errorf("config: listen[%d] binds all interfaces on port %d, conflicting with another listener on that port", i, l.Port)
			}
			wildcardPorts[l.Port] = true
		} else {
			if wildcardPorts[l.Port] {
				return fmt.Errorf("config: listen[%d] %q conflicts with an all-interfaces listener on port %d", i, l.Addr, l.Port)
			}
			key := l.BindAddr()
			if specificKeys[key] {
				return fmt.Errorf("config: listen[%d] duplicate bind address %s", i, key)
			}
			specificKeys[key] = true
		}
	}
	return nil
}

// hasAnyOnPort reports whether any recorded specific bind key uses the port.
func hasAnyOnPort(keys map[string]bool, port int) bool {
	suffix := ":" + strconv.Itoa(port)
	for k := range keys {
		if strings.HasSuffix(k, suffix) {
			return true
		}
	}
	return false
}

// Warnings returns non-fatal advisories about the config. main logs these at
// startup. A listener serving the web UI but not the API is the common case:
// the page would load but its same-origin fetches would 404.
func (c Config) Warnings() []string {
	var w []string
	for i, l := range c.Listen {
		if l.Serves(GroupWebUI) && !l.Serves(GroupAPI) {
			w = append(w, fmt.Sprintf("listen[%d] serves %q without %q; the web UI would load but its API calls would 404", i, GroupWebUI, GroupAPI))
		}
	}
	return w
}
