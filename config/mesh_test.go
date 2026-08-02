package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"daemonlord.ygg/madshare/config"
)

// load writes body to a temp config and loads it, failing the test on error.
func load(t *testing.T, body string) config.Config {
	t.Helper()
	f := filepath.Join(t.TempDir(), "mesh.toml")
	if err := os.WriteFile(f, []byte(validListeners+body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(f)
	if err != nil {
		t.Fatalf("Load(%q): %v", body, err)
	}
	return cfg
}

// loadErr expects Load to refuse body, and returns the error text.
func loadErr(t *testing.T, body string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "mesh.toml")
	if err := os.WriteFile(f, []byte(validListeners+body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(f)
	if err == nil {
		t.Fatalf("Load accepted %q (mesh enabled = %v), want an error", body, cfg.MeshEnabled())
	}
	return err.Error()
}

// The gate: the mesh is a smaller thing to ask for than federation, and either
// key turns it on. The one combination with no coherent reading is refused.
func TestMeshEnabled(t *testing.T) {
	cases := []struct {
		name, body string
		want       bool
	}{
		{"nothing set", "", false},
		{"federation on implies the transport", "[federation]\nenabled = true\n", true},
		{"transport alone", "[yggdrasil]\nenabled = true\n", true},
		{"transport off alone", "[yggdrasil]\nenabled = false\n", false},
		{"both on", "[federation]\nenabled = true\n[yggdrasil]\nenabled = true\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := load(t, tc.body).MeshEnabled(); got != tc.want {
				t.Errorf("MeshEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// Federation is served over the mesh and has no other transport, so switching
// the mesh off under it is a contradiction rather than a narrowing — and must
// say so instead of silently picking a side.
func TestMeshFederationWithoutTransportIsRefused(t *testing.T) {
	err := loadErr(t, "[federation]\nenabled = true\n[yggdrasil]\nenabled = false\n")
	for _, want := range []string{"[federation].enabled", "[yggdrasil].enabled = false"} {
		if !strings.Contains(err, want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestMeshListenerNeedsTheMesh(t *testing.T) {
	err := loadErr(t, "[[listen_mesh]]\nenabled = true\nport = 80\nserve = [\"api\"]\n")
	// The refusal has to name both ways out, since the whole point of the split
	// is that federating is not required to be reachable.
	for _, want := range []string{"[yggdrasil].enabled", "[federation].enabled"} {
		if !strings.Contains(err, want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestMeshListenerDefaults(t *testing.T) {
	cfg := load(t, "[yggdrasil]\nenabled = true\n[[listen_mesh]]\nenabled = true\nserve = [\"api\", \"webui\"]\n")
	if len(cfg.MeshListeners()) != 1 {
		t.Fatalf("MeshListeners = %v, want one entry", cfg.MeshListeners())
	}
	if got := cfg.ListenMesh[0].Port; got != config.DefaultMeshListenPort {
		t.Errorf("port = %d, want the default %d", got, config.DefaultMeshListenPort)
	}
	if !cfg.ListenMesh[0].Serves(config.GroupWebUI) {
		t.Error("Serves(webui) = false")
	}
	if cfg.ListenMesh[0].Serves(config.GroupAdmin) {
		t.Error("Serves(admin) = true, want false — it was not listed")
	}
}

// Writing the block is not enough. A mesh listener's address is derived rather
// than chosen and its audience is the whole Yggdrasil network, so it takes a
// second, explicit yes — even with federation on and the mesh already up.
func TestMeshListenerIsOptInPerBlock(t *testing.T) {
	cfg := load(t, "[federation]\nenabled = true\npeers = [\"tls://p:1\"]\n"+
		"[[listen_mesh]]\nport = 80\nserve = [\"api\", \"webui\"]\n")
	if len(cfg.ListenMesh) != 1 {
		t.Fatalf("ListenMesh = %v, want the block to be parsed", cfg.ListenMesh)
	}
	if got := cfg.MeshListeners(); len(got) != 0 {
		t.Errorf("MeshListeners() = %v, want none — the block never said enabled = true", got)
	}
	// Silently doing nothing is the failure mode this default invites, so it
	// must not be silent.
	found := false
	for _, w := range cfg.Warnings() {
		if strings.Contains(w, "listen_mesh[0]") && strings.Contains(w, "enabled = true") {
			found = true
		}
	}
	if !found {
		t.Errorf("want a not-served advisory, got %v", cfg.Warnings())
	}
}

func TestMeshListenerEnabledIsServed(t *testing.T) {
	cfg := load(t, "[yggdrasil]\nenabled = true\npeers = [\"tls://p:1\"]\n"+
		"[[listen_mesh]]\nenabled = true\nport = 80\nserve = [\"api\"]\n")
	live := cfg.MeshListeners()
	if len(live) != 1 || live[0].Index != 0 || live[0].Port != 80 {
		t.Fatalf("MeshListeners() = %v, want the one enabled block at index 0", live)
	}
	for _, w := range cfg.Warnings() {
		if strings.Contains(w, "not served") {
			t.Errorf("unexpected not-served advisory: %q", w)
		}
	}
}

// A block that is off is still a block: its schema is checked now, so a typo
// does not lie dormant until the day it is switched on.
func TestMeshDisabledListenerIsStillValidated(t *testing.T) {
	got := loadErr(t, "[yggdrasil]\nenabled = true\n[[listen_mesh]]\nport = 1314\nserve = [\"api\"]\n")
	if !strings.Contains(got, "reserved for the madnetwork protocol") {
		t.Errorf("error = %q, want the reserved-port refusal even for a disabled block", got)
	}
}

// ...but a block that serves nothing needs nothing, so it does not drag the
// mesh requirement in with it.
func TestMeshDisabledListenerNeedsNoMesh(t *testing.T) {
	cfg := load(t, "[[listen_mesh]]\nport = 80\nserve = [\"api\"]\n")
	if cfg.MeshEnabled() {
		t.Error("MeshEnabled() = true, want false — nothing asked for the mesh")
	}
	if got := cfg.MeshListeners(); len(got) != 0 {
		t.Errorf("MeshListeners() = %v, want none", got)
	}
}

func TestMeshListenerValidation(t *testing.T) {
	const on = "[yggdrasil]\nenabled = true\n"
	cases := []struct{ name, body, wantErr string }{
		{
			"protocol port is reserved",
			on + "[[listen_mesh]]\nport = 1314\nserve = [\"api\"]\n",
			"reserved for the madnetwork protocol",
		},
		{
			"port out of range",
			on + "[[listen_mesh]]\nport = 70000\nserve = [\"api\"]\n",
			"out of range",
		},
		{
			"duplicate mesh port",
			on + "[[listen_mesh]]\nport = 80\nserve = [\"api\"]\n[[listen_mesh]]\nport = 80\nserve = [\"webui\"]\n",
			"duplicate mesh port",
		},
		{
			"empty serve",
			on + "[[listen_mesh]]\nport = 80\nserve = []\n",
			"must list at least one group",
		},
		{
			"unknown group",
			on + "[[listen_mesh]]\nport = 80\nserve = [\"gossip\"]\n",
			"unknown group",
		},
		{
			"bad allow_from",
			on + "[[listen_mesh]]\nport = 80\nserve = [\"api\"]\nallow_from = [\"nonsense\"]\n",
			"invalid CIDR",
		},
		{
			// A mesh address is derived from the node key, so there is nothing to
			// choose here — and an operator reaching for `addr` by analogy with
			// [[listen]] must be told, not silently ignored.
			"addr is not a mesh listener key",
			on + "[[listen_mesh]]\naddr = \"127.0.0.1\"\nport = 80\nserve = [\"api\"]\n",
			"derived from its key",
		},
		{
			"typo in the yggdrasil section",
			"[yggdrasil]\nenabled = true\npeerz = [\"tls://h:1\"]\n",
			"unknown key",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := loadErr(t, tc.body); !strings.Contains(got, tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", got, tc.wantErr)
			}
		})
	}
}

// The mesh port lives in a different address space from the kernel sockets, so
// reusing 80 (or 3000) on both sides is not a conflict.
func TestMeshPortDoesNotCollideWithHostPort(t *testing.T) {
	cfg := load(t, "[yggdrasil]\nenabled = true\n[[listen_mesh]]\nenabled = true\nport = 3000\nserve = [\"api\"]\n")
	if len(cfg.ListenMesh) != 1 {
		t.Fatalf("ListenMesh = %v, want one entry", cfg.ListenMesh)
	}
}

// Every config written before the transport was separable must keep working
// untouched: the [federation] transport keys still resolve.
func TestMeshTransportKeysFoldFromFederation(t *testing.T) {
	cfg := load(t, "[federation]\nenabled = true\n"+
		"key_file = \"/etc/madshare/node.key\"\n"+
		"peers = [\"tls://peer.example:12345\"]\n"+
		"listen = [\"tcp://0.0.0.0:29871\"]\n")
	if cfg.Yggdrasil.KeyFile != "/etc/madshare/node.key" {
		t.Errorf("KeyFile = %q, want the [federation] value folded in", cfg.Yggdrasil.KeyFile)
	}
	if len(cfg.Yggdrasil.Peers) != 1 || cfg.Yggdrasil.Peers[0] != "tls://peer.example:12345" {
		t.Errorf("Peers = %v, want the [federation] value folded in", cfg.Yggdrasil.Peers)
	}
	if len(cfg.Yggdrasil.Listen) != 1 {
		t.Errorf("Listen = %v, want the [federation] value folded in", cfg.Yggdrasil.Listen)
	}
}

func TestMeshTransportKeysPreferYggdrasil(t *testing.T) {
	cfg := load(t, "[federation]\nenabled = true\n"+
		"peers = [\"tls://old.example:1\"]\n"+
		"[yggdrasil]\npeers = [\"tls://new.example:2\"]\n")
	if len(cfg.Yggdrasil.Peers) != 1 || cfg.Yggdrasil.Peers[0] != "tls://new.example:2" {
		t.Errorf("Peers = %v, want the explicit [yggdrasil] value to win", cfg.Yggdrasil.Peers)
	}
}

// A malformed URI is named in the section the operator actually wrote it in,
// even though resolveMesh has since copied it into the other one.
func TestMeshURIErrorNamesTheWrittenSection(t *testing.T) {
	if got := loadErr(t, "[federation]\npeers = [\"carrier-pigeon://h:1\"]\n"); !strings.Contains(got, "federation.peers") {
		t.Errorf("error = %q, want it to name federation.peers", got)
	}
	if got := loadErr(t, "[yggdrasil]\npeers = [\"carrier-pigeon://h:1\"]\n"); !strings.Contains(got, "yggdrasil.peers") {
		t.Errorf("error = %q, want it to name yggdrasil.peers", got)
	}
}

// Serving admin to the whole Yggdrasil network is allowed — it is the
// single-operator case the listener exists for — but it is not silent.
func TestMeshAdminWarns(t *testing.T) {
	cfg := load(t, "[yggdrasil]\nenabled = true\npeers = [\"tls://p:1\"]\n"+
		"[[listen_mesh]]\nenabled = true\nport = 80\nserve = [\"api\", \"webui\", \"admin\"]\n")
	found := false
	for _, w := range cfg.Warnings() {
		if strings.Contains(w, "listen_mesh[0]") && strings.Contains(w, "Yggdrasil network") {
			found = true
		}
	}
	if !found {
		t.Errorf("want an admin-exposure warning, got %v", cfg.Warnings())
	}
}

func TestMeshWithoutAdminDoesNotWarn(t *testing.T) {
	cfg := load(t, "[yggdrasil]\nenabled = true\npeers = [\"tls://p:1\"]\n"+
		"[[listen_mesh]]\nenabled = true\nport = 80\nserve = [\"api\", \"webui\"]\n")
	for _, w := range cfg.Warnings() {
		if strings.Contains(w, "listen_mesh") {
			t.Errorf("unexpected warning: %q", w)
		}
	}
}
