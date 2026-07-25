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
	"time"
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

// Share depth (F5) — how far along the friendship chain a recording travels.
// Stored per recording (recordings.share_depth, migration 030; NULL = inherit
// the node default) and compared against an [Audience]'s Distance: content is
// visible to an audience iff depth >= Distance. See
// docs/architecture/federation.md §Sharing scope.
const (
	// DepthPrivate keeps content off the network entirely — not even a direct
	// friend sees it. Distance is never negative, so depth >= Distance is false
	// for every requester.
	DepthPrivate = -1
	// DepthFriends shares with direct friends only (distance 0).
	DepthFriends = 0
	// DepthUnlimited is ∞, the whole reachable madnetwork — a concrete large
	// integer rather than a NULL sentinel so the comparison stays a plain >= in
	// SQL and on the wire, with no special case to forget in one of them.
	DepthUnlimited = 1 << 20
)

// ValidDepth reports whether d is a share depth this node accepts from an admin
// or a peer: private, or any non-negative number of hops up to ∞.
func ValidDepth(d int) bool { return d >= DepthPrivate && d <= DepthUnlimited }

// Audience is who a mesh request is answered for (F5). The same value decides
// what the catalog lists and what the byte endpoints serve — a node must never
// advertise what it would not serve, so both halves read one rule.
//
// Until transitive reach turns on (F7) every authenticated requester is at
// distance 0; the field exists so depth > 0 needs no protocol or schema change
// then, and so the depth ladder is inert by construction rather than by
// omission.
type Audience struct {
	// Distance is the friendship hops to the requester: 0 = a direct friend.
	Distance int
	// GuestOnly limits the audience to guest-accessible recordings (the
	// guest-playable / license policy). True for a friend mapped to a local
	// account without content.access, and for the open swarm's strangers.
	GuestOnly bool
}

// FriendAudience is the audience of an unmapped direct friend: the default
// regular-user identity, which sees the whole published set
// (docs/architecture/federation.md §Principals & access).
var FriendAudience = Audience{Distance: DepthFriends}

// GuestAudience is the audience of a mesh node with no friendship at all — the
// open swarm. It reaches guest-accessible content only, and only at the byte
// endpoints: catalog and holdings stay friends-only.
var GuestAudience = Audience{Distance: DepthFriends, GuestOnly: true}

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

	// PublishedCatalog is this node's own catalog for one audience — every
	// approved, live appearance the audience's scope admits, with its
	// recording's renditions, in a stable order (the snapshot serial is a hash
	// over it, so each audience has its own serial).
	PublishedCatalog(ctx context.Context, aud Audience) ([]CatalogEntry, error)
	// ReplacePeerCatalog atomically replaces the cached copy of a friend's
	// catalog with a fresh snapshot and records its serial + sync time.
	ReplacePeerCatalog(ctx context.Context, peerID int64, serial string, syncedAt int64, entries []CatalogEntry) error
	// MarkPeerCatalogChecked records a sync round that found the cached copy
	// still fresh (the not-modified path).
	MarkPeerCatalogChecked(ctx context.Context, peerID int64, serial string, syncedAt int64) error

	// BlobVisibleTo reports whether the blob with this content hash may be
	// served to aud: part of the published library (live file + an approved
	// appearance on its recording) *and* inside the audience's scope — the same
	// rule the catalog above answers by, so the node never advertises what it
	// would not serve (database/madnetwork_scope.go).
	BlobVisibleTo(ctx context.Context, hash string, aud Audience) (visible, found bool, err error)
	// PeerAudience resolves a known peer to the audience its requests are
	// answered for (the user mapping, §Principals & access).
	PeerAudience(ctx context.Context, peerID int64) (Audience, error)
	// MadnetworkBlobProviders returns the friends who hold hash — the swarm
	// tracker (F4): the union of catalog (library) holders and holdings (cache)
	// holders, most recently seen first (the fetch order) — plus the advertised
	// byte size (a hint; the origin's Content-Length / manifest wins).
	MadnetworkBlobProviders(ctx context.Context, hash string) (size int64, holders []*Peer, err error)
	// ReplacePeerHoldings atomically replaces the cached list of what one friend
	// holds in its download cache and will seed (F4 holdings sync).
	ReplacePeerHoldings(ctx context.Context, peerID int64, hashes []string) error
	// SeedingPolicy reports whether this node serves blobs to friends at all and
	// whether it also seeds its download cache — the F4 serving gate (both
	// default on).
	SeedingPolicy(ctx context.Context) (enabled, cache bool, err error)
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
	// Progress is the number of bytes readable as a contiguous prefix from the
	// front of the file (the swarm's watermark). For a status/progress readout.
	Progress() int64
	// Available reports how many bytes are readable contiguously starting at
	// offset right now (0 if that offset is not yet fetched). Unlike Progress,
	// it accounts for out-of-order chunks — a prioritized tail/seek read can be
	// served before the middle of the file arrives — so the streaming relay
	// uses it to bound each read.
	Available(offset int64) int64
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
	// Stats is a diagnostic snapshot of how the fetch is going — which holders
	// carried it, how often it retried or failed over, when the first byte
	// landed. Safe to call at any point in the transfer's life.
	Stats() TransferStats
}

