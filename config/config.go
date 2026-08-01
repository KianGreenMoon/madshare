package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// Route group tokens accepted in a listener's `serve` list. See
// docs/architecture/listeners-and-config.md for what each group covers.
const (
	GroupAPI   = "api"   // /, /api/* (non-admin), /files/*, /images/*
	GroupWebUI = "webui" // /, /static/*
	GroupAdmin = "admin" // /api/admin/* and the /admin page
)

// knownGroups is the set of valid `serve` tokens.
var knownGroups = map[string]bool{
	GroupAPI:   true,
	GroupWebUI: true,
	GroupAdmin: true,
}

type Config struct {
	// DataDir is the default root under which the database, uploaded files, and
	// derived media live. database.path, storage.files_dir and storage.variants_dir
	// are derived from it when unset (<data_dir>/madshare.db, <data_dir>/files,
	// <data_dir>/variants); each may be overridden independently. The default
	// "./data" reproduces the historical layout. See
	// docs/architecture/data-sources.md and docs/architecture/variants.md.
	DataDir string `toml:"data_dir"`

	Listen     []ListenConfig   `toml:"listen"`
	WebUI      WebUIConfig      `toml:"webui"`
	Database   DatabaseConfig   `toml:"database"`
	Storage    StorageConfig    `toml:"storage"`
	Auth       AuthConfig       `toml:"auth"`
	CORS       CORSConfig       `toml:"cors"`
	Sources    SourcesConfig    `toml:"sources"`
	Federation FederationConfig `toml:"federation"`

	// warnings accumulates non-fatal advisories produced while loading (e.g.
	// clamped out-of-range worker counts). It is unexported so it is not a TOML
	// field; Warnings() returns it alongside the listener-derived advisories.
	warnings []string
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
	// GitRepo is the URL behind the web UI's "GitRepo" nav button. A pointer so
	// the three states are distinguishable: absent (nil) → DefaultGitRepoURL,
	// empty string → the button is hidden, anything else → used as-is.
	GitRepo *string `toml:"git_repo"`
}

// DefaultGitRepoURL is the project's upstream repository, linked from the web
// UI's GitRepo nav button when [webui].git_repo is not set.
const DefaultGitRepoURL = "https://github.com/KianGreenMoon/madshare"

// GitRepoURL resolves [webui].git_repo per the WebUIConfig.GitRepo contract.
// An empty return value means "hide the button".
func (w WebUIConfig) GitRepoURL() string {
	if w.GitRepo == nil {
		return DefaultGitRepoURL
	}
	return strings.TrimSpace(*w.GitRepo)
}

// CORSConfig controls cross-origin access to the API. AllowedOrigins is the
// browser-origin allow-list emitted as Access-Control-Allow-Origin. Empty (the
// default) emits no CORS headers — the bundled web UI is same-origin and needs
// none, and non-browser clients (API tokens, curl) ignore CORS. A single "*"
// allows any origin (echoed as "*", without credentials). Otherwise list exact
// origins as scheme://host[:port]; a matching request's Origin is echoed back
// and may carry credentials. See docs/architecture/listeners-and-config.md.
type CORSConfig struct {
	AllowedOrigins []string `toml:"allowed_origins"`
}

// SourcesConfig controls in-place symlink imports (see
// docs/architecture/data-sources.md). SymlinkRoots is the operator-set allow-list
// of external directories a 'symlink' data source may reference; it is a trust
// boundary and intentionally deploy-time TOML only (not UI-editable). Empty (the
// default) disables the symlink source kind entirely — the local storage and
// uploads are unaffected. Each entry must be an absolute, Clean-stable path.
type SourcesConfig struct {
	SymlinkRoots []string `toml:"symlink_roots"`
}

// SymlinkSourcesEnabled reports whether any symlink root is configured. When
// false the symlink source kind is disabled: POST /api/admin/sources is refused
// and the Add form is hidden.
func (s SourcesConfig) SymlinkSourcesEnabled() bool {
	return len(s.SymlinkRoots) > 0
}

type DatabaseConfig struct {
	Path string `toml:"path"`
}

