package app_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"daemonlord.ygg/madshare/app"
)

// The pairing surface (app/pairing.go): an embedder's node performing the same
// acts as /admin/network. These tests cover the facade's own contract — the
// state machine behind it is federation's and tested there.

// TestPairingIsAbsentWithoutAMesh mirrors Network's rule: a configuration with
// no node says so once, at startup.
func TestPairingIsAbsentWithoutAMesh(t *testing.T) {
	cfg, _ := embeddedConfig(t, t.TempDir())
	lg, out := testLogger()
	inst, err := app.Start(context.Background(), cfg, app.WithLogger(lg))
	if err != nil {
		t.Fatalf("Start: %v\nlog:\n%s", err, out)
	}
	defer inst.Stop(context.Background())

	if _, ok := inst.Pairing(); ok {
		t.Error("Pairing() is available with federation disabled; want not ok")
	}
}

// TestPairingImportsAndForgets walks the embedder's whole loop against a live
// node: read the own card, import a stranger's key, see the pending_outgoing
// row, and remove it again.
func TestPairingImportsAndForgets(t *testing.T) {
	cfg := meshConfig(t, t.TempDir())
	lg, out := testLogger()
	inst, err := app.Start(context.Background(), cfg, app.WithLogger(lg))
	if err != nil {
		t.Fatalf("Start: %v\nlog:\n%s", err, out)
	}
	defer inst.Stop(context.Background())

	p, ok := inst.Pairing()
	if !ok {
		t.Fatalf("Pairing() not available with the mesh on\nlog:\n%s", out)
	}

	info := p.Info()
	if len(info.PublicKey) != 64 {
		t.Fatalf("Info().PublicKey = %q, want 64 hex characters", info.PublicKey)
	}
	raw, err := json.Marshal(info.Card)
	if err != nil {
		t.Fatalf("marshalling the own card: %v", err)
	}
	if !strings.Contains(string(raw), "madshare_node_card") {
		t.Fatalf("own card %s carries no format marker", raw)
	}

	// A different (well-formed) key: the own key with its first byte flipped.
	other := "00" + info.PublicKey[2:]
	if other == info.PublicKey {
		other = "01" + info.PublicKey[2:]
	}
	peer, err := p.ImportKey(context.Background(), other, "test peer")
	if err != nil {
		t.Fatalf("ImportKey: %v", err)
	}
	if peer.TrustState != "pending_outgoing" {
		t.Errorf("imported peer state = %q, want pending_outgoing", peer.TrustState)
	}

	peers, err := p.Peers(context.Background())
	if err != nil {
		t.Fatalf("Peers: %v", err)
	}
	if len(peers) != 1 || peers[0].PublicKey != other {
		t.Fatalf("Peers = %+v, want exactly the imported key", peers)
	}

	if err := p.RemovePeer(context.Background(), peers[0].ID); err != nil {
		t.Fatalf("RemovePeer: %v", err)
	}
	peers, err = p.Peers(context.Background())
	if err != nil {
		t.Fatalf("Peers after remove: %v", err)
	}
	if len(peers) != 0 {
		t.Fatalf("Peers after remove = %+v, want none", peers)
	}
}