// TransferStats is a point-in-time snapshot of one blob fetch: enough to answer
// *how* it went, not just whether it finished. The swarm's load-bearing claims
// (multi-source failover, seek priority, the chunk-0 prefetch overlap) are only
// assertable against numbers like these — see docs/plans/mesh-testing.md T1 —
// and the same numbers are what an admin transfer view would show.
type TransferStats struct {
	Hash string `json:"hash"`
	// Mode is how the bytes are being fetched: "local" (born complete from the
	// library or the cache), "swarm" (F4 multi-source chunks) or "whole" (the F3
	// single-source fallback). Empty before the fetch picks a path.
	Mode string `json:"mode"`
	Size int64  `json:"size"`
	// Progress is the contiguous readable prefix (Transfer.Progress).
	Progress int64 `json:"progress"`
	// Elapsed is wall-clock since the fetch started (frozen once it ended).
	Elapsed time.Duration `json:"elapsed_ns"`
	// FirstByte is how long it took the front of the file to become readable —
	// the streaming time-to-first-byte. 0 means "not yet"; a failed attempt that
	// resets progress clears it, so it always describes the live attempt.
	FirstByte time.Duration `json:"first_byte_ns"`

	Chunks     int `json:"chunks"`      // chunks in the manifest layout (0 in whole-file mode)
	ChunksDone int `json:"chunks_done"` // chunks verified and written
	Retries    int `json:"retries"`     // failed attempts that were re-queued
	Failovers  int `json:"failovers"`   // pieces completed by a holder after another holder failed them
	Stalls     int `json:"stalls"`      // idle-read watchdog firings (a hung mesh connection)
	Corrupt    int `json:"corrupt"`     // per-chunk verification failures

	// Providers is per-holder accounting, in the order the tracker offered them.
	Providers []ProviderStats `json:"providers"`

	// Prior holds the attempts this transfer abandoned before the live one —
	// today just the swarm phase of a swarm→whole-file fallback. The fields
	// above describe only the attempt still running, which is right for a live
	// transfer and useless for a failed one: without this, a fetch that fetched
	// half the chunks and then fell back reports mode=whole chunks=0/0, hiding
	// the phase that actually failed.
	Prior []AttemptStats `json:"prior,omitempty"`
}

// AttemptStats is what one abandoned fetch attempt achieved before giving way to
// the next. The cumulative counters (retries, failovers, stalls, corrupt and the
// per-provider rows) are NOT split per attempt — they are transfer-wide history
// and stay in [TransferStats].
type AttemptStats struct {
	Mode       string        `json:"mode"`
	FirstByte  time.Duration `json:"first_byte_ns"`
	Chunks     int           `json:"chunks"`
	ChunksDone int           `json:"chunks_done"`
}