// FederationConfig controls the embedded madnetwork node (the yggdrasil mesh
// identity + federation listener; see docs/architecture/federation.md).
// Disabled by default. Enabling it requires a binary built without
// -tags nofederation (main enforces this via federation.Available).
type FederationConfig struct {
	// Enabled starts the embedded yggdrasil node and the mesh-side federation
	// listener at startup.
	Enabled bool `toml:"enabled"`
	// Name is the human-readable node name shown on node cards and to peers
	// during pairing. Defaults to the host name when unset. Purely descriptive —
	// identity is the key, never the name.
	Name string `toml:"name"`
	// KeyFile is the path of the PEM ed25519 private key that IS the node's
	// madnetwork identity — its mesh address derives from this key, so losing
	// the file means a new identity. Derived as <data_dir>/federation.key when
	// unset; created (0600) on first federated start.
	KeyFile string `toml:"key_file"`
	// Peers are outbound underlay peering URIs (e.g. "tls://host:port") this
	// node dials to join the mesh.
	Peers []string `toml:"peers"`
	// Listen are underlay listener URIs (e.g. "tls://0.0.0.0:12345") accepting
	// incoming peerings — for backbone nodes; outbound-only nodes leave it empty.
	Listen []string `toml:"listen"`
	// SeedRateKiB caps the outbound rate, in KiB/s, at which this node serves
	// blobs to friends over the swarm (federation F4) — a token bucket over the
	// blob-serve write path. 0 (the default) is unlimited; negative is clamped
	// to 0 with a warning. The seed on/off and cache-seed toggles are runtime DB
	// settings (madnetwork.seed_enabled / .seed_cache), not config.
	SeedRateKiB int `toml:"seed_rate_kib"`
	// Member quotas (F7 item 6, docs/architecture/federation.md §Distribution,
	// "What a member may cost us"). SeedRateKiB above is one bucket for every
	// requester, which was the whole policy while a requester was always a
	// friend. This node now serves its entire community, and membership has no
	// admission cap by design — so what needs bounding is not who asks but what
	// one of them can cost.
	//
	// These four apply to **non-friends only**: members, guests, and a pending
	// peer nobody has accepted. A direct friend is an admin's decision and is
	// served under the global cap alone, which is the anti-starvation rule as
	// much as the anti-abuse one — the nodes an admin chose must never queue
	// behind the ones the graph let in.
	//
	// Each resource is bounded twice, and both halves are needed: the
	// per-requester limit is fairness *within* the class, while the class
	// ceiling is the actual bound on harm, since N forged keys would otherwise
	// buy N per-requester quotas.
	//
	// All default to 0 = unlimited (owner decision, 2026-08-01: opt-in, because a
	// handful of friends wants none of this and a guessed default would tax the
	// common case). Negative is clamped to 0 with a warning.

	// MemberRateKiB caps the outbound rate, in KiB/s, served to all non-friends
	// combined.
	MemberRateKiB int `toml:"member_rate_kib"`
	// PerMemberRateKiB caps the outbound rate, in KiB/s, served to any one
	// non-friend node.
	PerMemberRateKiB int `toml:"per_member_rate_kib"`
	// MemberMaxTransfers caps how many blob requests from non-friends this node
	// serves at once, across all of them. Concurrency is the sharper of the two
	// resources: a swarm client opens parallel Range requests by design, so this
	// is what a member most easily costs us in goroutines, file handles and
	// netstack connections.
	MemberMaxTransfers int `toml:"member_max_transfers"`
	// PerMemberMaxTransfers caps concurrent blob requests from any one
	// non-friend node.
	PerMemberMaxTransfers int `toml:"per_member_max_transfers"`
	// AllowMissingFingerprinting lets a federated node start without fpcalc on
	// PATH. Default false — i.e. fpcalc is REQUIRED once federation is enabled,
	// and main refuses to start otherwise.
	//
	// It is the one analysis tool federation cannot do without: downloaded bytes
	// are re-fingerprinted locally before they join a recording, because remote
	// claims are hints and never facts (docs/architecture/federation.md
	// §Catalog). Without it that verification silently does not happen — fetched
	// audio can never group with what this node already holds, and a mislabelled
	// tagset has no true recording to land on as a visible minority label, which
	// is the first layer of the anti-mislabel defense. The damage is to peers'
	// trust in this node's catalog, not merely to local convenience, hence a
	// refusal rather than a warning.
	//
	// ffprobe is deliberately NOT gated: without it renditions carry no quality
	// facts and the ladder degrades to format+size, which is worse output, not
	// unverified input.
	//
	// The polarity is chosen so the zero value is the safe one: an absent key
	// means "required", with no defaulting step to forget.
	AllowMissingFingerprinting bool `toml:"allow_missing_fingerprinting"`
	// ReachableWindowSec is the madnetwork availability freshness window: a friend
	// counts as reachable (its exclusively-held tracks are shown in the browse)
	// when last_seen is within this many seconds. Several × the 1-minute refresh
	// cadence, so a single missed ping never flips it — the margin is the anti-flap
	// guarantee. 0 (the default) → DefaultReachableWindowSec; a positive value
	// below MinReachableWindowSec is clamped up with a warning. Whether hiding is
	// applied at all is the runtime toggle madnetwork.hide_unavailable.
	ReachableWindowSec int `toml:"reachable_window_sec"`
	// DiscoveryBudget is how many *members'* catalogs this node pulls per catalog
	// cycle, beyond its friends — the bounded frontier of F7 item 5
	// (docs/architecture/federation.md §Discovery beyond the friend ring).
	// Friends are always pulled and never counted here.
	//
	// 0 (the default) → DefaultDiscoveryBudget. **-1 turns discovery off**: the
	// node still serves its whole community, it simply stops looking past its own
	// friends, which is what a node on a metered link or a deliberately inward
	// deployment wants. Off is spelled -1 rather than 0 so that an unset key can
	// keep meaning "the default" — the same reason 0 means unlimited above.
	DiscoveryBudget int `toml:"discovery_budget"`
	// DiscoveryCap is the largest number of foreign (non-peer) catalogs kept
	// cached; past it the least-recently-seen are dropped. A cached catalog is a
	// droppable cache — rebuildable from the network, referenced by nothing local
	// — so this is a disk-and-query-size knob, not a correctness one. 0 (the
	// default) → DefaultDiscoveryCap; negative is clamped with a warning.
	DiscoveryCap int `toml:"discovery_cap"`
}

