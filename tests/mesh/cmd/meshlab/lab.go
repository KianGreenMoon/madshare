//go:build tests && !nofederation

package main

// The lab itself: the two graphs, the netfault links between nodes, and the
// control API that lets a second shell change conditions while the first one
// holds the lab up.
//
// # The two graphs
//
// A topology preset describes the UNDERLAY PEERING GRAPH only — who dials whom
// over tcp:// or quic://, each peering through its own netfault link named
// "from-to". Who is FRIENDS with whom is a separate graph applied afterwards
// through the admin API (-friends).
//
// They are separate because madshare federation is friends-only and direct:
// catalog, manifest, blob and holdings each refuse a peer that is not a friend
// (federation/catalog.go, transfer.go, swarm.go). Nothing is relayed and nothing
// is discovered transitively. A lab that friended every node it started would
// therefore test one point in the space and hide the two that matter:
//
//   - friends across hops — friended but not underlay-adjacent. Must work
//     exactly as if adjacent; yggdrasil does the routing, we do not.
//   - adjacency is not access — underlay-peered but not friends. Must see
//     nothing: 403 on catalog and holdings, 404 on blobs and manifests.
//
// It is also what kept this usable as federation grew: F5 arrived as a different
// friendship graph plus the scope knobs (scope.go) over the same topology, not a
// meshlab rewrite. It did add one actor this file cannot express — an outsider
// that fetches rather than serves — because a madshare node only ever fetches
// from friends. That is probe.go.

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"daemonlord.ygg/madshare/tests/mesh/netfault"
)

// repoRoot is the madshare checkout the nodes run from; set in main.
var repoRoot string

func newJar() (*cookiejar.Jar, error) { return cookiejar.New(nil) }

// ── Links ────────────────────────────────────────────────────────────────────

// link is one underlay peering, faulted. Exactly one of stream/datagram is set,
// matching the peering's transport.
type link struct {
	name     string // "b-a": b dials a
	from, to string
	stream   *netfault.Proxy
	datagram *netfault.UDPProxy
}

func (l *link) addr() string {
	if l.stream != nil {
		return l.stream.Addr()
	}
	return l.datagram.Addr()
}

func (l *link) close() {
	if l.stream != nil {
		l.stream.Close()
		return
	}
	l.datagram.Close()
}

func (l *link) touches(node string) bool { return l.from == node || l.to == node }

// setFaultJSON applies a fault from the wire form, refusing knobs this
// transport cannot honour (the decoder upstream disallows unknown fields).
func (l *link) setFaultJSON(raw json.RawMessage) (string, error) {
	if l.stream != nil {
		var in netfault.FaultJSON
		if err := strictUnmarshal(raw, &in); err != nil {
			return "", err
		}
		f, err := in.Fault()
		if err != nil {
			return "", err
		}
		l.stream.Set(f)
		return summarize(f.ToJSON()), nil
	}
	var in netfault.DatagramFaultJSON
	if err := strictUnmarshal(raw, &in); err != nil {
		return "", err
	}
	f, err := in.Fault()
	if err != nil {
		return "", err
	}
	l.datagram.Set(f)
	return summarize(f.ToJSON()), nil
}

// partition flips only the Partition bit, leaving the rest of the fault alone —
// so "cut this node off, then heal it" does not silently discard a latency or
// bandwidth condition the session set earlier.
func (l *link) partition(on bool) {
	if l.stream != nil {
		f := l.stream.Fault()
		f.Partition = on
		l.stream.Set(f)
		return
	}
	f := l.datagram.Fault()
	f.Partition = on
	l.datagram.Set(f)
}

func (l *link) describe() map[string]any {
	m := map[string]any{"name": l.name, "from": l.from, "to": l.to}
	if l.stream != nil {
		m["transport"], m["fault"], m["stats"] = "tcp", l.stream.Fault().ToJSON(), l.stream.Stats()
	} else {
		m["transport"], m["fault"], m["stats"] = "quic", l.datagram.Fault().ToJSON(), l.datagram.Stats()
	}
	return m
}

// ── Lab ──────────────────────────────────────────────────────────────────────

type lab struct {
	root   string
	logger *log.Logger

	names []string
	nodes map[string]*node

	linkOrder []string
	links     map[string]*link

	friendPairs [][2]string

	mu    sync.Mutex
	flaps map[string]chan struct{} // node -> cancel for a running flap
	// probeNode is the outsider used by `check` — nobody's friend, started on
	// first use and kept for the lab's life (probe.go).
	probeNode *probe
}

