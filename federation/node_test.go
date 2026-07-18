//go:build !nofederation

package federation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"daemonlord.ygg/madshare/config"
)

// The key file is the node's durable identity: two loads of the same file must
// yield the same key, a fresh path must create one (0600), and corruption must
// be a hard error, not a silent new identity.
func TestLoadOrCreateKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "federation.key")

	first, err := loadOrCreateKey(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("key file not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file mode = %o, want 600", perm)
	}

	second, err := loadOrCreateKey(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !bytes.Equal(first.PrivateKey, second.PrivateKey) {
		t.Error("reload produced a different key — identity is not durable")
	}

	if err := os.WriteFile(path, []byte("not a pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateKey(path); err == nil {
		t.Error("corrupt key file accepted; want an error (silent new identity would orphan the node's madnetwork relationships)")
	}
}

// End-to-end skeleton test: two embedded nodes, B peering to A over a loopback
// underlay, B fetching A's protocol ping through the mesh. This is the F0 spike
// as a regression test — it proves identity, transport (no TUN), and the
// protocol listener in one pass.
func TestPingOverMesh(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	dir := t.TempDir()

	// Reserve a loopback port for the underlay peering between the two nodes.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	underlay := fmt.Sprintf("tcp://%s", probe.Addr())
	probe.Close()

	a, err := Start(config.FederationConfig{
		KeyFile: filepath.Join(dir, "a.key"),
		Listen:  []string{underlay},
	}, nil, logger)
	if err != nil {
		t.Fatalf("start node A: %v", err)
	}
	defer a.Stop()

	b, err := Start(config.FederationConfig{
		KeyFile: filepath.Join(dir, "b.key"),
		Peers:   []string{underlay},
	}, nil, logger)
	if err != nil {
		t.Fatalf("start node B: %v", err)
	}
	defer b.Stop()

	client := &http.Client{
		Transport: &http.Transport{DialContext: b.DialContext},
		Timeout:   5 * time.Second,
	}
	url := fmt.Sprintf("http://[%s]:%d/madnetwork/v0/ping", a.Address(), MeshPort)

	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := client.Get(url)
		if err != nil {
			if time.Now().After(deadline) {
				t.Fatalf("ping never succeeded: %v", err)
			}
			time.Sleep(250 * time.Millisecond)
			continue
		}
		var ping struct {
			Protocol  int    `json:"protocol"`
			Software  string `json:"software"`
			PublicKey string `json:"public_key"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&ping); err != nil {
			t.Fatalf("decode ping: %v", err)
		}
		resp.Body.Close()
		if ping.Protocol != ProtocolVersion {
			t.Errorf("ping protocol = %d, want %d", ping.Protocol, ProtocolVersion)
		}
		if ping.Software != "Madshare" {
			t.Errorf("ping software = %q, want Madshare", ping.Software)
		}
		if ping.PublicKey != a.PublicKeyHex() {
			t.Errorf("ping public_key = %q, want node A's key %q", ping.PublicKey, a.PublicKeyHex())
		}
		return
	}
}
