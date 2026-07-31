//go:build tests && !nofederation

package main

// The outsider probe: a real madnetwork node, on the real mesh, that is nobody's
// friend.
//
// Every other actor in the lab is a madshare server the lab friended. That is
// enough for "what do friends see", and useless for the question F5 introduced —
// what does a STRANGER see? Since F5 the answer is no longer a flat "nothing":
// guest-playable content serves any mesh node, friend or not
// (docs/architecture/federation.md §Sharing scope, "Guest-playable is an open
// swarm"), while catalog and holdings stay friends-only and anything else 404s.
//
// That asymmetry cannot be driven through a lab node's own HTTP API, because a
// madshare node only ever fetches from FRIENDS: providers come from the cached
// catalogs and holdings, and both are friends-only
// (database/madnetwork.go, MadnetworkBlobProviders). A stranger has to already
// know the hash — which is exactly the design, and exactly why the probe exists:
// it learns the hash from the lab, then asks over the mesh as an outsider.
//
// The probe carries its own ed25519 identity and peers straight into a lab
// node's underlay, deliberately bypassing the fault links: it is asking an
// authorization question, and an answer that depended on injected latency would
// be measuring the wrong thing.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"daemonlord.ygg/madshare/config"
	"daemonlord.ygg/madshare/federation"
)

// emptyStore is a PeerStore that knows nobody and publishes nothing.
//
// The probe never serves — it only dials — but federation.Node's refresh loop
// calls ListFederationPeers unconditionally, so the store cannot be nil. Every
// method answering "empty" is the honest model of a node with no friends: the
// probe is not pretending to be isolated, it IS isolated.
type emptyStore struct{}

func (emptyStore) ListFederationPeers(context.Context) ([]*federation.Peer, error) {
	return nil, nil
}
func (emptyStore) GetFederationPeer(context.Context, int64) (*federation.Peer, error) {
	return nil, federation.ErrPeerNotFound
}
func (emptyStore) GetFederationPeerByKey(context.Context, string) (*federation.Peer, error) {
	return nil, federation.ErrPeerNotFound
}
func (emptyStore) InsertFederationPeer(context.Context, *federation.Peer) (int64, error) {
	return 0, nil
}
func (emptyStore) SetFederationPeerState(context.Context, int64, string, string) error { return nil }
func (emptyStore) BlockFederationPeer(context.Context, int64, string, string, int64) error {
	return nil
}
func (emptyStore) UpdateFederationPeerName(context.Context, int64, string) error      { return nil }
func (emptyStore) UpdateFederationPeerHeardName(context.Context, int64, string) error { return nil }
func (emptyStore) SetFederationPeerUser(context.Context, int64, *int64) error         { return nil }
func (emptyStore) TouchFederationPeerSeen(context.Context, int64, int64) error        { return nil }
func (emptyStore) DeleteFederationPeer(context.Context, int64) error                  { return nil }

func (emptyStore) PublishedCatalog(context.Context, federation.Audience) ([]federation.CatalogEntry, error) {
	return nil, nil
}
func (emptyStore) ReplaceSourceCatalog(context.Context, int64, string, int64, []federation.CatalogEntry) error {
	return nil
}
func (emptyStore) MarkSourceCatalogChecked(context.Context, int64, string, int64) error { return nil }

// Catalog sources (F7 item 5): the probe caches nothing from anyone, so it never
// creates a source and its frontier is always empty.
func (emptyStore) EnsureCatalogSource(_ context.Context, publicKey string, now int64) (*federation.CatalogSource, error) {
	return &federation.CatalogSource{PublicKey: publicKey, FirstSeen: now}, nil
}
func (emptyStore) ListCatalogSources(context.Context) ([]*federation.CatalogSource, error) {
	return nil, nil
}
func (emptyStore) MarkCatalogSourceAttempted(context.Context, int64, int64) error { return nil }
func (emptyStore) TouchCatalogSourceSeen(context.Context, int64, int64, string) error {
	return nil
}
func (emptyStore) DropCatalogSources(context.Context, []int64) error { return nil }
func (emptyStore) BlobVisibleTo(context.Context, string, federation.Audience) (bool, bool, error) {
	return false, false, nil
}
func (emptyStore) PeerAudience(context.Context, int64) (federation.Audience, error) {
	return federation.GuestAudience, nil
}
func (emptyStore) MadnetworkBlobProviders(context.Context, string) (int64, []*federation.BlobProvider, error) {
	return 0, nil, nil
}
func (emptyStore) ReplaceSourceHoldings(context.Context, int64, []string) error { return nil }
func (emptyStore) SeedingPolicy(context.Context) (federation.SeedPolicy, error) {
	return federation.SeedPolicy{}, nil
}

