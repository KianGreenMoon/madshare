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
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// MeshPort is the fixed TCP port of the madnetwork protocol listener on every
// node's mesh address (the port lives inside the embedded netstack, so it can
// never collide with anything on the host). 1314 spells MAD (M=13, A=1, D=4).
const MeshPort = 1314

// ProtocolVersion is the madnetwork protocol generation, exchanged in ping so
// incompatible peers can refuse each other early.
const ProtocolVersion = 0

// Freshness cadences (F7 item 10, docs/architecture/federation.md §Availability,
// "Two clocks, two windows"). These live here rather than beside the running
// node's Intervals because the *browse* has to know them too, and the browse is
// compiled into every build — including -tags nofederation, where the node is
// not.
const (
	// CatalogCycle is how often a source's cached catalog is re-pulled. Far
	// slower than the friendship ping on purpose: a library changes rarely,
	// liveness constantly.
	CatalogCycle = 15 * time.Minute

	// PullFreshnessWindow is the availability window for a node whose only
	// liveness clock IS that pull — a member no friend of ours vouches for.
	// Three cycles, the same 3× anti-flap margin reachable_window_sec carries
	// over the one-minute ping. Judging such a node by the ping's window is the
	// category error that hid most of the community's library.
	PullFreshnessWindow = 3 * CatalogCycle

	// MaxFreshnessHints bounds a ping reply's hint list. A node vouches only for
	// nodes it pings itself, so this is the friend-list bound — the same one
	// MaxGraphEdges puts on a published friend list, for the same reason.
	MaxFreshnessHints = MaxGraphEdges

	// MaxHintAge is how stale a first-hand observation may be and still be worth
	// relaying. Past it the claim cannot satisfy any receiver's ping window, so
	// sending it only costs bytes.
	MaxHintAge = time.Hour
)

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

// Sharing scope (F5, collapsed to three values in F7) — who a recording is
// published to. Stored per recording (recordings.share_depth, migration 030;
// NULL = inherit the node default) and compared against an [Audience]'s
// Distance: content is visible iff depth >= Distance. See
// docs/architecture/federation.md §Sharing scope.
//
// The constants still read as distances because that is how the SQL compares
// them, and keeping the encoding is what let the vocabulary change without a
// schema or protocol break. Their *names in the UI* are Local, Direct friends
// and Madnetwork.
const (
	// DepthPrivate ("Local") keeps content off the network entirely — not even a
	// direct friend sees it. Distance is never negative, so depth >= Distance is
	// false for every requester.
	DepthPrivate = -1
	// DepthFriends ("Direct friends") shares with the nodes this admin
	// hand-picked and nobody else. Labelled *Direct* friends everywhere a person
	// reads it: "friends" in this project's vocabulary means the whole community
	// (§Goal & vocabulary), and using it here would understate what it restricts.
	DepthFriends = 0
	// DepthUnlimited ("Madnetwork") shares with our whole community — every
	// member reachable through mutually declared friendships. A concrete large
	// integer rather than a NULL sentinel so the comparison stays a plain >= in
	// SQL and on the wire, with no special case to forget in one of them.
	DepthUnlimited = 1 << 20
)

// ValidDepth reports whether d is a share depth this node accepts from an admin
// or a peer. Three values, not a ladder (F7): private, direct friends, or the
// whole madnetwork.
//
// The in-between hop counts were removed because every scope value is a claim
// about *our* behaviour, while "3 hops" is a claim about other people's — and
// nothing enforces it once a friend holds the bytes. Migration 035 snapped the
// stored ones onto [DepthFriends]; a peer or an admin sending one now is
// refused rather than silently rounded, since rounding a sharing decision is
// exactly the kind of quiet widening this predicate exists to prevent
// (docs/architecture/federation.md §Sharing scope).
func ValidDepth(d int) bool {
	return d == DepthPrivate || d == DepthFriends || d == DepthUnlimited
}

// Class is what a mesh requester *is* to this node — the three mesh principals
// of §Principals & access, in increasing order of reach. It is the first half of
// every access decision; [Audience.Distance] is the second.
//
// The zero value is [ClassOutsider], and that is load-bearing: an audience
// nobody filled in denies everything, so a forgotten branch or an error path
// that returns a bare Audience{} fails closed. Before F7 the zero value meant
// "direct friend at distance 0" — the widest audience there was — which is the
// shape of both leaks this type was introduced to close.
type Class int

const (
	// ClassOutsider is a node we cannot place in our community. It is served
	// nothing at all by default: being routable on Yggdrasil establishes nothing
	// (§Principals & access). The zero value, deliberately.
	ClassOutsider Class = iota
	// ClassGuest is an outsider this node has opted to answer anyway, limited to
	// guest-playable content — the node setting that replaced F5's always-on
	// guest-open swarm, and it defaults to off.
	ClassGuest
	// ClassMember is a key inside our community: reachable from us through
	// mutually declared friendships (§The membership rule). It reaches the
	// Madnetwork scope — by default the whole library — and the swarm, cache
	// included.
	ClassMember
	// ClassFriend is a node in federation_peers with state='friend'. A local
	// fact rather than hearsay, and the only class that additionally reaches
	// what an admin restricted to hand-picked nodes.
	ClassFriend
)

