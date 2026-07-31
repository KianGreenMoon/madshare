//go:build tests && !nofederation

package main

// `meshlab reach` — what does friendship DISTANCE actually cost?
//
// The question this answers came up while designing F7 (2026-07-31): if a track
// I want lives on a node six friendships away, is reaching it slow? The
// intuition that it must be — "ask my friend to ask his friend to ask…" — is a
// reasonable one, and it is wrong, because the friendship graph is not the
// transport. A yggdrasil address is DERIVED from a node key, so knowing a key is
// the same thing as being able to dial its owner: every fetch in the swarm does
// `AddrForKeyHex(peer.PublicKey)` and connects, at distance 1 or 20 alike.
//
// Being right in an argument is not evidence, so this command measures it. Two
// arms, deliberately separated, because they fail for unrelated reasons:
//
//   - ROUTING (works today) — the mesh RTT to every node in the lab, by
//     friendship distance. It uses `/madnetwork/v0/ping`, which is open to
//     strangers (meshAuth refuses only BLOCKED peers), so it measures the
//     network and nothing else. If distance were expensive, it would show up
//     here as a slope. This arm needs no F7 and is the direct answer to the
//     question.
//
//   - REACH (still fails past distance 1, by design) — an actual content fetch
//     from the vantage node for a track only the distant node publishes. Since
//     F7 items 1–3 the distant node *would* serve us: we are a member of its
//     community and it answers members. What is missing is knowing the hash
//     exists — `MadnetworkBlobProviders` and the catalog sweep both still join
//     `state = 'friend'`, so nothing past our own ring is ever a provider. The
//     failure IS F7 item 5 (discovery beyond the friend ring) stated as a
//     measurement; when that lands the same run should turn green at every
//     distance, and — the hypothesis worth recording — take roughly the same
//     time at each.
//
// Run it on a friendship chain, which is the shape where the two distances
// coincide and a slope would therefore be visible at its worst:
//
//	meshlab up -nodes 7 -topology chain -friends adjacent -seed ./audio
//	meshlab reach
//
// `-friends adjacent` is what makes this a chain of FRIENDSHIPS rather than a
// clique that happens to be wired in a line; with the default `-friends all`
// every node is at distance 1 and there is nothing to measure.

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"
	"time"
)

// reachHop is one node's result, at its friendship distance from the vantage.
type reachHop struct {
	Node     string `json:"node"`
	Distance int    `json:"distance"` // friendship hops from the vantage; -1 = unreachable in the friend graph
	// Routing arm. Cold is the FIRST contact — yggdrasil session setup to a node
	// this one has never spoken to — and it dwarfs everything else here, which is
	// the finding rather than an artifact. Median/Best are warm.
	//
	// Cold is only cold ONCE per lab: the probe is started on first use and kept
	// for the lab's life (check.go, ensureProbe), so a second `reach` run reports
	// a warm number in this column. Read it on the first run, or restart the lab.
	PingCold   string `json:"ping_cold,omitempty"`
	PingMedian string `json:"ping_median,omitempty"`
	PingBest   string `json:"ping_best,omitempty"`
	PingFailed string `json:"ping_failed,omitempty"`
	// Reach arm.
	Subject    string `json:"subject,omitempty"` // the track title we tried to fetch
	Hash       string `json:"hash,omitempty"`
	FirstChunk string `json:"first_chunk,omitempty"` // time to the first 64 KiB — the player's first byte
	Whole      string `json:"whole,omitempty"`       // time to the complete verified file
	Bytes      int    `json:"bytes,omitempty"`
	FetchOK    bool   `json:"fetch_ok"`
	FetchNote  string `json:"fetch_note,omitempty"`
}

type reachReport struct {
	Vantage string     `json:"vantage"`
	Hops    []reachHop `json:"hops"`
	Elapsed string     `json:"elapsed"`
	// Verdict is the one-line reading of the routing arm, which is the arm that
	// answers the design question.
	Verdict string `json:"verdict"`
}

func cmdReach(args []string) {
	fs := flag.NewFlagSet("reach", flag.ExitOnError)
	runs := fs.Int("runs", 5, "ping samples per node (the median is reported; one sample is noise)")
	timeout := fs.Duration("timeout", 3*time.Minute, "per-fetch timeout")
	noFetch := fs.Bool("no-fetch", false, "routing arm only — skip the content fetches")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	control := fs.String("control", defaultControl, "lab control address")
	fs.Parse(args)

	body, err := json.Marshal(map[string]any{
		"runs":     *runs,
		"timeout":  timeout.String(),
		"no_fetch": *noFetch,
	})
	if err != nil {
		fatalf("reach: %v", err)
	}
	raw, err := call(*control, http.MethodPost, "/reach", body)
	if err != nil {
		fatalf("%v", err)
	}
	if *asJSON {
		os.Stdout.Write(raw)
		return
	}
	var rep reachReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		fatalf("decoding report: %v", err)
	}
	printReach(&rep)
}

