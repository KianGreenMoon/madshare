package federation

import (
	"testing"

	"daemonlord.ygg/madshare/config"
)

// config duplicates MeshPort because the dependency only runs one way (federation
// imports config, never the reverse), and it uses the copy to refuse a
// [[listen_mesh]] entry that would claim the madnetwork protocol's port on the
// very same address. A drift here would let that config through and the bind
// would fail at startup instead — with an error about a busy port rather than
// about a reserved one.
func TestMeshPortMatchesConfig(t *testing.T) {
	if MeshPort != config.MeshProtocolPort {
		t.Fatalf("federation.MeshPort = %d but config.MeshProtocolPort = %d; "+
			"the [[listen_mesh]] port reservation is checked against the config copy",
			MeshPort, config.MeshProtocolPort)
	}
}