// Audience is who a mesh request is answered for (F5, extended by F7). The same
// value decides what the catalog lists and what the byte endpoints serve — a
// node must never advertise what it would not serve, so both halves read one
// rule.
//
// Class and Distance answer different questions and both are needed. Class is
// *who is asking*, and it gates the endpoints: whether this requester may pull a
// catalog at all, whether it may be served a cache blob. Distance is *how far
// away they are*, and it meets each recording's share depth in SQL
// (`COALESCE(r.share_depth, default) >= Distance`) — DepthFriends for a direct
// friend, DepthUnlimited for anyone further out, which yields exactly the
// recordings marked Madnetwork and nothing else.
type Audience struct {
	// Class is the mesh principal this request resolved to. The zero value
	// ([ClassOutsider]) is served nothing.
	Class Class
	// Distance is the reach the requester's class earns: DepthFriends (0) for a
	// direct friend, DepthUnlimited for a member or a guest, since a scope is a
	// statement about *whom* rather than about hop counts.
	Distance int
	// GuestOnly limits the audience to guest-accessible recordings (the
	// guest-playable / license policy). True for a friend mapped to a local
	// account without content.access, and for [ClassGuest].
	GuestOnly bool
}

// Serves reports whether this audience is answered at all. False for an
// outsider, which is every mesh node we cannot place in our community unless the
// admin opted into serving guests.
func (a Audience) Serves() bool { return a.Class >= ClassGuest }

// IsFriend reports whether the requester is a direct friend of ours — a peer row
// we hold, not a key vouched for by somebody else.
func (a Audience) IsFriend() bool { return a.Class == ClassFriend }

// InCommunity reports whether the requester belongs to our madnetwork: a member
// or a direct friend. This is the swarm's boundary and the catalog's
// (§Distribution — the swarm must not care which node happens to hold the
// bytes), so it is deliberately one predicate rather than two checks that could
// drift apart.
func (a Audience) InCommunity() bool { return a.Class >= ClassMember }

// ServesCache reports whether this node's download cache may be served to the
// requester. The community, and nobody else: a cached blob is somebody else's
// content that we merely hold, so seeding it outward is only defensible inside
// the network it came from.
//
// Written as a positive predicate on purpose. Its ancestor was `!aud.GuestOnly`
// — a guard that meant "is a friend" expressed as the negation of a bit whose
// meaning changed underneath it, and which would have started handing our cache
// to every member the moment members existed.
func (a Audience) ServesCache() bool { return a.InCommunity() }

// FriendAudience is the audience of an unmapped direct friend: the default
// regular-user identity, which sees the whole published set
// (docs/architecture/federation.md §Principals & access).
var FriendAudience = Audience{Class: ClassFriend, Distance: DepthFriends}

// MemberAudience is the audience of a node in our community that is not a direct
// friend (F7). Distance [DepthUnlimited] is what makes the F5 depth predicate
// select exactly the recordings marked Madnetwork — no new clause, no new
// column: the tier that needs no credential was always expressible as a reach.
var MemberAudience = Audience{Class: ClassMember, Distance: DepthUnlimited}

// GuestAudience is the audience of a node outside our community, on the node
// that opted to answer them: guest-playable content within the Madnetwork scope,
// at the byte endpoints only. Catalog and holdings stay inside the community.
//
// Its distance is DepthUnlimited rather than F5's 0, so an outsider can never be
// served something an admin restricted to direct friends — a stranger must never
// outrank a member.
var GuestAudience = Audience{Class: ClassGuest, Distance: DepthUnlimited, GuestOnly: true}

// SeedPolicy is the node-level answer to "what do we serve over the swarm"
// (runtime settings, /admin/settings). The two seeding switches are F4's; Guests
// is F7's, and it is the only one that defaults off.
type SeedPolicy struct {
	// Enabled is the master switch: off means this node consumes without serving
	// bytes to anyone. Default on.
	Enabled bool
	// Cache also seeds blobs this node merely downloaded, making popular content
	// spread — to our community only, never outward (see
	// [Audience.ServesCache]). Default on.
	Cache bool
	// Guests answers mesh nodes *outside* our community, with guest-playable
	// library content and nothing else. Default OFF: the posture is everything to
	// our community, nothing outside it, and this is the deliberate exception an
	// admin has to reach for. It replaces F5's always-on guest-open swarm.
	Guests bool
}

// Claim-report kinds (federation_claim_reports.kind, migration 034) — which
// check found a contradiction. See [ClaimReport].
const (
	// ClaimHeldBlob: the peer advertises a hash we hold ourselves, with a
	// fingerprint that does not match our own copy of those exact bytes. This is
	// the airtight case — identical bytes cannot fingerprint differently.
	ClaimHeldBlob = "held_blob"
	// ClaimGrouping: the peer asserts two renditions are the same recording, we
	// hold both, and our own fingerprints disagree. Testable without the peer's
	// cooperation and without any wire claim.
	ClaimGrouping = "grouping"
)

// Claim-report dispositions — the admin's decision, which detection never
// overwrites.
const (
	ClaimNew       = "new"
	ClaimDismissed = "dismissed"
	ClaimActed     = "acted"
)

