// Package federation embeds a madnetwork node: a yggdrasil mesh identity plus
// the mesh-side federation protocol listener, kept entirely in-process (gVisor
// userspace netstack, no TUN, no root). This is the F0 skeleton — identity,
// transport, and a protocol ping; friendship, catalog, and transfer arrive in
// later milestones. Design: docs/architecture/federation.md.
//
// The package compiles out with -tags nofederation (see node_stub.go), which
// also drops the yggdrasil and gVisor dependencies from the binary; main
// refuses to start such a build with federation.enabled set.
package federation

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// MeshPort is the fixed TCP port of the madnetwork protocol listener on every
// node's mesh address (the port lives inside the embedded netstack, so it can
// never collide with anything on the host). 1314 spells MAD (M=13, A=1, D=4).
const MeshPort = 1314

// ProtocolVersion is the madnetwork protocol generation, exchanged in ping so
// incompatible peers can refuse each other early.
const ProtocolVersion = 0

// Peer states (federation_peers.state, migration 026). The friendship state
// machine — see docs/architecture/federation.md §Trust graph and the pairing
// handshake in node.go:
//
//	pending_outgoing → friend   when the peer's node confirms (their admin
//	                            imported our card, or accepted our request)
//	pending_incoming → friend   when our admin accepts
//	any              → blocked  by admin action; prev_state remembers where an
//	                            unblock returns to (local effect only in F1)
const (
	PeerPendingOutgoing = "pending_outgoing"
	PeerPendingIncoming = "pending_incoming"
	PeerFriend          = "friend"
	PeerBlocked         = "blocked"
)

// ErrPeerNotFound is returned by peer lookups when no row matches.
var ErrPeerNotFound = errors.New("federation peer not found")

// ErrNoHolder marks a blob fetch that cannot start because no friend's cached
// catalog advertises the hash. The API maps it to 404.
var ErrNoHolder = errors.New("no friend holds this content")

// ErrPeerState marks an operation refused because the peer is in the wrong
// state (accepting a non-pending peer, importing a blocked node's card, …).
// The admin API maps it to 409.
var ErrPeerState = errors.New("invalid peer state for this operation")

// Peer is a row of the trusted-peer table plus display decorations: Username is
// the mapped local account's name (joined by the store), Address the mesh IPv6
// derived from PublicKey (filled by the running node — key derivation lives in
// the !nofederation build).
type Peer struct {
	ID        int64  `json:"id"`
	PublicKey string `json:"public_key"` // lowercase hex ed25519
	Name      string `json:"name"`
	State     string `json:"state"`
	PrevState string `json:"-"`
	UserID    *int64 `json:"user_id"`
	CreatedAt int64  `json:"created_at"`
	LastSeen  int64  `json:"last_seen"` // unix seconds; 0 = never

	// Catalog sync state (F2, pull-and-cache): the serial of the last snapshot
	// applied from this friend and when it was last confirmed fresh.
	CatalogSerial   string `json:"-"`
	CatalogSyncedAt int64  `json:"catalog_synced_at"`

	Username string `json:"username,omitempty"` // mapped account, display only
	Address  string `json:"address,omitempty"`  // derived mesh address, display only
}

