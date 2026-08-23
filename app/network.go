package app

import (
	"context"
	"errors"
	"time"

	"daemonlord.ygg/madshare/federation"
)

// Network is the mesh surface an embedder calls instead of the madnetwork HTTP
// API — the counterpart of Library, and the same bargain: a small named method
// set that is promised to stay, rather than a licence to reach into
// federation.Node's hundred methods.
//
// It exists for one participant, the listener node
// (docs/architecture/federation-access.md §"The household"). A server needs none of
// this: it discovers its own holders, is placed by other people's graph walks,
// and has nothing to sign in to. A device has none of those things, so each
// method here is the substitute for one of them — SetToken for the vouch it
// cannot earn, Fetch for the holders it cannot discover, AddHome for the
// community it is not in, Holdings for the advertisement nobody would otherwise
// make.
//
// Everything here is a no-op or an error when no madnetwork node is running,
// but you will not see that: Instance.Network reports it up front, because a
// program that cannot use the mesh should find out once at startup rather than
// per call.
type Network interface {
	// Key is this node's ed25519 public key, lowercase hex — what it calls
	// itself when asking a home server for a token, and the bearer that token
	// names.
	Key() string
	// Address is this node's yggdrasil address, derived from Key. For display:
	// nothing is addressed by it, and it is NOT reachable from this host (there
	// is no TUN device).
	Address() string

	// SetToken installs the capability token presented on outbound mesh
	// requests, or clears it with "". Safe to call at any time and from any
	// goroutine, which is the point — a token is renewed at its half-life while
	// transfers are in flight.
	SetToken(token string)

	// AddHome records a server this node has signed in to, so that server and
	// the devices it vouches for may fetch from this one. It is not a peering:
	// no card, no accept, no gossip edge, and the other end never learns of it.
	AddHome(ctx context.Context, publicKey, baseURL, name string) error
	// RemoveHome forgets one. Signing out stops us serving its devices on the
	// next request rather than on a timer.
	RemoveHome(ctx context.Context, publicKey string) error
	// Homes lists them, oldest first.
	Homes(ctx context.Context) ([]federation.ExternalNode, error)

	// Fetch downloads a blob from holders the caller names, into this node's
	// download cache, and returns the running transfer. size may be 0 when
	// unknown; the manifest is the authority either way.
	//
	// The holders are public keys, because that is what a home server's browse
	// rows and its holders endpoint carry. A hash this node already holds — in
	// its library or its cache — comes back complete without a byte crossing
	// the network, which is the offline case working rather than an
	// optimisation.
	Fetch(ctx context.Context, hash string, size int64, holders []string) (federation.Transfer, error)

	// Holdings is what this node has fetched and would seed: the list a device
	// pushes to its home server so anything can learn it holds anything.
	Holdings() []string

	// AddPeer dials an underlay peer now, rather than at the next start.
	//
	// A device learns where the mesh is by signing in, which happens long after
	// startup — so without this, "signing in also gets you onto the mesh" would
	// mean "next time you open the app". Re-adding a peer already dialled is not
	// a second link, so a caller needs no bookkeeping.
	AddPeer(uri string) error

	// UnderlayPeers reports what every peering this node holds is actually
	// doing: up or down, which way it was dialled, since when, its traffic, and
	// the last connection error with its age. Configured links that have never
	// connected are in it too, and sort first.
	//
	// It is the other half of AddPeer, and the halves are further apart than
	// they look. AddPeer returns as soon as the link is CONFIGURED — the dial
	// happens on the core's own goroutine, with backoff, and a URI that will
	// never connect returns exactly the same nil as one that connects in a
	// millisecond. Without this an embedder can offer somebody a box to type a
	// peer into and then has nothing to tell them, which is what a server's
	// admin gets from /admin/network's Underlay tab and a device's owner did
	// not (madplayer, 2026-08-18).
	//
	// Read-only diagnosis, and the same data that tab reads. The embedded core
	// has no yggdrasilctl socket, so this is the only way to see it at all.
	UnderlayPeers() []federation.UnderlayPeer

	// PublishNothing pins this node's default sharing scope to Local, so nothing
	// it holds is advertised in a catalog or served as bytes. Idempotent; a
	// listener node calls it once its mesh is up.
	//
	// It became load-bearing the moment a device could place anybody
	// (§"The household"). While a listener node served nobody, the scope check
	// was simply never reached and the shipped default of "Madnetwork" was inert
	// — which is what docs/ui/madplayer.md §"Why publishes nothing needs no
	// setting" says, and it was true when it was written. Now a home server is a
	// member from the device's side, so the unpinned default would let it pull
	// the device's own catalog and its blobs: exactly the one-way publication
	// rule, broken by the mechanism built to let the device seed.
	//
	// It does NOT touch the cache, which is served by a separate arm of
	// seedableBlob. Seeding what you fetched and publishing what you own are
	// different claims, and only the second is one nobody should make on a
	// person's phone.
	PublishNothing(ctx context.Context) error
}