// ClaimReport is one contradiction between what a peer advertises and what this
// node can verify for itself (F6). It is evidence shown to a human beside the
// Block action, never an input to a score: blocking stays manual here, because an
// automatic reputation system is a weapon in intra-network wars.
//
// Say "contradiction", not "lie". Only [ClaimHeldBlob] is arithmetic; the rest is
// a bit-error rate against a threshold, and a mismatch has innocent explanations
// (a different chromaprint build — hence both versions travel with the report —
// a peer that grouped a rendition wrongly, or an honest relay repeating someone
// else's claim, which makes the origin of a claim a separate question from its
// carrier).
type ClaimReport struct {
	ID int64 `json:"id"`
	// SourceID is the cached catalog the finding came out of. Since F7 item 5
	// that is a source row, not a peer row: we check what every member of our
	// community claims, and most of them are nodes no admin here has named.
	SourceID int64  `json:"source_id"`
	Kind     string `json:"kind"`
	Hash     string `json:"hash"`
	// OtherHash is the second blob involved, for ClaimGrouping.
	OtherHash string `json:"other_hash,omitempty"`
	// BER is the measured bit-error rate over Words compared fingerprint words.
	BER   float64 `json:"ber"`
	Words int     `json:"words"`
	// The two fingerprint heads that were compared, kept so the finding stays
	// reproducible after the catalog that carried it has been replaced. For
	// ClaimHeldBlob they are our own copy of the bytes and the peer's claim about
	// them; for ClaimGrouping *both are ours* — the two blobs the peer says are
	// one recording — since that check needs nothing from the peer.
	OurHead      string `json:"our_head,omitempty"`
	TheirHead    string `json:"their_head,omitempty"`
	OurVersion   string `json:"our_version,omitempty"`
	TheirVersion string `json:"their_version,omitempty"`
	Disposition  string `json:"disposition"`
	FirstSeen    int64  `json:"first_seen"`
	LastSeen     int64  `json:"last_seen"`

	// Display decorations, joined by the store: what to call the node the claim
	// came from, and the key that actually identifies it — which is also what an
	// admin blocks by when the node is not a peer.
	PeerName string `json:"peer_name,omitempty"`
	PeerKey  string `json:"peer_key,omitempty"`
}

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
//
// Two names, with different owners, deliberately never overwriting each other
// (migration 033): Name is the local label this admin chose and nothing else
// writes it, HeardName is what the peer calls *itself* — a claim, refreshed on
// every successful contact. [Peer.Label] resolves the two for display.
type Peer struct {
	ID        int64  `json:"id"`
	PublicKey string `json:"public_key"` // lowercase hex ed25519
	// Name is the local label: written only by an admin rename, and it always
	// wins. Empty means the admin never named this node.
	Name string `json:"name"`
	// HeardName is what the peer says its name is — hearsay, kept apart from the
	// label so that refreshing it can never destroy an admin's choice and
	// renaming can never hide what the peer calls itself.
	HeardName string `json:"heard_name"`
	State     string `json:"state"`
	PrevState string `json:"-"`
	UserID    *int64 `json:"user_id"`
	CreatedAt int64  `json:"created_at"`
	LastSeen  int64  `json:"last_seen"` // unix seconds; 0 = never

	// Block evidence (F6): what the published distrust mark says. Both describe
	// the current block only — an unblock leaves them behind, and the next block
	// overwrites them.
	BlockReason string `json:"block_reason,omitempty"`
	BlockedAt   int64  `json:"blocked_at,omitempty"`

	Username string `json:"username,omitempty"` // mapped account, display only
	Address  string `json:"address,omitempty"`  // derived mesh address, display only
	// LastAttempt is what our last outbound pairing attempt toward this node
	// did (display only, filled by the running node). Nil when we have not tried
	// since this process started.
	LastAttempt *PairAttempt `json:"last_attempt,omitempty"`
}

// PairAttempt is the outcome of the last outbound pairing attempt toward one
// node. It exists to answer the only question a peer stuck on
// `pending_outgoing` raises — *did our request get there, and what did they
// say?* — which the handshake otherwise keeps entirely to itself: every failure
// in [Node.pairWith] is a silent return, so an admin watching a pairing that
// never converges cannot tell an unreachable node from a refused request from a
// node whose admin simply has not clicked accept yet.
//
// In memory on the running node rather than in the peer row: it describes this
// process's last try, not a fact about the friendship, and one refresh tick
// re-derives it after a restart.
type PairAttempt struct {
	At int64 `json:"at"` // unix seconds
	// Result is what the far side answered: "friend" (it considers us mutual) or
	// "pending" (our request is recorded and waiting for its admin). Empty when
	// the attempt never got an answer.
	Result string `json:"result,omitempty"`
	// Error is a sentence for an admin when the attempt failed: unreachable,
	// refused, or an answer we could not read. Empty on success.
	Error string `json:"error,omitempty"`
}

// Delivered reports whether the request reached the far node and was answered —
// the line between "they have not seen it" and "they have not acted on it".
func (a PairAttempt) Delivered() bool { return a.Error == "" && a.Result != "" }

