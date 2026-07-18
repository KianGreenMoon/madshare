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

// MeshPort is the fixed TCP port of the madnetwork protocol listener on every
// node's mesh address (the port lives inside the embedded netstack, so it can
// never collide with anything on the host). 1314 spells MAD (M=13, A=1, D=4).
const MeshPort = 1314

// ProtocolVersion is the madnetwork protocol generation, exchanged in ping so
// incompatible peers can refuse each other early.
const ProtocolVersion = 0
