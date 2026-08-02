package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"daemonlord.ygg/madshare/config"
)

func TestLoad_Federation_Defaults(t *testing.T) {
	cfg, err := config.Load("/tmp/definitely_no_such_file_12345.toml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Federation.Enabled {
		t.Error("federation should be disabled by default")
	}
	// The node key belongs to the transport now ([yggdrasil].key_file); the path
	// and the default are unchanged, because it IS the node's identity and
	// renaming it would orphan every existing node.
	if cfg.Yggdrasil.KeyFile != filepath.Join("data", "federation.key") {
		t.Errorf("KeyFile = %q, want data/federation.key (derived from data_dir)", cfg.Yggdrasil.KeyFile)
	}
}

func TestLoad_Federation_ExplicitKeyFileOverride(t *testing.T) {
	f := filepath.Join(t.TempDir(), "fed.toml")
	body := validListeners + "[federation]\nkey_file = \"/etc/madshare/node.key\"\n"
	if err := os.WriteFile(f, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(f)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Written under the deprecated [federation] alias, so it must fold through to
	// the transport that actually reads it.
	if cfg.Yggdrasil.KeyFile != "/etc/madshare/node.key" {
		t.Errorf("KeyFile = %q, want the explicit override", cfg.Yggdrasil.KeyFile)
	}
}

// URI syntax is validated even with federation disabled, so typos surface
// immediately rather than on the day the flag is flipped.
func TestLoad_Federation_InvalidURIs(t *testing.T) {
	cases := []struct {
		name, body string
	}{
		{"garbage peer", "[federation]\npeers = [\"not a uri\"]\n"},
		{"unknown scheme", "[federation]\npeers = [\"carrier-pigeon://host:1\"]\n"},
		{"schemeless", "[federation]\nlisten = [\"0.0.0.0:123\"]\n"},
		{"dial-only scheme in listen", "[federation]\nlisten = [\"socks://127.0.0.1:1080\"]\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := filepath.Join(t.TempDir(), "fed.toml")
			if err := os.WriteFile(f, []byte(validListeners+tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := config.Load(f); err == nil {
				t.Errorf("Load accepted %q, want error", tc.body)
			}
		})
	}
}

func TestLoad_Federation_ValidURIs(t *testing.T) {
	f := filepath.Join(t.TempDir(), "fed.toml")
	body := validListeners + "[federation]\nenabled = true\n" +
		"peers = [\"tls://peer.example:12345\", \"socks://127.0.0.1:1080/peer:1\", \"unix:///var/run/ygg.sock\"]\n" +
		"listen = [\"tcp://0.0.0.0:29871\", \"quic://[::]:29871\"]\n"
	if err := os.WriteFile(f, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(f)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, w := range cfg.Warnings() {
		if strings.Contains(w, "federation") {
			t.Errorf("unexpected federation warning: %q", w)
		}
	}
}

func TestLoad_Federation_EnabledWithoutPeersWarns(t *testing.T) {
	f := filepath.Join(t.TempDir(), "fed.toml")
	if err := os.WriteFile(f, []byte(validListeners+"[federation]\nenabled = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(f)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	found := false
	for _, w := range cfg.Warnings() {
		if strings.Contains(w, "yggdrasil") && strings.Contains(w, "unreachable") {
			found = true
		}
	}
	if !found {
		t.Errorf("want an isolated-node warning, got %v", cfg.Warnings())
	}
}