// Label is the name to show for a peer: the admin's local label if they set one,
// otherwise what the peer calls itself, otherwise empty — and an empty label is
// rendered as the short key, never as a blank. Callers that show a Label must
// show the mesh address or key beside it: a name is a convenience, the key is the
// identity, and nothing may be identified by a name (see §Friendship).
func (p *Peer) Label() string {
	if p.Name != "" {
		return p.Name
	}
	return p.HeardName
}

// Display is Label with the fallback Label promises but deliberately does not
// apply itself: the short key, so a peer is never named by a blank. Use it
// wherever a peer is written into a log line or a stats row — anywhere the
// result is read by a person and there is no second field carrying the identity.
// Label stays the raw resolution because one caller (the network map's
// displayName) has a better fallback than the key: what the *graph* calls the
// node. This is the Go twin of peerLabel() in admin/network.js.
func (p *Peer) Display() string {
	if label := p.Label(); label != "" {
		return label
	}
	return p.ShortKey()
}

// ShortKey abbreviates the public key to the length the admin UI shows. Enough
// to tell nodes apart at a glance, never enough to identify one — the full key
// is what does that.
func (p *Peer) ShortKey() string {
	if len(p.PublicKey) > shortKeyRunes {
		return p.PublicKey[:shortKeyRunes]
	}
	return p.PublicKey
}

// shortKeyRunes matches the slice in admin/network.js's peerLabel.
const shortKeyRunes = 12

// CatalogSource is a node whose published catalog this node holds a cached copy
// of (F7 item 5, docs/architecture/federation.md §Discovery beyond the friend
// ring). It is deliberately NOT a [Peer]: a peer row exists because an admin
// here decided something about that node, while a source row exists because the
// sweep pulled from it. Every friend is a source; so is every member of our
// community the frontier rotation has reached, and nobody decided anything about
// those.
//
// The row carries only what pulling needs — whom to ask, what we already hold,
// and when we last got an answer. Trust lives in the peer table, and the two are
// joined by public key wherever a cached row has to know whether an admin
// blocked its origin.
type CatalogSource struct {
	ID        int64
	PublicKey string // lowercase hex ed25519; the mesh address derives from it
	// HeardName is what the node calls itself. A source has no local label —
	// naming a node is an admin act, and an admin who wants to name this one
	// makes it a peer.
	HeardName string

	CatalogSerial   string // serial of the snapshot we hold; the `since=` we send
	CatalogSyncedAt int64  // last round that confirmed the cached copy fresh
	// AttemptedAt is when we last *tried*, successful or not. The frontier
	// rotates on this rather than on success, so an unreachable node takes its
	// turn and yields it instead of being retried ahead of everyone every cycle.
	AttemptedAt int64
	FirstSeen   int64
	LastSeen    int64 // last successful contact; feeds the freshness window
	// HintedAt is when a friend last vouched for this node's liveness (F7 item
	// 10, migration 038). It records not *that* the node was alive — LastSeen
	// holds that, whoever observed it — but that a minute-cadence observer is
	// watching it, which is what decides whether this source is judged by the
	// ping window or by the far wider pull window.
	HintedAt int64
}

// Display names a source for a log line or a UI row: what it calls itself, or
// its short key. There is no admin label to prefer — see [Peer.Display] for the
// case where there is.
func (s *CatalogSource) Display() string {
	if s.HeardName != "" {
		return s.HeardName
	}
	if len(s.PublicKey) > shortKeyRunes {
		return s.PublicKey[:shortKeyRunes]
	}
	return s.PublicKey
}

// BlobProvider is one node the swarm may fetch a hash from: a source whose
// cached catalog or holdings advertise it. Separate from [Peer] because a
// provider need not be one — since F7 item 5 most holders of a popular blob are
// members we never friended — and separate from [CatalogSource] because the
// fetch path also wants the peer identity when there is one, to label a stats
// row the way the admin named that node.
type BlobProvider struct {
	SourceID int64
	// PeerID is the local peer row, or 0 for a member no admin has touched.
	PeerID    int64
	PublicKey string
	Name      string // admin's label, when there is a peer row
	HeardName string
	LastSeen  int64
}

