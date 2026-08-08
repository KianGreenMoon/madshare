package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Peer sharing (docs/architecture/federation.md §"The household", "Getting onto
// the mesh at all"): what [yggdrasil].share_peers / shared_peers resolve to, and
// the one distinction the whole shape rests on — an absent list is not an empty
// one.

func loadTOML(t *testing.T, body string) (Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "madshare.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

// TestSharedPeersDefaultToPeers: a node shares the way it connects, which is the
// answer that is right without anybody deciding anything.
func TestSharedPeersDefaultToPeers(t *testing.T) {
	cfg, err := loadTOML(t, `
[[listen]]
port = 3000
serve = ["api"]

[yggdrasil]
peers = ["tls://one.example:1", "tls://two.example:2"]
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.SharesPeers() {
		t.Error("share_peers defaults to false; want true")
	}
	if len(cfg.Yggdrasil.SharedPeers) != 2 ||
		cfg.Yggdrasil.SharedPeers[0] != "tls://one.example:1" {
		t.Errorf("shared_peers = %v, want the configured peers", cfg.Yggdrasil.SharedPeers)
	}
}

// TestSharedPeersEmptyListSharesNothing is the distinction that makes this a
// list rather than a flag: absent inherits peers, [] is a decision to share
// none. Collapsing the two would leave no way to run the endpoint while sending
// devices only to the listeners.
func TestSharedPeersEmptyListSharesNothing(t *testing.T) {
	cfg, err := loadTOML(t, `
[[listen]]
port = 3000
serve = ["api"]

[yggdrasil]
peers = ["tls://one.example:1"]
shared_peers = []
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Yggdrasil.SharedPeers) != 0 {
		t.Errorf("shared_peers = %v, want none — an explicit empty list is a choice",
			cfg.Yggdrasil.SharedPeers)
	}
}

// TestSharedPeersOverrideAndSwitch: an explicit list replaces peers entirely,
// and share_peers = false is distinguishable from absent.
func TestSharedPeersOverrideAndSwitch(t *testing.T) {
	cfg, err := loadTOML(t, `
[[listen]]
port = 3000
serve = ["api"]

[yggdrasil]
peers = ["tls://private.internal:1"]
share_peers = false
shared_peers = ["tls://public.example:2"]
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.SharesPeers() {
		t.Error("share_peers = false was not honoured")
	}
	if len(cfg.Yggdrasil.SharedPeers) != 1 || cfg.Yggdrasil.SharedPeers[0] != "tls://public.example:2" {
		t.Errorf("shared_peers = %v, want only the explicit entry", cfg.Yggdrasil.SharedPeers)
	}
}

// TestSharedPeersInheritTheDeprecatedSection: a node that still writes its peers
// under [federation] shares those, because resolveMesh folds the alias first.
func TestSharedPeersInheritTheDeprecatedSection(t *testing.T) {
	cfg, err := loadTOML(t, `
[[listen]]
port = 3000
serve = ["api"]

[federation]
peers = ["tls://old.example:1"]
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Yggdrasil.SharedPeers) != 1 || cfg.Yggdrasil.SharedPeers[0] != "tls://old.example:1" {
		t.Errorf("shared_peers = %v, want the aliased peer", cfg.Yggdrasil.SharedPeers)
	}
}

// TestSharedPeersAreValidated: a typo here fails on somebody else's device,
// where the operator cannot see it — so it fails here instead, and it does so
// whether or not sharing is currently on.
func TestSharedPeersAreValidated(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"sharing on", `shared_peers = ["not a uri"]`},
		{"sharing off", "share_peers = false\n" + `shared_peers = ["not a uri"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadTOML(t, "[[listen]]\nport = 3000\nserve = [\"api\"]\n\n[yggdrasil]\n"+tc.body+"\n")
			if err == nil || !strings.Contains(err.Error(), "shared_peers") {
				t.Errorf("load error = %v, want one naming shared_peers", err)
			}
		})
	}
}

// TestMulticastIsOffUnlessAsked: a divergence from upstream yggdrasil, and the
// reason is who is at the keyboard — a server has an operator who wrote a peer
// list, and auto-peering with whatever else is on the network is a thing to ask
// for rather than inherit.
func TestMulticastIsOffUnlessAsked(t *testing.T) {
	cfg, err := loadTOML(t, "[[listen]]\nport = 3000\nserve = [\"api\"]\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Yggdrasil.Multicast {
		t.Error("multicast defaults on; want off")
	}
	cfg, err = loadTOML(t, "[[listen]]\nport = 3000\nserve = [\"api\"]\n\n[yggdrasil]\nmulticast = true\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.Yggdrasil.Multicast {
		t.Error("multicast = true was not honoured")
	}
}

// TestUnknownYggdrasilKeyIsFatal: the new keys must not turn a typo into silence
// — the section's whole schema is closed, and the message names what is valid.
func TestUnknownYggdrasilKeyIsFatal(t *testing.T) {
	_, err := loadTOML(t, "[[listen]]\nport = 3000\nserve = [\"api\"]\n\n[yggdrasil]\nshare_peer = true\n")
	if err == nil || !strings.Contains(err.Error(), "share_peers") {
		t.Errorf("load error = %v, want an unknown-key error listing the valid keys", err)
	}
}