// PeerStore is the persistence the node needs: the trusted-peer table (F1) and
// the catalog — both what this node publishes to friends and the cached copies
// pulled from them (F2). *database.DB implements it (database/federation.go +
// database/madnetwork.go).
type PeerStore interface {
	ListFederationPeers(ctx context.Context) ([]*Peer, error)
	GetFederationPeer(ctx context.Context, id int64) (*Peer, error)
	GetFederationPeerByKey(ctx context.Context, publicKey string) (*Peer, error)
	InsertFederationPeer(ctx context.Context, p *Peer) (int64, error)
	SetFederationPeerState(ctx context.Context, id int64, state, prevState string) error
	UpdateFederationPeerName(ctx context.Context, id int64, name string) error
	SetFederationPeerUser(ctx context.Context, id int64, userID *int64) error
	TouchFederationPeerSeen(ctx context.Context, id int64, when int64) error
	DeleteFederationPeer(ctx context.Context, id int64) error

	// PublishedCatalog is this node's own catalog — every approved, live
	// appearance with its recording's renditions — in a stable order (the
	// snapshot serial is a hash over it).
	PublishedCatalog(ctx context.Context) ([]CatalogEntry, error)
	// ReplacePeerCatalog atomically replaces the cached copy of a friend's
	// catalog with a fresh snapshot and records its serial + sync time.
	ReplacePeerCatalog(ctx context.Context, peerID int64, serial string, syncedAt int64, entries []CatalogEntry) error
	// MarkPeerCatalogChecked records a sync round that found the cached copy
	// still fresh (the not-modified path).
	MarkPeerCatalogChecked(ctx context.Context, peerID int64, serial string, syncedAt int64) error

	// BlobPubliclyVisible reports whether the blob with this content hash is
	// part of the published library (live file + an approved appearance on its
	// recording) — the F3 blob-serving gate, the same predicate that governs
	// the local library and the catalog (database/review.go).
	BlobPubliclyVisible(ctx context.Context, hash string) (visible, found bool, err error)
	// MadnetworkBlobProviders returns the friends whose cached catalogs
	// advertise hash — most recently seen first, the fetch order — plus the
	// advertised byte size (a hint; the origin's Content-Length wins).
	MadnetworkBlobProviders(ctx context.Context, hash string) (size int64, holders []*Peer, err error)
}

// Transfer is one in-flight or completed blob fetch (federation F3). Readers
// follow the growing cache file: WaitFor blocks until an offset is readable,
// so the API can relay bytes to a browser while the download continues
// (cache-through streaming — never download-fully-then-play). All methods are
// safe for concurrent use.
type Transfer interface {
	Hash() string
	// Size is the expected total in bytes (the origin's Content-Length once
	// the fetch started; before that the catalog's advertised size). Stable
	// after WaitFor(ctx, 0) returns.
	Size() int64
	// Filename is the origin's on-disk filename (from Content-Disposition);
	// may be empty. Stable after WaitFor(ctx, 0) returns.
	Filename() string
	// Progress is the number of bytes readable from the cache file so far.
	Progress() int64
	// Done is closed when the transfer finished — verified and renamed into
	// the cache on success, or failed (Err non-nil).
	Done() <-chan struct{}
	// Err is the terminal error; valid once Done is closed.
	Err() error
	// Open opens the underlying file for reading (the partial file while the
	// fetch runs; the verified cache file — or a local blob — when complete).
	Open() (*os.File, error)
	// WaitFor blocks until at least offset+1 bytes are readable, the transfer
	// ends, or ctx is done. An offset at or beyond EOF returns io.EOF.
	WaitFor(ctx context.Context, offset int64) error
}

// Option configures Start with the F3 transfer wiring. Both build variants
// accept options; the stub ignores them.
type Option func(*nodeOptions)

type nodeOptions struct {
	cacheDir    string
	resolveBlob func(hash string) (path string, ok bool)
}

// WithCacheDir sets the directory for fetched blobs (<data_dir>/cache/madnetwork
// in the running server). Without it the node cannot fetch remote blobs.
func WithCacheDir(dir string) Option { return func(o *nodeOptions) { o.cacheDir = dir } }

// WithBlobResolver wires the local blob lookup (the storages registry): the
// serving side resolves published hashes to on-disk paths with it, and a fetch
// of a hash the library already holds short-circuits to the local copy.
func WithBlobResolver(f func(hash string) (path string, ok bool)) Option {
	return func(o *nodeOptions) { o.resolveBlob = f }
}