// Madnetwork availability freshness-window bounds (federation.reachable_window_sec).
const (
	DefaultReachableWindowSec = 180 // 3× the 1-minute refresh cadence
	MinReachableWindowSec     = 120 // 2× cadence — the floor that keeps it from flapping
)

// Frontier-pull bounds (federation.discovery_budget / .discovery_cap). Policy
// numbers, not derivations: they trade how fast the community's libraries become
// visible against a cost that must not grow with the community's size. See
// federation.Discovery for the reasoning and docs/architecture/federation.md
// §Open questions 2 for what would change them.
const (
	DefaultDiscoveryBudget = 4   // member catalogs per cycle — ~16 nodes an hour
	DefaultDiscoveryCap    = 200 // foreign catalogs kept
)

// federationSchemes are the underlay URI schemes yggdrasil accepts. socks /
// sockstls are dial-only, hence the peersOnly flag.
var federationSchemes = map[string]struct{ peersOnly bool }{
	"tcp": {}, "tls": {}, "quic": {}, "ws": {}, "wss": {}, "unix": {},
	"socks": {peersOnly: true}, "sockstls": {peersOnly: true},
}

// MaxUploadMBLimit is the largest accepted value for max_upload_mb (1 TiB
// expressed in MiB). It is far below the point at which MaxUploadBytes would
// overflow int64, so MaxUploadBytes cannot wrap negative for any accepted
// config value.
const MaxUploadMBLimit = 1 << 20

type StorageConfig struct {
	// FilesDir is the directory where uploaded source blobs are stored and served
	// (audio under FilesDir/audio).
	FilesDir string `toml:"files_dir"`
	// VariantsDir is the directory holding owned, derived media — cover-image
	// variants under VariantsDir/images, with VariantsDir/cache reserved for the
	// future evictable audio-variant tier. Derived from data_dir
	// (<data_dir>/variants) when unset. See docs/architecture/variants.md.
	VariantsDir string `toml:"variants_dir"`
	// MaxUploadMB caps the size of a single upload request body, in MiB. It is
	// distinct from the in-memory hashing threshold (storage.memBufferLimit),
	// above which an upload is spooled to the spool dir rather than buffered.
	MaxUploadMB int64 `toml:"max_upload_mb"`
	// ServerMaxParallelWorkers caps concurrent uploads across all users.
	// 0 (the default) means unlimited. Negative values are clamped to 0.
	ServerMaxParallelWorkers int `toml:"server_max_parallel_workers"`
	// UserMaxParallelWorkers caps concurrent uploads per user. 0 means
	// unlimited; negative is clamped to 0. There is no admin bypass.
	UserMaxParallelWorkers int `toml:"user_max_parallel_workers"`
	// ImageProcessingWorkers sizes the CPU-bound cover-variant resize pool.
	// 0/unset resolves to runtime.NumCPU(); negative is treated as auto (warn).
	// Decoupled from upload concurrency on purpose (resize is CPU-bound).
	ImageProcessingWorkers int `toml:"image_processing_workers"`
}

