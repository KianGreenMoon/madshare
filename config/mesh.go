package config

import (
	"fmt"
	"net"
	"path/filepath"
	"slices"
	"strings"
)

// MeshProtocolPort mirrors federation.MeshPort, the fixed port of the madnetwork
// protocol listener on every node's mesh address. It is duplicated here because
// the federation package imports config, so config cannot import it back;
// federation.TestMeshPortMatchesConfig pins the two together.
//
// A [[listen_mesh]] entry may not claim it — see validateMesh.
const MeshProtocolPort = 1314

// DefaultMeshListenPort is the port a [[listen_mesh]] entry binds when it names
// none. 80 is the point of the feature rather than a convention: nothing in the
// userspace netstack makes a low port privileged (it is not a kernel socket), so
// the address can be handed out with no ":port" suffix at all.
const DefaultMeshListenPort = 80

// YggdrasilConfig is the mesh *transport*: the identity key, the underlay
// peering, and therefore the node's address. [federation] keeps what is a
// madnetwork *feature* — friendship, catalogs, scope, discovery, quotas,
// seeding.
//
// The split falls where the concepts divide. A key and a set of peers are what
// put a node on the mesh and give it an address; friendship and catalogs are
// what it then chooses to do there. Serving the web UI over the mesh
// ([[listen_mesh]]) needs the first half and none of the second, which is why
// the transport can be enabled on its own. See docs/plans/mesh-listener.md §4.
type YggdrasilConfig struct {
	// Enabled brings the yggdrasil core and its userspace netstack up. It is a
	// *bool so that absent and explicitly-false are distinguishable: absent
	// means "infer it from [federation].enabled", false means the operator said
	// no — and combining that false with federation.enabled is a hard error
	// rather than a silent override (validateMesh). Resolve it via
	// Config.MeshEnabled rather than reading the pointer.
	Enabled *bool `toml:"enabled"`
	// KeyFile is the PEM ed25519 private key that IS this node's identity: the
	// mesh address derives from it, so losing the file means coming back as a
	// different node. Derived as <data_dir>/federation.key when unset — the name
	// is mildly off-topic now that the mesh outlives federation, but renaming it
	// would orphan every existing node's identity, and that is the one file that
	// must never move.
	KeyFile string `toml:"key_file"`
	// Peers are outbound underlay peering URIs (e.g. "tls://host:port") dialled
	// to join the mesh.
	Peers []string `toml:"peers"`
	// Listen are underlay listener URIs (e.g. "tls://0.0.0.0:12345") accepting
	// incoming peerings — backbone nodes only; a node behind NAT dials out via
	// Peers and needs no inbound anything.
	Listen []string `toml:"listen"`
}

// MeshListenConfig describes one HTTP listener on this node's *mesh* address:
// the same route groups as [[listen]], served over the federation netstack
// instead of a kernel socket.
//
// There is deliberately no Addr: a node has exactly one mesh address and derives
// it from its key, so an address here could only be ignored or wrong (Load
// rejects the key outright).
type MeshListenConfig struct {
	// Enabled serves this listener. It defaults to **false**, so writing the
	// block is not enough — a mesh listener has to be asked for twice.
	//
	// The asymmetry with [[listen]] is deliberate. A host listener's exposure is
	// written on its face: an operator who types 192.168.1.67 knows who can
	// reach it. A mesh listener's address is derived from a key rather than
	// chosen, and its audience is the whole Yggdrasil network — so the block is
	// easy to paste from an example, or to inherit from a config someone else
	// wrote, without that being a decision anybody made. A plain bool (not the
	// tri-state used for the transport) because there is nothing to infer here:
	// off is off, and the zero value is the safe one.
	Enabled bool `toml:"enabled"`
	// Port is the mesh-side TCP port; 0 resolves to DefaultMeshListenPort.
	Port int `toml:"port"`
	// Serve is the non-empty subset of route groups mounted on this listener,
	// using the same vocabulary as [[listen]].
	Serve []string `toml:"serve"`
	// AllowFrom optionally restricts accepted requests to these source CIDRs.
	//
	// It is accepted for symmetry with [[listen]] but is close to useless here:
	// mesh addresses are derived from node keys, not allocated in prefixes, so
	// there is no subnet to name. The filter operators actually want — friends
	// and community members only — is a separate piece of work over the
	// membership index (docs/plans/mesh-listener.md §8).
	AllowFrom []string `toml:"allow_from"`
}