// ProviderStats is one holder's contribution to a transfer.
type ProviderStats struct {
	Name      string `json:"name"`
	PublicKey string `json:"public_key"`
	Bytes     int64  `json:"bytes"`
	Chunks    int    `json:"chunks"`
	Failures  int    `json:"failures"`
	Dropped   bool   `json:"dropped"` // taken out of rotation (corrupt bytes, or too many failures)
	LastError string `json:"last_error,omitempty"`
}

// Option configures Start: the transfer wiring (cache dir, blob resolver) and
// the test/lab seams (intervals, timeouts). Both build variants accept options;
// the stub ignores them.
type Option func(*nodeOptions)

type nodeOptions struct {
	cacheDir    string
	resolveBlob func(hash string) (path string, ok bool)
	intervals   Intervals
	timeouts    Timeouts
}

// Intervals overrides the node's background cadences. A zero field keeps the
// built-in default, so a caller sets only what it cares about.
//
// This is a test/lab seam (docs/plans/mesh-testing.md T1): the production values
// are tuned for a quiet mesh, but a chaos scenario cannot wait out a 15-minute
// catalog sync and a multi-node demo whose catalogs converge on that cadence is
// unusable. Nothing in the server sets it.
type Intervals struct {
	Refresh     time.Duration // refresh-loop sweep period (pair retries + friend pings); default 1 min
	CatalogSync time.Duration // how stale a friend's cached catalog may get before a re-pull; default 15 min
	SnapshotTTL time.Duration // how long this node's own catalog snapshot is memoized; default 1 min
}

// WithIntervals overrides the background cadences (zero fields keep defaults).
func WithIntervals(iv Intervals) Option { return func(o *nodeOptions) { o.intervals = iv } }

// Timeouts overrides the deadlines on the protocol and transfer paths. A zero
// field keeps the built-in default.
//
// The same seam as [Intervals], for the other half of the problem: a stall
// scenario that waits out the production 2-minute per-chunk backstop takes
// minutes to assert a fact that happens in the first second.
type Timeouts struct {
	Control    time.Duration // one control call (ping, catalog, holdings); default 15 s
	Manifest   time.Duration // one manifest probe against a holder; default 20 s
	ChunkStall time.Duration // idle-read watchdog: no bytes for this long ⇒ the connection is hung; default 20 s
	PerChunk   time.Duration // overall backstop for one chunk fetch; default 2 min
	Transfer   time.Duration // overall backstop for one whole-file fetch; default 30 min
}

// WithTimeouts overrides the protocol/transfer deadlines (zero fields keep
// defaults).
func WithTimeouts(to Timeouts) Option { return func(o *nodeOptions) { o.timeouts = to } }

// withDefaults fills every unset (non-positive) field from d.
func (iv Intervals) withDefaults(d Intervals) Intervals {
	if iv.Refresh <= 0 {
		iv.Refresh = d.Refresh
	}
	if iv.CatalogSync <= 0 {
		iv.CatalogSync = d.CatalogSync
	}
	if iv.SnapshotTTL <= 0 {
		iv.SnapshotTTL = d.SnapshotTTL
	}
	return iv
}

// withDefaults fills every unset (non-positive) field from d.
func (to Timeouts) withDefaults(d Timeouts) Timeouts {
	if to.Control <= 0 {
		to.Control = d.Control
	}
	if to.Manifest <= 0 {
		to.Manifest = d.Manifest
	}
	if to.ChunkStall <= 0 {
		to.ChunkStall = d.ChunkStall
	}
	if to.PerChunk <= 0 {
		to.PerChunk = d.PerChunk
	}
	if to.Transfer <= 0 {
		to.Transfer = d.Transfer
	}
	return to
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