// build lays out the lab: reserve ports, open a faulted link per peering, and
// write each node's config. Nothing is started yet.
func newLab(root, bin, scheme string, top topology, logger *log.Logger) (*lab, error) {
	l := &lab{
		root: root, logger: logger,
		nodes: map[string]*node{}, links: map[string]*link{},
		flaps: map[string]chan struct{}{},
		names: top.nodes, friendPairs: top.friends,
	}
	client := &http.Client{Timeout: 60 * time.Second}

	for _, name := range top.nodes {
		httpAddr, err := reservePort()
		if err != nil {
			return nil, err
		}
		var underlay string
		if scheme == "quic" {
			underlay, err = reserveUDPPort()
		} else {
			underlay, err = reservePort()
		}
		if err != nil {
			return nil, err
		}
		l.nodes[name] = &node{
			name: name, dir: filepath.Join(root, name), bin: bin,
			httpAdr: httpAddr, underlay: underlay, scheme: scheme,
			client: &http.Client{Timeout: client.Timeout},
		}
	}

	// One proxy per peering, named "from-to": from dials to. The fetcher-dials
	// convention of the chaos suite does not apply here — a lab node is both, so
	// the name records the direction and Up/Down follow from it (Up is from→to).
	for _, e := range top.edges {
		target := l.nodes[e.to].underlay
		var (
			lk  = &link{name: e.from + "-" + e.to, from: e.from, to: e.to}
			err error
		)
		if scheme == "quic" {
			lk.datagram, err = netfault.NewUDP(target, netfault.DatagramFault{})
		} else {
			lk.stream, err = netfault.New(target, netfault.Fault{})
		}
		if err != nil {
			l.closeLinks()
			return nil, fmt.Errorf("link %s: %w", lk.name, err)
		}
		l.linkOrder = append(l.linkOrder, lk.name)
		l.links[lk.name] = lk
		from := l.nodes[e.from]
		from.peers = append(from.peers, scheme+"://"+lk.addr())
	}
	return l, nil
}

func (l *lab) closeLinks() {
	for _, name := range l.linkOrder {
		l.links[name].close()
	}
}

// start launches every node, waits for readiness, bootstraps admin access,
// optionally seeds, and finally applies the friendship graph.
//
// Seeding before friending is not cosmetic ordering — it is what makes the lab
// usable. A friend's catalog is pulled on the refresh sweep only when it is
// older than Intervals.CatalogSync, fifteen minutes in production, and the
// timestamp lives in the database, so a restart does not reset it. Friend first
// and you sync an empty catalog, then wait a quarter of an hour to see anything.
// Friend last and the nudge that fires on a new friendship
// (federation/friendship.go: "start the first catalog sync right away") pulls a
// library that is already there.
//
// `meshlab seed` after the fact still works; its results just take until the
// next sync to appear on the friends, which the command says out loud.
func (l *lab) start(seedDir string, perNode int) error {
	for _, name := range l.names {
		n := l.nodes[name]
		if err := n.start(); err != nil {
			return err
		}
		l.logger.Printf("node %s: http://%s  (data %s)", name, n.httpAdr, n.dir)
	}
	for _, name := range l.names {
		if err := l.nodes[name].waitReady(90 * time.Second); err != nil {
			return err
		}
	}
	for _, name := range l.names {
		if err := l.nodes[name].bootstrap(); err != nil {
			return err
		}
		l.logger.Printf("node %s: ready, key %s…", name, short(l.nodes[name].publicKey()))
	}
	if seedDir != "" {
		report, err := l.seed(seedDir, perNode)
		if err != nil {
			return fmt.Errorf("seeding: %w", err)
		}
		l.logger.Printf("seeded %d files from %s (%d per node)", report.Total, report.Dir, report.PerNode)
		for name, e := range report.Errors {
			l.logger.Printf("seeding %s: %s", name, e)
		}
	}
	return l.applyFriends()
}