// Display names a provider on the same rules as [Peer.Display]: the admin's
// label if they set one, else what the node calls itself, else the short key.
func (p *BlobProvider) Display() string {
	if p.Name != "" {
		return p.Name
	}
	if p.HeardName != "" {
		return p.HeardName
	}
	if len(p.PublicKey) > shortKeyRunes {
		return p.PublicKey[:shortKeyRunes]
	}
	return p.PublicKey
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
	// BlockFederationPeer blocks a peer and records the evidence the published
	// distrust mark carries: when, and why. Every block publishes a mark — there
	// are no private blocks (docs/architecture/federation.md §Friend-list gossip).
	BlockFederationPeer(ctx context.Context, id int64, prevState, reason string, at int64) error
	UpdateFederationPeerName(ctx context.Context, id int64, name string) error
	// UpdateFederationPeerHeardName records what the peer calls itself, learned
	// from a ping or pairing reply. Separate from the rename above because the
	// two names have separate owners: this one must never touch a local label.
	UpdateFederationPeerHeardName(ctx context.Context, id int64, name string) error
	SetFederationPeerUser(ctx context.Context, id int64, userID *int64) error
	TouchFederationPeerSeen(ctx context.Context, id int64, when int64) error
	DeleteFederationPeer(ctx context.Context, id int64) error

	// PublishedCatalog is this node's own catalog for one audience — every
	// approved, live appearance the audience's scope admits, with its
	// recording's renditions, in a stable order (the snapshot serial is a hash
	// over it, so each audience has its own serial).
	PublishedCatalog(ctx context.Context, aud Audience) ([]CatalogEntry, error)
	// ReplaceSourceCatalog atomically replaces the cached copy of one source's
	// catalog with a fresh snapshot and records its serial + sync time.
	ReplaceSourceCatalog(ctx context.Context, sourceID int64, serial string, syncedAt int64, entries []CatalogEntry) error
	// MarkSourceCatalogChecked records a sync round that found the cached copy
	// still fresh (the not-modified path).
	MarkSourceCatalogChecked(ctx context.Context, sourceID int64, serial string, syncedAt int64) error

	// ── Catalog sources (F7 item 5, §Discovery beyond the friend ring) ───────
	// The set of nodes we pull catalogs from: every friend, plus the members the
	// frontier rotation has reached. Rows here are a cache in the sense
	// federation_catalog established — rebuildable from the network, referenced
	// by nothing local, safe to drop.

	// EnsureCatalogSource returns the source row for a node key, creating it if
	// this is the first time we have pulled from it.
	EnsureCatalogSource(ctx context.Context, publicKey string, now int64) (*CatalogSource, error)
	// ListCatalogSources returns every source, oldest attempt first — the order
	// the frontier rotation consumes.
	ListCatalogSources(ctx context.Context) ([]*CatalogSource, error)
	// MarkCatalogSourceAttempted records that we tried, whatever came of it.
	MarkCatalogSourceAttempted(ctx context.Context, id int64, at int64) error
	// TouchCatalogSourceSeen records a successful contact, and what the node
	// called itself if it said. last_seen is monotonic.
	TouchCatalogSourceSeen(ctx context.Context, id int64, at int64, heardName string) error
	// ApplyFreshnessHints records what a friend just vouched for (F7 item 10):
	// seen maps a node key to the unix time that friend last touched it
	// first-hand. Only sources we already hold are updated — a hint about a node
	// we cache nothing from names no row and must create none. last_seen moves
	// forward as usual; hinted_at records that a minute-cadence observer is
	// watching, which is what picks the availability window. Returns how many
	// sources it moved.
	ApplyFreshnessHints(ctx context.Context, seen map[string]int64, at int64) (int, error)
	// DropCatalogSources deletes sources and everything cached from them
	// (CASCADE): the ones we may no longer keep, and the ones evicted past the
	// cap. An empty list is a no-op, never "drop everything".
	DropCatalogSources(ctx context.Context, ids []int64) error

	// BlobVisibleTo reports whether the blob with this content hash may be
	// served to aud: part of the published library (live file + an approved
	// appearance on its recording) *and* inside the audience's scope — the same
	// rule the catalog above answers by, so the node never advertises what it
	// would not serve (database/madnetwork_scope.go).
	BlobVisibleTo(ctx context.Context, hash string, aud Audience) (visible, found bool, err error)
	// PeerAudience resolves a known peer to the audience its requests are
	// answered for (the user mapping, §Principals & access).
	PeerAudience(ctx context.Context, peerID int64) (Audience, error)
	// MadnetworkBlobProviders returns the nodes that hold hash — the swarm
	// tracker (F4): the union of catalog (library) holders and holdings (cache)
	// holders, most recently seen first (the fetch order) — plus the advertised
	// byte size (a hint; the origin's Content-Length / manifest wins). Since F7
	// item 5 these are cached *sources*, so a holder may be any member of our
	// community and not only a friend; a blocked node is never one.
	MadnetworkBlobProviders(ctx context.Context, hash string) (size int64, holders []*BlobProvider, err error)
	// ReplaceSourceHoldings atomically replaces the cached list of what one
	// source holds in its download cache and will seed (F4 holdings sync).
	ReplaceSourceHoldings(ctx context.Context, sourceID int64, hashes []string) error
	// SeedingPolicy reports what this node is willing to serve over the swarm —
	// the F4 serving gate, extended by F7's guest switch.
	SeedingPolicy(ctx context.Context) (SeedPolicy, error)

	// CheckSourceClaims re-runs the contradiction checks over one source's cached
	// catalog and records what it finds, returning how many reports are newly
	// open. Idempotent: re-finding a contradiction refreshes it and leaves the
	// admin's disposition alone (F6, migration 034).
	//
	// It reads the *cache* rather than a freshly received snapshot on purpose. A
	// peer whose catalog has not changed sends no entries, but what we hold
	// locally changes all the time — every upload and every materialized download
	// is a new blob to check old claims against — so the check has to be able to
	// run when only our side moved.
	CheckSourceClaims(ctx context.Context, sourceID int64) (newlyOpen int, err error)
	// ScanSourceUpgrades compares one source's cached catalog against the local
	// library and records the renditions that would beat ours, returning how many
	// findings await a decision (F8 item 3, migration 039). It rides the same
	// cadence and reads the same cache as CheckSourceClaims, for the same reason:
	// their catalog stands still while our library moves. Idempotent, and it
	// never overwrites an admin's disposition.
	ScanSourceUpgrades(ctx context.Context, sourceID, now int64) (open int, err error)
	// SweepUpgrades drops findings that stopped being true — the remote blob left
	// every cached catalog, or we hold those bytes now.
	SweepUpgrades(ctx context.Context) error
	// ListClaimReports returns the findings still awaiting an admin decision,
	// newest first, decorated with the reporting peer's label and key.
	ListClaimReports(ctx context.Context) ([]*ClaimReport, error)
	// SetClaimReportDisposition records that decision. Detection never overwrites
	// it, so a dismissed finding stays dismissed.
	SetClaimReportDisposition(ctx context.Context, id int64, disposition string) error

	GraphStore
}