func printReach(rep *reachReport) {
	fmt.Printf("reach from %s (%s)\n\n", rep.Vantage, rep.Elapsed)
	fmt.Printf("  %-6s %-5s %-11s %-11s %-11s %-11s %s\n",
		"node", "dist", "ping warm", "ping cold", "first 64K", "whole file", "note")
	for _, h := range rep.Hops {
		dist := fmt.Sprint(h.Distance)
		if h.Distance < 0 {
			dist = "—"
		}
		ping := h.PingMedian
		if ping == "" {
			ping = "—"
		}
		if h.PingFailed != "" {
			ping = "FAILED"
		}
		note := h.FetchNote
		if h.PingFailed != "" {
			note = h.PingFailed
		}
		fmt.Printf("  %-6s %-5s %-11s %-11s %-11s %-11s %s\n",
			h.Node, dist, ping, dash(h.PingCold), dash(h.FirstChunk), dash(h.Whole), note)
	}
	fmt.Printf("\n%s\n", rep.Verdict)
}

// ── The measurement, server side ─────────────────────────────────────────────

// reach measures both arms. The vantage is the lab's FIRST node, not a flag:
// the outsider probe peers into that node's underlay listener, so its view of
// the mesh is the closest thing the lab has to "what node[0] sees", and a knob
// that silently kept measuring from a while claiming to measure from c would be
// worse than no knob.
func (l *lab) reach(runs int, timeout time.Duration, noFetch bool) (*reachReport, error) {
	started := time.Now()
	if len(l.names) < 2 {
		return nil, fmt.Errorf("reach needs at least 2 nodes (this lab has %d)", len(l.names))
	}
	vantage := l.nodes[l.names[0]]
	if !vantage.running() {
		return nil, fmt.Errorf("vantage node %s is not running", vantage.name)
	}
	rep := &reachReport{Vantage: vantage.name}

	dist := l.friendDistances(vantage.name)

	p, err := l.ensureProbe()
	if err != nil {
		return nil, err
	}

	for _, name := range l.names[1:] {
		n := l.nodes[name]
		hop := reachHop{Node: name, Distance: -1}
		if d, ok := dist[name]; ok {
			hop.Distance = d
		}
		if !n.running() {
			hop.FetchNote = "node is stopped"
			rep.Hops = append(rep.Hops, hop)
			continue
		}

		// ── Routing arm ──────────────────────────────────────────────────────
		// Ping is not friends-gated, so this reaches every node regardless of
		// the friendship graph — which is the whole point: it separates "can I
		// route there" from "may I have it".
		//
		// The wait is TIMED, because on a first run it is not waiting for the
		// lab — it is paying yggdrasil's session setup to a node this one has
		// never spoken to, and that cost is far larger than anything else this
		// command measures (observed >60 s in the F4 work, and again here). It is
		// also per-PEER, not per-hop, so it is not a distance cost — but it is
		// the number a person feels when they click play on a stranger's track.
		coldStart := time.Now()
		if err := p.wait(n, 150*time.Second); err != nil {
			hop.PingFailed = err.Error()
			rep.Hops = append(rep.Hops, hop)
			continue
		}
		hop.PingCold = round(time.Since(coldStart))
		samples := make([]time.Duration, 0, runs)
		for range runs {
			t0 := time.Now()
			code, _, err := p.get(n, "/madnetwork/v0/ping")
			if err != nil || code != http.StatusOK {
				continue
			}
			samples = append(samples, time.Since(t0))
		}
		if len(samples) == 0 {
			hop.PingFailed = "no ping answered"
			rep.Hops = append(rep.Hops, hop)
			continue
		}
		sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
		hop.PingMedian = round(samples[len(samples)/2])
		hop.PingBest = round(samples[0])

		// ── Reach arm ────────────────────────────────────────────────────────
		if !noFetch {
			l.measureFetch(&hop, vantage, n, timeout)
		}
		rep.Hops = append(rep.Hops, hop)
	}

	rep.Elapsed = round(time.Since(started))
	rep.Verdict = reachVerdict(rep)
	return rep, nil
}