// applyFriends walks the friendship graph: each pair exchanges node cards and
// the receiving side accepts. Importing a card for a node that already asked to
// pair completes the friendship, so the second import usually finishes it and
// the explicit accept is the fallback for the ordering where it does not.
func (l *lab) applyFriends() error {
	for _, pair := range l.friendPairs {
		a, b := l.nodes[pair[0]], l.nodes[pair[1]]
		if a == nil || b == nil {
			return fmt.Errorf("friend pair %s-%s names a node that is not in this lab", pair[0], pair[1])
		}
		if err := a.postJSON("/api/admin/federation/peers", map[string]any{"card": b.nodeCard()}, nil); err != nil {
			return fmt.Errorf("import %s's card on %s: %w", b.name, a.name, err)
		}
		if err := b.postJSON("/api/admin/federation/peers", map[string]any{"card": a.nodeCard()}, nil); err != nil {
			return fmt.Errorf("import %s's card on %s: %w", a.name, b.name, err)
		}
		if err := l.acceptEachOther(a, b); err != nil {
			return err
		}
		l.logger.Printf("friends: %s <-> %s", a.name, b.name)
	}
	if len(l.friendPairs) == 0 {
		l.logger.Print("friends: none — every node is underlay-peered but isolated at the madshare layer")
	}
	return nil
}

// acceptEachOther approves any pending_incoming request on both sides, polling
// because the pairing handshake crosses the mesh and the mesh has just come up.
func (l *lab) acceptEachOther(a, b *node) error {
	deadline := time.Now().Add(90 * time.Second)
	for {
		aOK, err := l.acceptFrom(a, b.publicKey())
		if err != nil {
			return err
		}
		bOK, err := l.acceptFrom(b, a.publicKey())
		if err != nil {
			return err
		}
		if aOK && bOK {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("friendship %s<->%s did not converge in 90s "+
				"(a=%v b=%v) — is the link between them up?", a.name, b.name, aOK, bOK)
		}
		time.Sleep(time.Second)
	}
}

// acceptFrom accepts a pending request from key on n, reporting whether n now
// considers that peer a friend.
func (l *lab) acceptFrom(n *node, key string) (bool, error) {
	peers, err := n.peerList()
	if err != nil {
		return false, err
	}
	for _, p := range peers {
		if p.PublicKey != key {
			continue
		}
		switch p.State {
		case "friend":
			return true, nil
		case "pending_incoming":
			if err := n.postJSON(fmt.Sprintf("/api/admin/federation/peers/%d/accept", p.ID), nil, nil); err != nil {
				return false, fmt.Errorf("accept on %s: %w", n.name, err)
			}
			return true, nil
		}
		return false, nil
	}
	return false, nil
}

type peerRow struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	PublicKey string `json:"public_key"`
	State     string `json:"state"`
	LastSeen  int64  `json:"last_seen"`
}

func (n *node) peerList() ([]peerRow, error) {
	var out struct {
		Peers []peerRow `json:"peers"`
	}
	if err := n.getJSON("/api/admin/federation/peers", &out); err != nil {
		return nil, err
	}
	return out.Peers, nil
}

// stop tears the lab down: flaps first, then nodes, then the links.
func (l *lab) stop() {
	l.mu.Lock()
	for name, cancel := range l.flaps {
		close(cancel)
		delete(l.flaps, name)
	}
	p := l.probeNode
	l.probeNode = nil
	l.mu.Unlock()
	p.stop()
	for _, name := range l.names {
		l.nodes[name].stop()
	}
	l.closeLinks()
}

// ── Node operations ──────────────────────────────────────────────────────────

// restart brings a node back with its data dir intact — the same identity, not
// a new one. federation.key lives in the data dir, and a node that lost it is a
// stranger to every friend it had.
func (l *lab) restart(name string) error {
	n, ok := l.nodes[name]
	if !ok {
		return fmt.Errorf("no node %q", name)
	}
	n.stop()
	if err := n.start(); err != nil {
		return err
	}
	if err := n.waitReady(90 * time.Second); err != nil {
		return err
	}
	return n.bootstrap()
}

// partitionNode cuts every link touching a node, which is the closest thing to
// "unplug this machine" the lab has. Distinct from kill: the process keeps
// running, so its own view of the outage is observable too.
func (l *lab) partitionNode(name string, on bool) error {
	if _, ok := l.nodes[name]; !ok {
		return fmt.Errorf("no node %q", name)
	}
	n := 0
	for _, ln := range l.linkOrder {
		if lk := l.links[ln]; lk.touches(name) {
			lk.partition(on)
			n++
		}
	}
	if n == 0 {
		return fmt.Errorf("node %q has no links to partition", name)
	}
	return nil
}