// GraphStore is the gossiped-network-graph half of the persistence (F6,
// gossip.go): the signed records this node holds — its own and every one
// relayed to it by a friend — plus the denormalized edges and marks the map
// and the admission checks query. Embedded in [PeerStore] rather than wired
// separately, since a node that gossips is always a node that has a store.
//
// Everything here is a cache in the sense federation_catalog established:
// rebuildable from the network, referenced by nothing local, and safe to drop.
type GraphStore interface {
	// PutGraphRecord stores a verified friend-list record and rewrites that
	// origin's edges, but only if seq is higher than what we already hold —
	// reported by the bool. A record we have seen is dropped and NOT
	// re-propagated, which is what terminates gossip loops without hop counts
	// or TTL bookkeeping. receivedFrom is the friend that delivered it (nil for
	// this node's own record).
	PutGraphRecord(ctx context.Context, rec *GraphRecord, payload []byte, receivedFrom *int64, expiresAt, now int64) (stored bool, err error)
	// PutMarkRecord is the same for a distrust list.
	PutMarkRecord(ctx context.Context, rec *MarkRecord, payload []byte, receivedFrom *int64, expiresAt, now int64) (stored bool, err error)

	// GraphDigest lists what this node holds, unexpired and origin-ordered, for
	// the digest exchange that opens a sync round.
	GraphDigest(ctx context.Context, now int64) (records, marks []GraphDigestEntry, err error)
	// GraphPayloads returns the raw signed bytes for the named origins —
	// verbatim, since re-encoding a record would invalidate its signature.
	GraphPayloads(ctx context.Context, origins []string, now int64) (map[string][]byte, error)
	// MarkPayloads is the same for distrust records.
	MarkPayloads(ctx context.Context, origins []string, now int64) (map[string][]byte, error)

	// GraphKnowsKey reports whether a key is one we would accept a record from:
	// a direct friend, or a node some record we hold already names. It is the
	// admission rule — a record whose author nobody in our store has heard of
	// is junk a friend invented, and is dropped unread.
	GraphKnowsKey(ctx context.Context, key string) (bool, error)
	// GraphIntroducedCount is how many origins arrived through one friend, for
	// the per-branch quota ([MaxOriginsPerBranch]).
	GraphIntroducedCount(ctx context.Context, peerID int64) (int, error)

	// ExpireGraph drops records past their expiry along with their edges and
	// marks, returning how many records went. Ageing by wall clock: stop
	// refreshing a record and it leaves every store on its own.
	ExpireGraph(ctx context.Context, now int64) (int, error)
	// DropUnreachableGraph drops every record whose origin is not in keep — the
	// other ageing mechanism, and the one that answers an admin's action rather
	// than a clock. Blocking or removing a friend severs an edge, which makes
	// the branch behind it unreachable, which this collects ([ReachableKeys],
	// docs/architecture/federation.md §Forgetting).
	DropUnreachableGraph(ctx context.Context, keep map[string]struct{}) (int, error)

	// GraphEdges and GraphMarks are the unexpired denormalized claims, for the
	// network map and its branch-weighted mark display.
	GraphEdges(ctx context.Context, now int64) ([]GraphEdgeClaim, error)
	GraphMarks(ctx context.Context, now int64) ([]StoredMark, error)

	// PublishFriendList reports whether this node publishes its own friend-list
	// record (runtime setting, default on). Off means only that: friends' own
	// records still name this node, so it stays on the map either way.
	PublishFriendList(ctx context.Context) (bool, error)
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
	discovery   Discovery
}

// Discovery bounds how far past the friend ring this node pulls catalogs (F7
// item 5, docs/architecture/federation.md §Discovery beyond the friend ring).
//
// Friends are pulled unbudgeted — they are the ring an admin chose, and there
// are few of them. Members are not: a community has no size limit by design (§The
// membership rule), so pulling every mapped node every cycle is the N² dialling
// pattern that was rejected for gossip records, and caching every node's library
// is unbounded storage. The frontier is therefore rotated a few nodes at a time
// and capped, which trades how *fast* the network becomes visible for a cost that
// does not grow with it.
//
// Both numbers are policy nobody can derive from first principles — they want a
// real network to observe — so they are configuration ([federation]
// discovery_budget / discovery_cap) rather than constants.
type Discovery struct {
	// Budget is how many member catalogs to pull per catalog cycle, beyond the
	// friends. 0 disables discovery entirely: the node still serves its
	// community, it just stops seeing past its friends. Default 4.
	Budget int
	// Cap is the largest number of non-peer catalogs to keep cached. Past it the
	// least-recently-seen are dropped, which is safe because a cached catalog is
	// rebuildable from the network and referenced by nothing local. Default 200.
	Cap int
}