// Serves reports whether this listener mounts the given route group.
func (m MeshListenConfig) Serves(group string) bool {
	return slices.Contains(m.Serve, group)
}

// MeshListeners returns the mesh listeners that will actually be served, paired
// with their index in the configured list so diagnostics can still name the
// block an operator wrote. Every consumer goes through this rather than ranging
// over Config.ListenMesh, so a disabled block cannot leak into one code path
// while being skipped in another.
func (c Config) MeshListeners() []IndexedMeshListener {
	var out []IndexedMeshListener
	for i, m := range c.ListenMesh {
		if m.Enabled {
			out = append(out, IndexedMeshListener{Index: i, MeshListenConfig: m})
		}
	}
	return out
}

// IndexedMeshListener is an enabled mesh listener and its position in the
// configured list.
type IndexedMeshListener struct {
	Index int
	MeshListenConfig
}

// MeshEnabled reports whether the yggdrasil transport should be started: either
// asked for directly, or implied by federation, which is served over the mesh
// and has no other route to a peer.
//
// The contradictory combination (federation on, yggdrasil explicitly off) is
// rejected by validateMesh, so every caller of this sees a coherent answer.
func (c Config) MeshEnabled() bool {
	if c.Yggdrasil.Enabled != nil {
		return *c.Yggdrasil.Enabled
	}
	return c.Federation.Enabled
}

// resolveMesh folds the deprecated [federation] transport keys into [yggdrasil],
// derives the key path from data_dir, and applies the default mesh port. It runs
// on every load, so everything downstream reads c.Yggdrasil alone and never has
// to know which section a value was written in.
//
// The aliases exist because these keys lived under [federation] before the
// transport was separable; a config written then must keep working untouched.
// An explicit [yggdrasil] value always wins.
func (c *Config) resolveMesh() {
	if c.Yggdrasil.KeyFile == "" {
		c.Yggdrasil.KeyFile = c.Federation.KeyFile
	}
	if c.Yggdrasil.Peers == nil {
		c.Yggdrasil.Peers = c.Federation.Peers
	}
	if c.Yggdrasil.Listen == nil {
		c.Yggdrasil.Listen = c.Federation.Listen
	}
	if c.Yggdrasil.KeyFile == "" && c.DataDir != "" {
		c.Yggdrasil.KeyFile = filepath.Join(c.DataDir, "federation.key")
	}
	for i := range c.ListenMesh {
		if c.ListenMesh[i].Port == 0 {
			c.ListenMesh[i].Port = DefaultMeshListenPort
		}
	}
}