// flap partitions and heals a node's links on a period until cancelled. A
// second flap on the same node replaces the first.
func (l *lab) flap(name string, down, up time.Duration) error {
	if _, ok := l.nodes[name]; !ok {
		return fmt.Errorf("no node %q", name)
	}
	cancel := make(chan struct{})
	l.mu.Lock()
	if prev, ok := l.flaps[name]; ok {
		close(prev)
	}
	l.flaps[name] = cancel
	l.mu.Unlock()

	go func() {
		for {
			l.partitionNode(name, true)
			select {
			case <-time.After(down):
			case <-cancel:
				l.partitionNode(name, false)
				return
			}
			l.partitionNode(name, false)
			select {
			case <-time.After(up):
			case <-cancel:
				return
			}
		}
	}()
	return nil
}

func (l *lab) stopFlap(name string) {
	l.mu.Lock()
	cancel, ok := l.flaps[name]
	delete(l.flaps, name)
	l.mu.Unlock()
	if ok {
		close(cancel)
	}
}

// ── Status ───────────────────────────────────────────────────────────────────

type nodeStatus struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	Running bool   `json:"running"`
	Key     string `json:"key,omitempty"`
	Tracks  int    `json:"tracks"`
	// Madnetwork is what this node sees on /madnetwork — its own published set
	// plus every friend's, after the freshness filter and the sharing scope. It
	// is the number a lab exists to watch: it is what the browse would show, so
	// it moves when a friend goes stale, when one returns, and when this node
	// takes a recording off the network.
	Madnetwork     int         `json:"madnetwork"`
	InboundHealthy bool        `json:"inbound_healthy"`
	Peers          []peerState `json:"peers,omitempty"`
	Error          string      `json:"error,omitempty"`
}

type peerState struct {
	Name        string `json:"name"`
	State       string `json:"state"`
	LastSeenAge string `json:"last_seen_age,omitempty"`
	Reachable   bool   `json:"reachable"`
}

// status asks every running node what it currently believes, which is the only
// honest source: the lab knows what it configured, the node knows what it sees.
func (l *lab) status() map[string]any {
	nodes := make([]nodeStatus, 0, len(l.names))
	for _, name := range l.names {
		n := l.nodes[name]
		st := nodeStatus{Name: name, URL: "http://" + n.httpAdr, Running: n.running(), Key: short(n.publicKey())}
		if st.Running {
			if count, err := n.libraryCount(); err == nil {
				st.Tracks = count
			}
			if seen, healthy, err := n.madnetworkView(); err == nil {
				st.Madnetwork, st.InboundHealthy = seen, healthy
			}
			peers, err := n.peerList()
			if err != nil {
				st.Error = err.Error()
			}
			for _, p := range peers {
				ps := peerState{Name: p.Name, State: p.State}
				if p.LastSeen > 0 {
					age := time.Since(time.Unix(p.LastSeen, 0)).Truncate(time.Second)
					ps.LastSeenAge = age.String()
					// The same predicate the browse uses at request time: a friend
					// outside the freshness window has its exclusive tracks hidden.
					ps.Reachable = age < reachableWindowSec*time.Second
				}
				st.Peers = append(st.Peers, ps)
			}
		}
		nodes = append(nodes, st)
	}
	links := make([]any, 0, len(l.linkOrder))
	for _, name := range l.linkOrder {
		links = append(links, l.links[name].describe())
	}
	friends := make([]string, 0, len(l.friendPairs))
	for _, p := range l.friendPairs {
		friends = append(friends, p[0]+"-"+p[1])
	}
	l.mu.Lock()
	flapping := make([]string, 0, len(l.flaps))
	for name := range l.flaps {
		flapping = append(flapping, name)
	}
	l.mu.Unlock()
	sort.Strings(flapping)

	return map[string]any{
		"root": l.root, "nodes": nodes, "links": links,
		"friends": friends, "flapping": flapping,
		"reachable_window_sec": reachableWindowSec,
		"scope":                l.scopeAll(),
	}
}

// ── Control API ──────────────────────────────────────────────────────────────