// MaxUploadBytes returns the configured upload cap in bytes.
func (s StorageConfig) MaxUploadBytes() int64 {
	return s.MaxUploadMB << 20
}

// DefaultDataDir is the default root for derived data paths. "./data"
// reproduces the historical default layout (db + files under ./data).
const DefaultDataDir = "./data"

func defaults() Config {
	return Config{
		DataDir: DefaultDataDir,
		// Default: a single loopback listener serving the full stack. Loopback
		// is the safe default while the admin surface is unauthenticated.
		Listen: []ListenConfig{
			{
				Addr:  "127.0.0.1",
				Port:  3000,
				Serve: []string{GroupAPI, GroupWebUI, GroupAdmin},
			},
		},
		// Database.Path and Storage.FilesDir are intentionally left empty here;
		// resolveDataDir derives them from DataDir unless the file overrides them.
		Storage: StorageConfig{
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
	cfg.resolveDataDir()
	cfg.resolveStorageWorkers()
	cfg.resolveGitRepo()
	cfg.resolveSources()
	if err := cfg.validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// resolveDataDir derives the effective database and files paths from DataDir
// when the file did not set them explicitly. An explicit database.path /
// storage.files_dir takes precedence (independent overrides). When DataDir is
// empty nothing is derived — validate() reports it. Paths are joined with
// filepath.Join so a data_dir with a trailing slash or "." segments resolves
// cleanly.
func (c *Config) resolveDataDir() {
	if c.DataDir == "" {
		return
	}
	if c.Database.Path == "" {
		c.Database.Path = filepath.Join(c.DataDir, "madshare.db")
	}
	if c.Storage.FilesDir == "" {
		c.Storage.FilesDir = filepath.Join(c.DataDir, "files")
	}
	if c.Storage.VariantsDir == "" {
		c.Storage.VariantsDir = filepath.Join(c.DataDir, "variants")
	}
	if c.Federation.KeyFile == "" {
		c.Federation.KeyFile = filepath.Join(c.DataDir, "federation.key")
	}
}

// LinksDir returns the root of the shared "links" storage (the single dir of
// symlinks to externals), derived as <data_dir>/links. See
// docs/architecture/data-sources.md.
func (c Config) LinksDir() string {
	return filepath.Join(c.DataDir, "links")
}

// MadnetworkCacheDir is where blobs fetched from friends land (federation F3
// cache-through streaming; created on first fetch). Derived from data_dir like
// the links storage; no eviction in v1.
func (c Config) MadnetworkCacheDir() string {
	return filepath.Join(c.DataDir, "cache", "madnetwork")
}

// resolveGitRepo trims [webui].git_repo and warns (non-fatal) when a non-empty
// value doesn't look like an http(s) URL — the UI links to it verbatim.
func (c *Config) resolveGitRepo() {
	if c.WebUI.GitRepo == nil {
		return
	}
	v := strings.TrimSpace(*c.WebUI.GitRepo)
	*c.WebUI.GitRepo = v
	if v != "" && !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") {
		c.warnings = append(c.warnings, fmt.Sprintf(
			"webui.git_repo %q does not look like an http(s) URL; the GitRepo button will link to it as-is", v))
	}
}

// resolveSources records non-fatal advisories about the symlink-source
// allow-list: a configured root that does not currently exist (or is not a
// directory) is warned about, not rejected — the directory may be mounted later,
// and the hard format checks live in validateSources. Malformed entries (caught
// there) are skipped here so a single advisory does not pre-empt the fatal error.
func (c *Config) resolveSources() {
	for _, root := range c.Sources.SymlinkRoots {
		if !filepath.IsAbs(root) || filepath.Clean(root) != root {
			continue // validateSources turns this into a hard error
		}
		info, err := os.Stat(root)
		if err != nil {
			c.warnings = append(c.warnings, fmt.Sprintf(
				"sources.symlink_roots entry %q does not exist yet; symlink sources under it cannot be added until it appears", root))
			continue
		}
		if !info.IsDir() {
			c.warnings = append(c.warnings, fmt.Sprintf(
				"sources.symlink_roots entry %q is not a directory", root))
		}
	}
}

// resolveStorageWorkers normalises the worker-count fields and records any
// adjustment as a non-fatal warning (see Warnings). It runs on every load,
// including the all-defaults case, so the returned config always carries usable
// values. runtime.NumCPU() always returns >= 1.
func (c *Config) resolveStorageWorkers() {
	switch {
	case c.Storage.ImageProcessingWorkers == 0:
		c.Storage.ImageProcessingWorkers = runtime.NumCPU()
	case c.Storage.ImageProcessingWorkers < 0:
		c.warnings = append(c.warnings, fmt.Sprintf(
			"storage.image_processing_workers %d is invalid; using auto (NumCPU=%d)",
			c.Storage.ImageProcessingWorkers, runtime.NumCPU()))
		c.Storage.ImageProcessingWorkers = runtime.NumCPU()
	}
	if c.Storage.ServerMaxParallelWorkers < 0 {
		c.warnings = append(c.warnings, fmt.Sprintf(
			"storage.server_max_parallel_workers %d is invalid; using 0 (unlimited)",
			c.Storage.ServerMaxParallelWorkers))
		c.Storage.ServerMaxParallelWorkers = 0
	}
	if c.Storage.UserMaxParallelWorkers < 0 {
		c.warnings = append(c.warnings, fmt.Sprintf(
			"storage.user_max_parallel_workers %d is invalid; using 0 (unlimited)",
			c.Storage.UserMaxParallelWorkers))
		c.Storage.UserMaxParallelWorkers = 0
	}
	// The seed caps all share one rule — negative is meaningless, so it becomes
	// "unlimited" with a warning rather than an aborted start. Kept in one loop
	// so a knob added later cannot be forgotten here.
	for _, cap := range []struct {
		name string
		val  *int
	}{
		{"seed_rate_kib", &c.Federation.SeedRateKiB},
		{"member_rate_kib", &c.Federation.MemberRateKiB},
		{"per_member_rate_kib", &c.Federation.PerMemberRateKiB},
		{"member_max_transfers", &c.Federation.MemberMaxTransfers},
		{"per_member_max_transfers", &c.Federation.PerMemberMaxTransfers},
	} {
		if *cap.val < 0 {
			c.warnings = append(c.warnings, fmt.Sprintf(
				"federation.%s %d is invalid; using 0 (unlimited)", cap.name, *cap.val))
			*cap.val = 0
		}
	}
	switch {
	case c.Federation.ReachableWindowSec == 0:
		c.Federation.ReachableWindowSec = DefaultReachableWindowSec
	case c.Federation.ReachableWindowSec < MinReachableWindowSec:
		c.warnings = append(c.warnings, fmt.Sprintf(
			"federation.reachable_window_sec %d is below the anti-flap floor; using %d",
			c.Federation.ReachableWindowSec, MinReachableWindowSec))
		c.Federation.ReachableWindowSec = MinReachableWindowSec
	}
	switch {
	case c.Federation.DiscoveryBudget == 0:
		c.Federation.DiscoveryBudget = DefaultDiscoveryBudget
	case c.Federation.DiscoveryBudget < 0:
		// Any negative value is "off". Normalizing to -1 keeps the one sentinel
		// federation.Discovery understands, rather than passing an arbitrary
		// negative number down and hoping every consumer reads it the same way.
		c.Federation.DiscoveryBudget = -1
	}
	if c.Federation.DiscoveryCap < 0 {
		c.warnings = append(c.warnings, fmt.Sprintf(
			"federation.discovery_cap %d is invalid; using %d",
			c.Federation.DiscoveryCap, DefaultDiscoveryCap))
		c.Federation.DiscoveryCap = DefaultDiscoveryCap
	}
	if c.Federation.DiscoveryCap == 0 {
		c.Federation.DiscoveryCap = DefaultDiscoveryCap
	}
}

// validate checks that the config holds usable values. It is run by Load on
// every successfully-parsed config (including the all-defaults case). It
// returns only hard errors; soft advisories are reported by Warnings.
func (c Config) validate() error {
	if c.DataDir == "" {
		return errors.New("config: data_dir must not be empty")
	}
	if c.Database.Path == "" {
		return errors.New("config: database.path must not be empty")
	}
	if c.Storage.FilesDir == "" {
		return errors.New("config: storage.files_dir must not be empty")
	}
	if c.Storage.VariantsDir == "" {
		return errors.New("config: storage.variants_dir must not be empty")
	}
	if c.Storage.MaxUploadMB <= 0 {
		return errors.New("config: storage.max_upload_mb must be positive")
	}
	if c.Storage.MaxUploadMB > MaxUploadMBLimit {
		return fmt.Errorf("config: storage.max_upload_mb must not exceed %d", MaxUploadMBLimit)
	}
	if err := c.validateCORS(); err != nil {
		return err
	}
	if err := c.validateSources(); err != nil {
		return err
	}
	if err := c.validateFederation(); err != nil {
		return err
	}
	return c.validateListeners()
}

// validateFederation rejects malformed underlay URIs. Syntax is checked even
// when federation is disabled, so a typo does not lie dormant until the flag is
// flipped. A listen entry with a dial-only scheme (socks) is a hard error too.
func (c Config) validateFederation() error {
	check := func(field, uri string, listening bool) error {
		u, err := url.Parse(uri)
		if err != nil || u.Scheme == "" || (u.Host == "" && u.Scheme != "unix") {
			return fmt.Errorf("config: federation.%s has invalid URI %q (want scheme://host:port)", field, uri)
		}
		s, ok := federationSchemes[u.Scheme]
		if !ok {
			return fmt.Errorf("config: federation.%s URI %q has unknown scheme %q (valid: tcp, tls, quic, ws, wss, unix, socks, sockstls)", field, uri, u.Scheme)
		}
		if listening && s.peersOnly {
			return fmt.Errorf("config: federation.%s URI %q: scheme %q is dial-only and cannot be listened on", field, uri, u.Scheme)
		}
		return nil
	}
	for _, p := range c.Federation.Peers {
		if err := check("peers", p, false); err != nil {
			return err
		}
	}
	for _, l := range c.Federation.Listen {
		if err := check("listen", l, true); err != nil {
			return err
		}
	}
	return nil
}

// validateSources rejects a malformed symlink-source allow-list. Each entry is a
// trust boundary used to gate where a symlink import may point, so a relative or
// non-Clean path (which could be interpreted ambiguously at scan time) is a hard
// error, not an advisory. Existence is checked separately (resolveSources warns).
func (c Config) validateSources() error {
	for _, root := range c.Sources.SymlinkRoots {
		if root == "" {
			return errors.New("config: sources.symlink_roots has an empty entry")
		}
		if !filepath.IsAbs(root) {
			return fmt.Errorf("config: sources.symlink_roots entry %q must be an absolute path", root)
		}
		if filepath.Clean(root) != root {
			return fmt.Errorf("config: sources.symlink_roots entry %q is not Clean-stable (want %q)", root, filepath.Clean(root))
		}
	}
	return nil
}

// validateCORS rejects malformed allowed-origin entries. A silently non-matching
// origin would be a security footgun (the operator thinks they granted access),
// so a bad value is a hard error, like an invalid allow_from CIDR. Each entry
// must be "*" or an absolute scheme://host[:port] with no path/query/fragment.
func (c Config) validateCORS() error {
	for _, o := range c.CORS.AllowedOrigins {
		if o == "*" {
			continue
		}
		u, err := url.Parse(o)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") ||
			u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("config: cors.allowed_origins has invalid origin %q "+
				`(want "*" or scheme://host[:port])`, o)
		}
	}
	return nil
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
	w := append([]string(nil), c.warnings...)
	for i, l := range c.Listen {
		if l.Serves(GroupWebUI) && !l.Serves(GroupAPI) {
			w = append(w, fmt.Sprintf("listen[%d] serves %q without %q; the web UI would load but its API calls would 404", i, GroupWebUI, GroupAPI))
		}
	}
	// A separately hosted UI ([webui].api_base set) makes cross-origin browser
	// calls, which an empty CORS allow-list blocks. Flag the likely misconfig.
	if c.WebUI.APIBase != "" && len(c.CORS.AllowedOrigins) == 0 {
		w = append(w, "webui.api_base is set (separately hosted UI) but cors.allowed_origins is empty; "+
			"the browser UI's cross-origin API calls will be blocked — add its origin to [cors].allowed_origins")
	}
	if slices.Contains(c.CORS.AllowedOrigins, "*") && len(c.CORS.AllowedOrigins) > 1 {
		w = append(w, `cors.allowed_origins contains "*" plus specific origins; "*" already allows every origin, so the specific entries are redundant`)
	}
	// An enabled federation node with neither outbound peers nor an underlay
	// listener cannot reach the mesh at all (F0 has no multicast discovery).
	if c.Federation.Enabled && len(c.Federation.Peers) == 0 && len(c.Federation.Listen) == 0 {
		w = append(w, "federation.enabled is set but neither federation.peers nor federation.listen is configured; the node starts but is unreachable on the mesh")
	}
	return w
}