// The gossiped graph (F6) is empty for the same reason as everything above: a
// node with no friends has heard no records and publishes none. GraphKnowsKey
// answering false also means the probe would refuse any record offered to it,
// which is the correct behaviour for a node that knows nobody.
func (emptyStore) PutGraphRecord(context.Context, *federation.GraphRecord, []byte, *int64, int64, int64) (bool, error) {
	return false, nil
}
func (emptyStore) PutMarkRecord(context.Context, *federation.MarkRecord, []byte, *int64, int64, int64) (bool, error) {
	return false, nil
}
func (emptyStore) GraphDigest(context.Context, int64) ([]federation.GraphDigestEntry, []federation.GraphDigestEntry, error) {
	return nil, nil, nil
}
func (emptyStore) GraphPayloads(context.Context, []string, int64) (map[string][]byte, error) {
	return nil, nil
}
func (emptyStore) MarkPayloads(context.Context, []string, int64) (map[string][]byte, error) {
	return nil, nil
}
func (emptyStore) GraphKnowsKey(context.Context, string) (bool, error)      { return false, nil }
func (emptyStore) GraphIntroducedCount(context.Context, int64) (int, error) { return 0, nil }
func (emptyStore) ExpireGraph(context.Context, int64) (int, error)          { return 0, nil }
func (emptyStore) DropUnreachableGraph(context.Context, map[string]struct{}) (int, error) {
	return 0, nil
}
func (emptyStore) GraphEdges(context.Context, int64) ([]federation.GraphEdgeClaim, error) {
	return nil, nil
}
func (emptyStore) GraphMarks(context.Context, int64) ([]federation.StoredMark, error) {
	return nil, nil
}

// The probe publishes no record of its own: it has no friendships to describe,
// and an outsider that gossiped would stop being an outsider.
func (emptyStore) PublishFriendList(context.Context) (bool, error) { return false, nil }

// The probe caches no catalogs, so there is never a claim of anyone's to check
// against bytes it holds — it holds none either (F6, contradicted identity
// claims).
func (emptyStore) CheckSourceClaims(context.Context, int64) (int, error) { return 0, nil }
func (emptyStore) ListClaimReports(context.Context) ([]*federation.ClaimReport, error) {
	return nil, nil
}
func (emptyStore) SetClaimReportDisposition(context.Context, int64, string) error { return nil }

// probe is the outsider node plus an HTTP client dialling through its netstack.
type probe struct {
	node   *federation.Node
	client *http.Client
}

// startProbe brings the outsider up, peered into peerURI (a lab node's underlay
// listener, not a fault link — see the file comment).
func startProbe(root, peerURI string, logger *log.Logger) (*probe, error) {
	dir := filepath.Join(root, "probe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	n, err := federation.Start(config.FederationConfig{
		Enabled: true,
		Name:    "meshlab-probe",
		KeyFile: filepath.Join(dir, "federation.key"),
		Peers:   []string{peerURI},
	}, emptyStore{}, logger)
	if err != nil {
		return nil, fmt.Errorf("probe node: %w", err)
	}
	p := &probe{
		node: n,
		client: &http.Client{
			Transport: &http.Transport{DialContext: n.DialContext},
			Timeout:   30 * time.Second,
		},
	}
	logger.Printf("probe: outsider node up, key %s… (friends: none, by design)", short(n.PublicKeyHex()))
	return p, nil
}

func (p *probe) stop() {
	if p != nil && p.node != nil {
		p.node.Stop()
	}
}

// get asks one lab node's mesh port, as the outsider. It returns the status and
// the body so a caller can both assert the code and verify bytes.
func (p *probe) get(target *node, path string) (int, []byte, error) {
	addr, err := federation.AddrForKeyHex(target.publicKey())
	if err != nil {
		return 0, nil, fmt.Errorf("mesh address of %s: %w", target.name, err)
	}
	url := fmt.Sprintf("http://[%s]:%d%s", addr, federation.MeshPort, path)
	resp, err := p.client.Get(url)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	return resp.StatusCode, body, nil
}

// wait polls a node's mesh ping until the probe can reach it, so an assertion
// never fails merely because the mesh had not converged yet.
//
// Ping is the right readiness signal precisely because it is NOT friends-gated:
// meshAuth refuses only BLOCKED peers, so a stranger reaching ping proves the
// path works and leaves every authorization question still to be asked.
func (p *probe) wait(target *node, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		code, _, err := p.get(target, "/madnetwork/v0/ping")
		if err == nil && code == http.StatusOK {
			return nil
		}
		if err != nil {
			last = err
		} else {
			last = fmt.Errorf("ping returned %d", code)
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("probe could not reach %s within %v: %v", target.name, timeout, last)
}

// sha256Hex is how the probe checks that guest-open bytes are the real thing —
// the open swarm has to serve the CONTENT, not just a 200.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