// ErrNoMesh is returned by Network's calls when the node stopped underneath
// them. Instance.Network's second return value is the check that matters; this
// is the one for the race.
var ErrNoMesh = errors.New("app: no madnetwork node is running")

// Network returns the mesh surface, and whether there is one.
//
// False means this instance has no madnetwork node — federation is disabled, or
// the binary was built with -tags nofederation, or only the transport is up.
// Unlike Library, which every instance has, the mesh is a thing a configuration
// can simply not include, and an embedder should branch on that once rather than
// discover it from a failing call.
func (i *Instance) Network() (Network, bool) {
	if i == nil || i.node == nil {
		return nil, false
	}
	return network{i}, true
}

type network struct{ inst *Instance }

func (n network) Key() string     { return n.inst.node.PublicKeyHex() }
func (n network) Address() string { return n.inst.node.Address().String() }

func (n network) SetToken(token string) { n.inst.token.Store(&token) }

func (n network) AddHome(ctx context.Context, publicKey, baseURL, name string) error {
	if err := n.inst.db.AddHomeNode(ctx, publicKey, baseURL, name, time.Now().Unix()); err != nil {
		return err
	}
	// The household is an input of the node's membership memo
	// (federation/membership.go), and this write goes straight to the store —
	// the node would not notice for up to a MembershipTTL, refusing the home
	// server's perfectly valid tokens meanwhile, which a listener experiences
	// as a minute of skipped tracks right after signing in. Recorded here,
	// honoured on the next request.
	n.inst.node.InvalidateMembers()
	return nil
}

func (n network) RemoveHome(ctx context.Context, publicKey string) error {
	if err := n.inst.db.RemoveHomeNode(ctx, publicKey); err != nil {
		return err
	}
	// Signing out stops us serving its devices on the next request rather than
	// on a timer — the promise this method's contract makes.
	n.inst.node.InvalidateMembers()
	return nil
}

func (n network) Homes(ctx context.Context) ([]federation.ExternalNode, error) {
	return n.inst.db.ListHomeNodes(ctx)
}

func (n network) Holdings() []string { return n.inst.node.CacheHoldings() }

func (n network) AddPeer(uri string) error { return n.inst.node.Mesh().AddPeer(uri) }

func (n network) UnderlayPeers() []federation.UnderlayPeer { return n.inst.node.UnderlayPeers() }

func (n network) PublishNothing(ctx context.Context) error {
	policy, err := n.inst.db.GetMadnetworkPolicy(ctx)
	if err != nil {
		return err
	}
	if policy.DefaultShareDepth == federation.DepthPrivate {
		return nil
	}
	// Read-modify-write rather than a single-key setter, because the policy is
	// stored and read as one object: writing the depth alone through a private
	// path would leave two ways to change it that could disagree.
	policy.DefaultShareDepth = federation.DepthPrivate
	return n.inst.db.SetMadnetworkPolicy(ctx, policy)
}

// Fetch turns the keys a caller has into the providers the swarm wants. A key
// that is not 64 hex characters is dropped rather than refused: a holder list
// arrives from another machine, and one bad entry in it should cost that holder
// and not the download.
func (n network) Fetch(ctx context.Context, hash string, size int64, holders []string) (federation.Transfer, error) {
	providers := make([]*federation.BlobProvider, 0, len(holders))
	for _, key := range holders {
		norm, err := federation.NormalizeKey(key)
		if err != nil {
			continue
		}
		providers = append(providers, &federation.BlobProvider{PublicKey: norm})
	}
	if len(providers) == 0 {
		// Distinguished from "we asked everyone and nobody had it", which is what
		// federation.ErrNoHolder means and what a caller may want to retry.
		return nil, federation.ErrNoHolder
	}
	return n.inst.node.EnsureBlobFrom(ctx, hash, size, providers)
}
