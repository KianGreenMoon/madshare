//go:build !nofederation

package federation

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"
)

// The reactive down-mark (docs/architecture/federation.md §Availability,
// "Reactive down-mark + the ping floor"; build order in
// docs/plans/availability.md Phase 5) — the *negative* half of liveness.
//
// Positive contact has always been recorded: a ping, a catalog pull, a
// delivered chunk each advance last_seen (observePeerAlive, freshness.go). A
// failure had nowhere to go. The scheduler de-ranked the holder for the one
// transfer that tried it and the knowledge died there, so the browse went on
// offering a dead member's exclusively-held tracks until the pull window ran
// out — up to 45 minutes, and every reader who clicked Play re-paid the
// discovery.
//
// Three rules make a failure safe to record, and all three are about what a
// failure is *evidence of*:
//
//   - **Connect-class only.** A dial that timed out, was refused, or found no
//     route says the node is not there. A read stall, a corrupt chunk or a slow
//     body says something about the transfer, which the scheduler already owns.
//     Any HTTP answer — a 429 included — is proof of life and advances
//     last_seen instead: a member protecting itself with quotas must not be
//     marked dead by the nodes it throttles.
//   - **Relative, never absolute.** A failure counts only when some *other*
//     node answered us recently. One node silent while others answer is
//     evidence about that node; everything silent is evidence about us, and
//     marks nobody. (InboundHealthy() was rejected as the gate: it covers the
//     inbound half only, so an outbound fault would still paint the community
//     dead.)
//   - **First-hand and local, forever.** Never gossiped, never hinted — the
//     same argument that keeps distrust marks advisory. A relayed "X is down"
//     is a defamation lever.
//
// The predicate that reads the mark lives in the database package
// (reachClause / ReachableAt): unavailable when the mark is newer than
// last_seen AND last_seen is already outside the tight window, so the mark may
// shorten the pull window and never the ping window.

// errMeshDial tags every failure that happened while *connecting*, so a caller
// holding only the finished request's error can still tell the connect class
// from everything that can go wrong afterwards.
//
// This is deliberately a marker rather than a taxonomy of OS and gVisor errors:
// the question "did we get as far as an answer" is one the dialer can answer
// exactly, and one that inspecting an error string can only guess at.
var errMeshDial = errors.New("mesh dial failed")

// dialMesh is DialContext with that tag. Both HTTP clients dial through it —
// the control client directly, the blob client through dialHolder's Connect
// deadline — so any request either of them makes carries the distinction.
func (n *Node) dialMesh(ctx context.Context, network, address string) (net.Conn, error) {
	c, err := n.DialContext(ctx, network, address)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errMeshDial, err)
	}
	return c, nil
}

// connectFailure reports whether err is a connect-class failure: we never got
// as far as an answer, and the node is the likely reason.
//
// A cancelled dial is excluded, because that one is ours: a hedge losing its
// race, a transfer abandoned, the node shutting down. Blaming the holder for a
// connection we tore down ourselves is the same mistake as blaming the losing
// half of a hedge (scheduler.go, "fail"). A dial *timeout* is not excluded — it
// is exactly the observation this file exists to record.
func connectFailure(err error) bool {
	return err != nil && errors.Is(err, errMeshDial) && !errors.Is(err, context.Canceled)
}

// noteContact records that a node answered us. It feeds the self-guard only —
// the durable "this node is alive" write is observePeerAlive's or the ping
// path's, which are throttled or row-scoped in ways this must not be: the guard
// asks whether ANY node answered recently, so it has to see every answer.
func (n *Node) noteContact(key string) {
	if key == "" {
		return
	}
	n.contactMu.Lock()
	n.lastReplyAt, n.lastReplyKey = time.Now(), key
	n.contactMu.Unlock()
}

// guardWindow is how recently another node must have answered for a failure to
// count as evidence about the node that failed. Three refresh rounds — the same
// 3× anti-flap margin the freshness windows carry over the cadence that feeds
// them, and derived from that cadence rather than configured, because it is a
// question about the mesh and not about what an operator wants displayed.
func (n *Node) guardWindow() time.Duration { return 3 * n.intervals.Refresh }

// guardPasses implements the relative self-protection rule: mark only when
// somebody ELSE answered us inside the guard window.
//
// Holding one contact rather than a set is deliberate. It errs toward marking
// less: a node that is both our most recent success and our newest failure
// (a flapping link) is never marked, which is the safe direction — the window
// remains the hysteresis, and the next successful contact retires a mark
// anyway.
func (n *Node) guardPasses(key string) bool {
	n.contactMu.Lock()
	defer n.contactMu.Unlock()
	if n.lastReplyKey == "" || n.lastReplyKey == key {
		return false
	}
	return time.Since(n.lastReplyAt) < n.guardWindow()
}

// observeUnreachable records a first-hand connect failure against one node.
// Silent when the guard refuses it, which is most of the time on a healthy
// network and all of the time during our own outage.
func (n *Node) observeUnreachable(key string) {
	if n.store == nil || key == "" {
		return
	}
	// Ourselves: nothing to observe, and nothing that could be hidden by it.
	// Read off the mesh directly, since a node assembled without a transport is
	// exactly the shape the narrow tests use.
	if n.mesh != nil && key == n.mesh.PublicKeyHex() {
		return
	}
	if !n.guardPasses(key) {
		return
	}
	if err := n.store.MarkNodeUnreachable(n.transferCtx, key, time.Now().Unix()); err != nil {
		n.logger.Printf("federation: mark %s unreachable: %v", shortKey(key), err)
	}
}

// observeReply is the transfer path's funnel: what one request against a holder
// said about that holder's reachability.
//
// An answer of any status is liveness — this is where the 429 fix lands, since
// before it only a *verified chunk* advanced last_seen and a member refusing
// under its own quota looked exactly like a dead one. A connect-class failure is
// the down-mark. Everything in between (a stall, a corrupt chunk, a 416) says
// nothing here and is the scheduler's to judge.
func (n *Node) observeReply(p *BlobProvider, err error) {
	if p == nil {
		return
	}
	if err == nil {
		n.noteContact(p.PublicKey)
		n.observePeerAlive(p)
		return
	}
	if connectFailure(err) {
		n.observeUnreachable(p.PublicKey)
	}
}

// observeControl is the same funnel for the control paths, which know a key but
// hold no BlobProvider: the catalog pull, the friendship ping, the discovery
// ping and the floor ping. The durable positive write is the caller's — those
// paths already touch last_seen with the name the reply carried, and doing it
// twice would only add a write.
//
// A friend's failure is recorded like anyone's and is inert by construction: the
// predicate refuses to let a mark shorten the ping window, and a friend is
// always on it. Special-casing the call site would be one more rule to keep in
// agreement with the predicate.
func (n *Node) observeControl(key string, err error) {
	if err == nil {
		n.noteContact(key)
		return
	}
	if connectFailure(err) {
		n.observeUnreachable(key)
	}
}