// CatalogRendition is one blob of a recording as advertised in a catalog
// entry: the content hash (the future swarm id, F4) plus the quality facts the
// ladder ranks by. Remote values are hints — bytes are verified against the
// hash and fingerprinted locally on any future download (F3).
type CatalogRendition struct {
	Hash       string  `json:"hash"`
	Size       int64   `json:"size"`
	Codec      string  `json:"codec,omitempty"`
	Bitrate    int64   `json:"bitrate,omitempty"`
	SampleRate int64   `json:"sample_rate,omitempty"`
	BitDepth   int64   `json:"bit_depth,omitempty"`
	Duration   float64 `json:"duration,omitempty"`
}

// CatalogEntry is one published appearance (tagset) of a recording, as carried
// by the catalog protocol and cached per peer. Key and RecordingKey are the
// origin node's stable ids — opaque strings here (never joined onto local
// entities; remote claims are hints).
type CatalogEntry struct {
	Key           string             `json:"key"`
	RecordingKey  string             `json:"recording_key"`
	Title         string             `json:"title"`
	Artist        string             `json:"artist,omitempty"`
	AlbumArtist   string             `json:"album_artist,omitempty"`
	Album         string             `json:"album,omitempty"`
	Genre         string             `json:"genre,omitempty"`
	Year          *int64             `json:"year,omitempty"`
	TrackNumber   *int64             `json:"track_number,omitempty"`
	DiscNumber    *int64             `json:"disc_number,omitempty"`
	Duration      float64            `json:"duration,omitempty"`
	License       string             `json:"license,omitempty"`
	GuestPlayable bool               `json:"guest_playable,omitempty"`
	Renditions    []CatalogRendition `json:"renditions"`
}

// CatalogSerial is the deterministic serial of a snapshot: the SHA-256 (hex)
// of its canonical JSON. Two identical catalogs — regardless of when or where
// serialized — get the same serial, which is all the not-modified check needs.
func CatalogSerial(entries []CatalogEntry) string {
	raw, _ := json.Marshal(entries)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Card is a node card — the out-of-band introduction two admins exchange to
// friend their nodes (copy-paste JSON). It deliberately carries only identity:
// underlay connectivity is [federation] config's business (public mesh or
// explicit peers/listen), not the card's. Version is the protocol generation
// (field name "madshare_node_card" doubles as the format marker).
type Card struct {
	Version   int    `json:"madshare_node_card"`
	Name      string `json:"name,omitempty"`
	PublicKey string `json:"public_key"`
}

// NodeInfo is the running node's own identity as shown on the admin page.
type NodeInfo struct {
	Name      string `json:"name"`
	Address   string `json:"address"`
	PublicKey string `json:"public_key"`
	Card      Card   `json:"card"`
}

// NormalizeKey validates a hex-encoded ed25519 public key and returns it
// lowercased, the canonical form stored and compared everywhere.
func NormalizeKey(hexKey string) (string, error) {
	k := strings.ToLower(strings.TrimSpace(hexKey))
	raw, err := hex.DecodeString(k)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return "", errors.New("public key must be 64 hex characters (ed25519)")
	}
	return k, nil
}

// ParseCard validates raw JSON as a node card. It tolerates unknown fields
// (future card versions may add some) but requires the format marker and a
// well-formed key; the name is trimmed and length-capped.
func ParseCard(raw []byte) (Card, error) {
	var c Card
	if err := json.Unmarshal(raw, &c); err != nil {
		return Card{}, errors.New("not a node card (invalid JSON)")
	}
	if c.Version != ProtocolVersion {
		return Card{}, fmt.Errorf("unsupported node card version %d (this node speaks %d)", c.Version, ProtocolVersion)
	}
	key, err := NormalizeKey(c.PublicKey)
	if err != nil {
		return Card{}, err
	}
	c.PublicKey = key
	c.Name = CleanPeerName(c.Name)
	return c, nil
}

// CleanPeerName trims and length-caps a peer-supplied display name (cards and
// pair requests are remote input).
func CleanPeerName(name string) string {
	name = strings.TrimSpace(name)
	if len(name) > 100 {
		name = name[:100]
	}
	return name
}