// validateMesh enforces the mesh gate and the [[listen_mesh]] schema. The two
// refusals it can produce are written out at length because each is a place an
// operator lands with a plausible mental model that happens to be wrong.
func (c Config) validateMesh() error {
	if c.Federation.Enabled && c.Yggdrasil.Enabled != nil && !*c.Yggdrasil.Enabled {
		return fmt.Errorf("config: [federation].enabled is set but [yggdrasil].enabled = false.\n" +
			"  Madnetwork IS served over the Yggdrasil mesh — peers reach this node at its mesh\n" +
			"  address and there is no other transport — so federation cannot run with the mesh\n" +
			"  switched off. Remove [yggdrasil].enabled to let federation bring the mesh up, or\n" +
			"  set [federation].enabled = false to turn both off.")
	}
	if len(c.ListenMesh) == 0 {
		return nil
	}
	// Schema is checked on every block, enabled or not, so a typo surfaces now
	// rather than on the day the listener is switched on. Only the mesh
	// requirement below is scoped to the enabled ones — a block that serves
	// nothing needs nothing.
	if live := c.MeshListeners(); len(live) > 0 && !c.MeshEnabled() {
		return fmt.Errorf("config: listen_mesh[%d] needs the Yggdrasil mesh, but neither [yggdrasil].enabled\n"+
			"  nor [federation].enabled is set. The address a listen_mesh block binds is this node's\n"+
			"  own mesh address, which exists only while the mesh is running. Set\n"+
			"  [yggdrasil].enabled = true for a reachable server that federates with nobody, or\n"+
			"  [federation].enabled = true to join the madnetwork too (README, \"Deploying a\n"+
			"  madnetwork node\"). To bind an ordinary host address instead, use [[listen]].", live[0].Index)
	}
	seenPorts := map[int]bool{}
	for i, m := range c.ListenMesh {
		if m.Port < 1 || m.Port > 65535 {
			return fmt.Errorf("config: listen_mesh[%d].port %d out of range 1..65535", i, m.Port)
		}
		// Reserved unconditionally, including in the transport-only mode where
		// nothing is actually serving it: a config that works must not stop
		// working the day its operator enables federation.
		if m.Port == MeshProtocolPort {
			return fmt.Errorf("config: listen_mesh[%d].port %d is reserved for the madnetwork protocol "+
				"on this node's mesh address; pick another port", i, m.Port)
		}
		if seenPorts[m.Port] {
			return fmt.Errorf("config: listen_mesh[%d] duplicate mesh port %d", i, m.Port)
		}
		seenPorts[m.Port] = true
		if len(m.Serve) == 0 {
			return fmt.Errorf("config: listen_mesh[%d].serve must list at least one group", i)
		}
		for _, g := range m.Serve {
			if !knownGroups[g] {
				return fmt.Errorf("config: listen_mesh[%d].serve has unknown group %q (valid: %s, %s, %s)",
					i, g, GroupAPI, GroupWebUI, GroupAdmin)
			}
		}
		for _, cidr := range m.AllowFrom {
			if _, _, err := net.ParseCIDR(cidr); err != nil {
				return fmt.Errorf("config: listen_mesh[%d].allow_from has invalid CIDR %q: %w", i, cidr, err)
			}
		}
	}
	return nil
}

// meshWarnings are the non-fatal advisories about mesh listeners, folded into
// Warnings.
func (c Config) meshWarnings() []string {
	var w []string
	// A block that is written but off is the likeliest way to arrive at a mesh
	// listener that silently does nothing (uncommenting the example gets you
	// here), so name it rather than leaving the operator to wonder. The
	// counterpart of defaulting Enabled to false.
	for i, m := range c.ListenMesh {
		if !m.Enabled {
			w = append(w, fmt.Sprintf("listen_mesh[%d] is configured but not served; set enabled = true on that block", i))
		}
	}
	for _, m := range c.MeshListeners() {
		if m.Serves(GroupWebUI) && !m.Serves(GroupAPI) {
			w = append(w, fmt.Sprintf("listen_mesh[%d] serves %q without %q; the web UI would load but its API calls would 404",
				m.Index, GroupWebUI, GroupAPI))
		}
		// Not an error: administering your own node from your phone is exactly
		// what this listener is for, and the admin API is permission-gated
		// regardless. But its audience is every node on the Yggdrasil network,
		// which is a larger blast radius than the LAN address an operator is
		// used to typing, so it is worth a line rather than a discovery.
		if m.Serves(GroupAdmin) {
			w = append(w, fmt.Sprintf("listen_mesh[%d] serves %q on this node's mesh address, which is reachable "+
				"from the whole Yggdrasil network — authentication is the only gate there", m.Index, GroupAdmin))
		}
	}
	return w
}

// rejectUnknownMeshKeys turns a typo in the two new sections into a startup
// error instead of a silent no-op. Scoped to [yggdrasil] and [[listen_mesh]]
// because they are new — no existing config can trip it — and because the key
// this most needs to catch is `addr` on a mesh listener, which an operator will
// reach for by analogy with [[listen]] and which cannot mean anything here.
func rejectUnknownMeshKeys(undecoded []string) error {
	for _, key := range undecoded {
		head, _, _ := strings.Cut(key, ".")
		switch head {
		case "yggdrasil":
			return fmt.Errorf("config: unknown key %q in [yggdrasil] "+
				"(valid: enabled, key_file, peers, listen)", key)
		case "listen_mesh":
			if strings.HasSuffix(key, ".addr") {
				return fmt.Errorf("config: %q is not a valid key: a [[listen_mesh]] entry binds this node's "+
					"own mesh address, which is derived from its key and cannot be chosen", key)
			}
			return fmt.Errorf("config: unknown key %q in [[listen_mesh]] "+
				"(valid: enabled, port, serve, allow_from)", key)
		}
	}
	return nil
}