func (l *lab) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, l.status())
	})

	mux.HandleFunc("/links/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/links/")
		lk, ok := l.links[name]
		if !ok {
			writeErr(w, http.StatusNotFound, "no link %q (have: %s)", name, strings.Join(l.linkOrder, ", "))
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, lk.describe())
		case http.MethodPut:
			raw, err := readBody(r)
			if err != nil {
				writeErr(w, http.StatusBadRequest, "%v", err)
				return
			}
			applied, err := lk.setFaultJSON(raw)
			if err != nil {
				writeErr(w, http.StatusBadRequest, "%v", err)
				return
			}
			l.logger.Printf("link %s <- %s", name, applied)
			writeJSON(w, http.StatusOK, lk.describe())
		default:
			writeErr(w, http.StatusMethodNotAllowed, "GET or PUT")
		}
	})

	mux.HandleFunc("/nodes/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/nodes/")
		name, action, _ := strings.Cut(rest, "/")
		n, ok := l.nodes[name]
		if !ok {
			writeErr(w, http.StatusNotFound, "no node %q (have: %s)", name, strings.Join(l.names, ", "))
			return
		}
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "POST")
			return
		}
		var err error
		switch action {
		case "kill":
			n.stop()
			l.logger.Printf("node %s: killed (data dir and identity kept)", name)
		case "restart":
			l.logger.Printf("node %s: restarting", name)
			err = l.restart(name)
		case "partition":
			err = l.partitionNode(name, true)
			l.logger.Printf("node %s: partitioned from every peer", name)
		case "heal":
			l.stopFlap(name)
			err = l.partitionNode(name, false)
			l.logger.Printf("node %s: healed", name)
		case "flap":
			var body struct{ Down, Up string }
			raw, rerr := readBody(r)
			if rerr == nil && len(raw) > 0 {
				json.Unmarshal(raw, &body)
			}
			down, uerr := netfault.ParseDuration(orDefault(body.Down, "10s"), "down")
			if uerr != nil {
				writeErr(w, http.StatusBadRequest, "%v", uerr)
				return
			}
			up, uerr := netfault.ParseDuration(orDefault(body.Up, "20s"), "up")
			if uerr != nil {
				writeErr(w, http.StatusBadRequest, "%v", uerr)
				return
			}
			err = l.flap(name, down, up)
			l.logger.Printf("node %s: flapping %v down / %v up", name, down, up)
		default:
			writeErr(w, http.StatusNotFound, "unknown action %q (kill, restart, partition, heal, flap)", action)
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "%v", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "node": name, "action": action})
	})

	mux.HandleFunc("/seed", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "POST")
			return
		}
		var body struct {
			Dir     string `json:"dir"`
			PerNode int    `json:"per_node"`
		}
		if raw, err := readBody(r); err == nil && len(raw) > 0 {
			json.Unmarshal(raw, &body)
		}
		report, err := l.seed(body.Dir, body.PerNode)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "%v", err)
			return
		}
		writeJSON(w, http.StatusOK, report)
	})

	// Sharing scope (F5): set a node's default depth, or the depth / guest flag
	// of its recordings.
	mux.HandleFunc("/scope", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "POST")
			return
		}
		var req scopeRequest
		raw, err := readBody(r)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "%v", err)
			return
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid request: %v", err)
			return
		}
		report, err := l.applyScope(req)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "%v", err)
			return
		}
		writeJSON(w, http.StatusOK, report)
	})

	// The scope assertion pass. Runs here rather than in the client because the
	// outsider probe is a mesh node the lab owns, not something a CLI invocation
	// can spin up per command.
	mux.HandleFunc("/check", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "POST")
			return
		}
		report, err := l.check()
		if err != nil {
			writeErr(w, http.StatusBadRequest, "%v", err)
			return
		}
		writeJSON(w, http.StatusOK, report)
	})

	return mux
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func strictUnmarshal(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid fault JSON: %w", err)
	}
	return nil
}

func readBody(r *http.Request) (json.RawMessage, error) {
	r.Body = http.MaxBytesReader(nil, r.Body, 64<<10)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(strings.TrimSpace(string(raw))), nil
}

func summarize(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "?"
	}
	if s := string(b); s != `{"up":{},"down":{}}` {
		return s
	}
	return "transparent"
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, format string, args ...any) {
	writeJSON(w, code, map[string]string{"error": fmt.Sprintf(format, args...)})
}

func short(key string) string {
	if len(key) > 12 {
		return key[:12]
	}
	return key
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "meshlab: "+format+"\n", args...)
	os.Exit(1)
}