// measureFetch times what a person actually waits for: the first 64 KiB (the
// player's first byte, served from the first chunk the swarm verifies) and then
// the complete file, hash-checked. Both go through
// /api/madnetwork/stream/{hash} — the same request the web player makes — so a
// number here is a number a user would have felt.
func (l *lab) measureFetch(hop *reachHop, from, holder *node, timeout time.Duration) {
	apps, err := holder.appearances()
	if err != nil {
		hop.FetchNote = "appearances: " + err.Error()
		return
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].TagsetID < apps[j].TagsetID })
	var subject *appearance
	for i := range apps {
		if apps[i].Hash == "" {
			continue
		}
		// A track the vantage already holds locally short-circuits before any
		// peer is asked, which would time the local disk and call it the mesh.
		if held, err := from.holdsLocally(apps[i].Hash); err == nil && held {
			continue
		}
		subject = &apps[i]
		break
	}
	if subject == nil {
		hop.FetchNote = "nothing published here that " + from.name + " does not already hold"
		return
	}
	hop.Subject, hop.Hash = subject.Title, subject.Hash

	// Start from cold every time. A cached blob answers from disk, which would
	// make distance look free for the most flattering possible reason.
	if err := from.dropCachedBlob(subject.Hash); err != nil {
		hop.FetchNote = "could not clear cache: " + err.Error()
		return
	}

	t0 := time.Now()
	code, body, err := from.streamRange(subject.Hash, 0, 65535, timeout)
	switch {
	case err != nil:
		hop.FetchNote = "first chunk: " + err.Error()
		return
	case code != http.StatusOK && code != http.StatusPartialContent:
		// The expected outcome above distance 1 until F7 item 5, and the reason
		// this is worth running: the gap is a measurement, not a claim. Note where
		// it now lives — since F7 items 1–3, %s WOULD serve %s as a member of its
		// community; what is missing is that %s never learned %s holds the hash,
		// because MadnetworkBlobProviders and the catalog sweep still stop at
		// state='friend'. Discovery, not authorization.
		hop.FetchNote = fmt.Sprintf("stream = %d (no provider: %s never learned %s holds it — catalogs are pulled from friends only)",
			code, from.name, holder.name)
		return
	}
	hop.FirstChunk = round(time.Since(t0))
	_ = body

	t1 := time.Now()
	code, whole, err := from.streamBody(subject.Hash, timeout)
	switch {
	case err != nil:
		hop.FetchNote = "whole file: " + err.Error()
	case code != http.StatusOK:
		hop.FetchNote = fmt.Sprintf("whole file = %d", code)
	default:
		hop.Whole = round(time.Since(t1))
		hop.Bytes = len(whole)
		if got := sha256Hex(whole); got != subject.Hash {
			hop.FetchNote = fmt.Sprintf("CONTENT MISMATCH: got %s…, want %s…", short(got), short(subject.Hash))
			return
		}
		hop.FetchOK = true
	}
}

// reachVerdict reads the routing arm out loud, because a table of durations is
// exactly the kind of evidence people skim past. It compares the nearest and
// farthest node that answered a ping: if friendship distance had a cost, this is
// where a slope would appear.
func reachVerdict(rep *reachReport) string {
	var near, far *reachHop
	for i := range rep.Hops {
		h := &rep.Hops[i]
		if h.PingMedian == "" || h.Distance < 0 {
			continue
		}
		if near == nil || h.Distance < near.Distance {
			near = h
		}
		if far == nil || h.Distance > far.Distance {
			far = h
		}
	}
	if near == nil || far == nil || near == far {
		return "routing: not enough reachable nodes at different distances to compare — " +
			"run this on `-topology chain -friends adjacent` with 4 or more nodes."
	}
	cold := ""
	if far.PingCold != "" && near.PingCold != "" {
		cold = fmt.Sprintf(
			"\n\nCold contact is the cost that is actually there — first ping %s to %s, %s to %s. It is\n"+
				"yggdrasil session setup, paid per PEER and not per hop, so it does not track distance:\n"+
				"the FIRST node contacted absorbs the mesh join and is usually the slowest of them all,\n"+
				"whatever its distance. That is the number a person feels on a cold track, and the\n"+
				"argument for preferring holders we already have a warm session with.",
			near.PingCold, near.Node, far.PingCold, far.Node)
	}
	return fmt.Sprintf(
		"routing: %s is %d friendship hop(s) away and answers in %s; %s is %d hop(s) away and answers in %s.\n"+
			"A mesh address derives from a node key, so both were dialled DIRECTLY — friendship\n"+
			"distance is not network distance, and asking along the friend chain would add one\n"+
			"round trip per hop to a question one round trip already answers.%s",
		near.Node, near.Distance, near.PingMedian, far.Node, far.Distance, far.PingMedian, cold)
}

// friendDistances is BFS over the friendship graph the lab was built with —
// NOT the underlay. The two are kept separate everywhere in meshlab (topology.go),
// and this command exists to show that the separation is real.
func (l *lab) friendDistances(from string) map[string]int {
	adj := map[string][]string{}
	for _, p := range l.friendGraph() {
		adj[p[0]] = append(adj[p[0]], p[1])
		adj[p[1]] = append(adj[p[1]], p[0])
	}
	dist := map[string]int{from: 0}
	queue := []string{from}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range adj[cur] {
			if _, seen := dist[next]; !seen {
				dist[next] = dist[cur] + 1
				queue = append(queue, next)
			}
		}
	}
	return dist
}

// streamRange asks the relay for one byte range — the request a player makes to
// start playback, and the one the swarm answers with seek-priority rather than
// by fetching the whole file first.
func (n *node) streamRange(hash string, start, end int64, timeout time.Duration) (int, []byte, error) {
	return n.rawGetRange("/api/madnetwork/stream/"+hash, fmt.Sprintf("bytes=%d-%d", start, end), timeout)
}

// round keeps a duration readable without rounding the interesting cases to
// zero: a warm mesh ping on loopback is hundreds of MICROseconds, and printing
// that as "0s" would hide the very result this command exists to produce.
func round(d time.Duration) string {
	switch {
	case d >= time.Second:
		return d.Truncate(10 * time.Millisecond).String()
	case d >= time.Millisecond:
		return d.Truncate(100 * time.Microsecond).String()
	default:
		return d.Truncate(time.Microsecond).String()
	}
}

func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
