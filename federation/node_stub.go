//go:build nofederation

// Stub used when madshare is compiled with -tags nofederation, producing a
// standalone server with no yggdrasil/gVisor dependencies. main checks
// Available before calling Start, so the stub only has to satisfy the compiler.
package federation

import (
	"context"
	"errors"
	"log"
	"net"

	"daemonlord.ygg/madshare/config"
)

// Available is false in -tags nofederation builds; see node.go for the real build.
const Available = false

// Node is a placeholder; no instance is ever created in nofederation builds.
type Node struct{}

// Start always fails in nofederation builds.
func Start(config.FederationConfig, *log.Logger) (*Node, error) {
	return nil, errors.New("federation compiled out (-tags nofederation)")
}

func (n *Node) Stop()                {}
func (n *Node) Address() net.IP     { return nil }
func (n *Node) PublicKeyHex() string { return "" }

func (n *Node) DialContext(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("federation compiled out (-tags nofederation)")
}