// WithDiscovery overrides the frontier bounds (zero fields keep defaults; a
// negative budget is read as 0, "friends only").
func WithDiscovery(d Discovery) Option { return func(o *nodeOptions) { o.discovery = d } }

// withDefaults fills unset fields from d. Budget is distinguished from unset by
// being negative, since 0 is a meaningful choice here — "pull from friends only"
// — while a 0 interval is merely an unfilled struct field.
func (dc Discovery) withDefaults(d Discovery) Discovery {
	if dc.Budget == 0 {
		dc.Budget = d.Budget
	} else if dc.Budget < 0 {
		dc.Budget = 0
	}
	if dc.Cap <= 0 {
		dc.Cap = d.Cap
	}
	return dc
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

	// GraphRepublish is how often this node re-signs its own friend-list record
	// even when nothing changed (F6). The heartbeat is what keeps a live node on
	// everyone's map, since records age out on GraphTTL rather than on a hop
	// count. Default 6 h.
	GraphRepublish time.Duration
	// GraphTTL is how long a gossiped record is kept and served after it was
	// issued. Default 7 days — long enough that an intermittently-online home
	// server survives a weekend offline, short enough that an abandoned key
	// fades from every store within a week without anyone acting.
	GraphTTL time.Duration
	// GraphAccept bounds how often one origin's record may be accepted, so a
	// node churning sequences costs a map lookup rather than a write. Default
	// 1 min.
	//
	// It throttles honest updates too — a node that friends three peers in a
	// row has its first republish stored and the rest deferred — which is why
	// convergence must never depend on catching a particular record: the next
	// round fetches whatever was dropped. A lab shrinks this, or a scenario
	// that changes friendships faster than the interval will look broken while
	// merely being slow.
	GraphAccept time.Duration
	// GraphDigestTTL is how long the digest this node serves to friends is
	// memoized (the ownSnapshot pattern, gossip_node.go). Default 30 s.
	//
	// It is the whole rate limit on GET /madnetwork/v0/graph: a friend pulling
	// too often gets the memo instead of a refusal, because syncGraph cannot
	// tell a 429 from a peer that has no such endpoint. Anything that changes
	// the store — a record learned, a branch dropped — invalidates it, so the
	// TTL bounds staleness only for the case where nothing happened.
	GraphDigestTTL time.Duration
	// MembershipTTL is how long the computed community (membership.go, F7) is
	// reused before it is rebuilt from the store. Default 1 min.
	//
	// The sweep recomputes it every round anyway, so this bounds only the case
	// where nothing has swept yet — and it bounds it in the safe direction: a
	// node that just joined the community waits at most this long for reach,
	// while a node that just left loses it immediately, because a block is
	// enforced at meshAuth off the peer table and never from this memo.
	MembershipTTL time.Duration
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
	if iv.GraphRepublish <= 0 {
		iv.GraphRepublish = d.GraphRepublish
	}
	if iv.GraphTTL <= 0 {
		iv.GraphTTL = d.GraphTTL
	}
	if iv.GraphAccept <= 0 {
		iv.GraphAccept = d.GraphAccept
	}
	if iv.GraphDigestTTL <= 0 {
		iv.GraphDigestTTL = d.GraphDigestTTL
	}
	if iv.MembershipTTL <= 0 {
		iv.MembershipTTL = d.MembershipTTL
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
	// Fingerprint is the origin's audio-identity claim for these bytes (F6). Nil
	// when the origin never fingerprinted the blob or speaks an older protocol —
	// an absent claim is uncheckable, not suspicious.
	Fingerprint *FingerprintClaim `json:"fingerprint,omitempty"`
}

// ClaimHeadWords is how many raw sub-fingerprint words a [FingerprintClaim]
// carries. It is a *head*, not the whole fingerprint, and the reason is
// arithmetic: a real fingerprint measures ~950 words (3.8 KB packed) for a
// four-minute track, and a catalog snapshot is re-sent in full whenever its
// serial changes, so shipping all of it would inflate a thousand-rendition
// catalog by ~5 MB per sync — on a 15-minute cadence, between home servers that
// are only intermittently online.
//
// 64 words is ~15 s of audio and 2048 compared bits, which is decisive: the same
// bytes score a bit-error rate of 0, and unrelated audio lands near 0.5. The
// comparison is start-aligned, exactly like the local matcher
// (database.ResolveRecording), so a head is the same kind of evidence, measured
// over less of it.
const ClaimHeadWords = 64

// FingerprintClaim is what a rendition says its audio *is* — the head of the
// origin's own acoustic fingerprint, so a receiver holding the same bytes can
// check the claim instead of taking it on trust
// (docs/architecture/federation.md §Trust graph, contradicted claims).
//
// Publishing it leaks nothing new: a friend already receives the content hash and
// the full tag text of everything in scope, so the claim only makes an existing
// assertion checkable.
type FingerprintClaim struct {
	// Algo and Version identify the fingerprinter. Version matters more than it
	// looks: chromaprint output is build-sensitive, which is why a mismatch
	// between different versions is an innocent explanation rather than evidence.
	Algo    string `json:"algo,omitempty"`
	Version string `json:"version,omitempty"`
	// Words is the length of the origin's *whole* fingerprint, so a receiver can
	// see that two claims describe audio of very different lengths.
	Words int `json:"words,omitempty"`
	// Head is base64 (standard encoding) of the first [ClaimHeadWords] words,
	// packed little-endian — the wire form of what media.DecodeFingerprint reads.
	Head string `json:"head"`
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

// CapabilityGrant is one issued listener-node token as its holder needs it
// (F7 item 9, token.go — untagged so the API surface exists in both build
// variants). RenewAfter is the token's half-life: a madplayer that refreshes
// then keeps a transient outage of its home server from becoming a service
// interruption, and no verifier enforces it.
type CapabilityGrant struct {
	Token      string    `json:"token"`
	Issuer     string    `json:"issuer"`
	Bearer     string    `json:"bearer"`
	ExpiresAt  time.Time `json:"expires_at"`
	RenewAfter time.Time `json:"renew_after"`
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

// MaxPeerNameRunes caps a node's display name. 64 clears a DNS label (63
// octets), so no realistic host name — the default self-name — is ever
// truncated, while staying short enough not to disturb a layout. A name is only
// a label: identity is the key, and every surface showing a name shows the mesh
// address beside it (docs/architecture/federation.md §Friendship).
const MaxPeerNameRunes = 64

// CleanPeerName sanitizes and length-caps a peer-supplied display name (cards,
// pair requests, gossiped friend lists and admin renames all pass through here —
// it is deliberately the single choke point).
//
// The cap counts runes, not bytes. Slicing a UTF-8 string at a byte offset can
// cut a multi-byte character in half and store the broken tail, which every
// non-ASCII name long enough to be truncated would hit.
func CleanPeerName(name string) string { return sanitizeLabel(name, MaxPeerNameRunes) }

// maxCombiningMarks bounds the combining marks allowed per base character — the
// "Zalgo" stack that otherwise smears a name over the rows above and below it.
// Two is generous for every living script.
const maxCombiningMarks = 2

// sanitizeLabel is the display-integrity rule set shared by peer names and
// distrust-mark reasons (docs/architecture/federation.md §Name sanitization).
//
// This is NOT an injection defense and must never be sold as one: the admin UI
// escapes by assigning textContent, and that stays the defense against XSS. What
// this buys is that a label renders as what it is, and that two different nodes
// cannot render identically — an invisible difference is worse than a visible
// collision, because a collision is something an admin can see and check the
// address of.
//
// The order is load-bearing:
//
//  1. drop invalid UTF-8 (Go decodes it as U+FFFD, i.e. tofu);
//  2. strip Cc (controls: C0/C1, newline, tab, DEL), Cf (bidi overrides that
//     reverse a rendered name, the zero-width characters that make two names
//     look alike, U+FEFF) and Co (private use: vendor glyphs and tofu);
//  3. normalize to NFC, so "é" as one rune and "e" plus a combining accent stop
//     being two byte-different names that render the same — before the mark
//     bound, since composing removes marks that step would otherwise count;
//  4. collapse whitespace runs to one U+0020 (this is also what folds NBSP and
//     friends onto the plain space) and trim the ends;
//  5. bound the combining marks per base character;
//  6. cap to maxRunes LAST, so stripped junk cannot eat the budget and truncate
//     the real name.
//
// The result may be empty, which every caller renders as the short key rather
// than as a blank label.
//
// Accepted cost, stated rather than hidden: step 2 also removes U+200C (ZWNJ),
// orthographically meaningful in Persian and Arabic, and U+200D (ZWJ), so an
// emoji family becomes separate people. The narrower "all of Cf except the
// joiners" reopens exactly the invisible-difference vector the rule closes, so
// it is not the default — a label carries no identity role here. Homoglyphs
// (Cyrillic "а" against Latin "a") stay unsolved on purpose: filtering them
// needs mixed-script heuristics that punish legitimate multilingual names, and
// the answer is the one the whole design rests on — the address is shown beside
// the name, and identity is the key.
func sanitizeLabel(s string, maxRunes int) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		// A decoding failure yields RuneError; so does a literal U+FFFD, which is
		// tofu we have no reason to keep either.
		if r == utf8.RuneError {
			continue
		}
		if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Co, r) {
			continue
		}
		b.WriteRune(r)
	}

	out := make([]rune, 0, maxRunes)
	marks := 0            // combining marks seen on the current base character
	base := false         // is there a base character for a mark to attach to?
	pendingSpace := false // a collapsed run, written only if something follows it
	for _, r := range norm.NFC.String(b.String()) {
		switch {
		case unicode.IsSpace(r):
			pendingSpace = len(out) > 0 // never leading; a trailing run is dropped
			base = false
			continue
		case unicode.Is(unicode.M, r):
			// A mark with no base is a floating diacritic, not a name.
			if !base || marks >= maxCombiningMarks {
				continue
			}
			marks++
		default:
			marks, base = 0, true
		}
		if pendingSpace {
			if len(out) >= maxRunes {
				break
			}
			out, pendingSpace = append(out, ' '), false
		}
		if len(out) >= maxRunes {
			break
		}
		out = append(out, r)
	}
	return string(out)
}
