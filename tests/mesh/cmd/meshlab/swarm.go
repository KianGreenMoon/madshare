//go:build tests && !nofederation

package main

// `meshlab swarm` — the person-visible balance run (docs/plans/swarm-lab.md
// §The meshlab arm). The chaos suite asserts the swarm's balance claims in
// milliseconds with TransferStats; this command shows the same behaviour on
// REAL servers, with the per-counterparty byte ledger (/api/admin/swarm/peers)
// as the instrument, under whatever link state the lab currently has. The
// knobs stay the person's: cap a holder with `meshlab link`, kill one with
// `meshlab kill`, and run this again to watch the split move.
//
// Three phases, reported as two tables and a verdict:
//
//   SPREAD  — every middle node materializes the subject through its own
//             cache-through relay, then the vantage is told to pull those
//             nodes' holdings (pull-now beats the 15-minute production
//             cadence). The table answers "how does holder information
//             spread, and how long until the fetcher KNEW".
//   FETCH   — the vantage clears its cache and streams the subject, exactly
//             as the web player would. Per-counterparty download deltas
//             around the fetch are the balance table.
//   VERDICT — who carried what share, and what to turn next.
//
// Everything goes through the stream relay rather than POST /download on
// purpose: a download stages a review-bucket draft into the library on every
// node it touches, which pollutes the lab and makes a second run
// short-circuit on the local copy. The relay lands bytes in the download
// cache only — which is also what makes the middles advertise them via
// holdings, i.e. the very spread this command measures — and stays
// re-runnable through dropCachedBlob.

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sort"
	"time"
)

type swarmSpreadRow struct {
	Node string `json:"node"`
	Role string `json:"role"` // "library" (the origin) or "cache" (a materialized copy)
	// Fetch is how long this node took to hold the bytes; Known how long until
	// the vantage listed it as a holder. Empty = not applicable / already there.
	Fetch string `json:"fetch,omitempty"`
	Known string `json:"known,omitempty"`
	Note  string `json:"note,omitempty"`
}

type swarmBalanceRow struct {
	Node  string `json:"node"` // lab name when the key is one of ours, else the short key
	Key   string `json:"key"`
	Bytes int64  `json:"bytes"`
	Share string `json:"share"`
}

type swarmReport struct {
	Vantage    string           `json:"vantage"`
	Subject    string           `json:"subject"`
	Hash       string           `json:"hash"`
	Holders    int              `json:"holders"`
	Spread     []swarmSpreadRow `json:"spread"`
	FirstChunk string           `json:"first_chunk,omitempty"`
	Whole      string           `json:"whole,omitempty"`
	// Settled is how long AFTER the last byte the transfer stayed open —
	// a straggling dispatch to a slow holder finishing or timing out.
	Settled  string            `json:"settled,omitempty"`
	Bytes    int               `json:"bytes,omitempty"`
	Verified bool              `json:"verified"`
	Balance  []swarmBalanceRow `json:"balance"`
	Elapsed  string            `json:"elapsed"`
	Verdict  string            `json:"verdict"`
}

func cmdSwarm(args []string) {
	fs := flag.NewFlagSet("swarm", flag.ExitOnError)
	subject := fs.String("subject", "", "content hash to fetch (default: the origin's oldest track the vantage does not hold)")
	timeout := fs.Duration("timeout", 5*time.Minute, "per-fetch timeout")
	spreadTimeout := fs.Duration("spread-timeout", 3*time.Minute, "how long to wait for the vantage to learn each holder (the sweep runs on a 1-minute cadence)")
	noSpread := fs.Bool("no-spread", false, "skip the spread phase — fetch against whatever holders already exist")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	control := fs.String("control", defaultControl, "lab control address")
	fs.Parse(args)

	body, err := json.Marshal(map[string]any{
		"subject":        *subject,
		"timeout":        timeout.String(),
		"spread_timeout": spreadTimeout.String(),
		"no_spread":      *noSpread,
	})
	if err != nil {
		fatalf("swarm: %v", err)
	}
	raw, err := call(*control, http.MethodPost, "/swarm", body)
	if err != nil {
		fatalf("%v", err)
	}
	if *asJSON {
		os.Stdout.Write(raw)
		return
	}
	var rep swarmReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		fatalf("decoding report: %v", err)
	}
	printSwarm(&rep)
}

func printSwarm(rep *swarmReport) {
	fmt.Printf("swarm fetch of %q (%s…) from %s (%s)\n\n",
		rep.Subject, short(rep.Hash), rep.Vantage, rep.Elapsed)
	fmt.Printf("  spread — who holds it, and when the vantage knew:\n")
	fmt.Printf("  %-8s %-9s %-11s %-11s %s\n", "node", "role", "fetch", "known", "note")
	for _, s := range rep.Spread {
		fmt.Printf("  %-8s %-9s %-11s %-11s %s\n",
			s.Node, s.Role, dash(s.Fetch), dash(s.Known), s.Note)
	}
	settled := ""
	if rep.Settled != "" {
		settled = fmt.Sprintf(", transfer open %s after the last byte", rep.Settled)
	}
	fmt.Printf("\n  balance — the measured fetch (first 64K %s, whole %s, verified %v%s):\n",
		dash(rep.FirstChunk), dash(rep.Whole), rep.Verified, settled)
	fmt.Printf("  %-8s %-14s %10s   %s\n", "node", "key", "bytes", "share")
	for _, b := range rep.Balance {
		fmt.Printf("  %-8s %-14s %10d   %s\n", b.Node, short(b.Key)+"…", b.Bytes, b.Share)
	}
	fmt.Printf("\n%s\n", rep.Verdict)
}

// ── The measurement, server side ─────────────────────────────────────────────

func (l *lab) swarm(subject string, timeout, spreadTimeout time.Duration, noSpread bool) (*swarmReport, error) {
	started := time.Now()
	if len(l.names) < 2 {
		return nil, fmt.Errorf("swarm needs at least 2 nodes (this lab has %d)", len(l.names))
	}
	origin := l.nodes[l.names[0]]
	vantage := l.nodes[l.names[len(l.names)-1]]
	if !origin.running() || !vantage.running() {
		return nil, fmt.Errorf("origin (%s) and vantage (%s) must both be running", origin.name, vantage.name)
	}
	rep := &swarmReport{Vantage: vantage.name}

	// ── The subject ──────────────────────────────────────────────────────────
	apps, err := origin.appearances()
	if err != nil {
		return nil, fmt.Errorf("%s appearances: %v", origin.name, err)
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].TagsetID < apps[j].TagsetID })
	if subject != "" {
		rep.Hash = subject
		for i := range apps {
			if apps[i].Hash == subject {
				rep.Subject = apps[i].Title
				break
			}
		}
	} else {
		for i := range apps {
			if apps[i].Hash == "" {
				continue
			}
			// A subject the vantage already publishes is served from its own disk
			// before any peer is asked — that would time the filesystem and call
			// it a swarm.
			if held, err := vantage.holdsLocally(apps[i].Hash); err == nil && held {
				continue
			}
			rep.Subject, rep.Hash = apps[i].Title, apps[i].Hash
			break
		}
		if rep.Hash == "" {
			return nil, fmt.Errorf("nothing published on %s that %s does not already hold — pass -subject", origin.name, vantage.name)
		}
	}
	rep.Spread = append(rep.Spread, swarmSpreadRow{Node: origin.name, Role: "library"})

	// ── Spread ───────────────────────────────────────────────────────────────
	// Every running node between origin and vantage materializes the subject
	// into its cache. A node that already holds it streams from its own cache in
	// milliseconds, so the phase is idempotent and a re-run reports ~0s here.
	expect := map[string]string{origin.publicKey(): origin.name}
	if !noSpread {
		for _, name := range l.names[1 : len(l.names)-1] {
			mid := l.nodes[name]
			row := swarmSpreadRow{Node: name, Role: "cache"}
			if !mid.running() {
				row.Note = "node is stopped"
				rep.Spread = append(rep.Spread, row)
				continue
			}
			t0 := time.Now()
			code, body, err := mid.streamBody(rep.Hash, timeout)
			switch {
			case err != nil:
				row.Note = "fetch: " + err.Error()
			case code != http.StatusOK:
				row.Note = fmt.Sprintf("fetch = %d", code)
			case sha256Hex(body) != rep.Hash:
				row.Note = "CONTENT MISMATCH on the spread fetch"
			default:
				row.Fetch = round(time.Since(t0))
				expect[mid.publicKey()] = name
			}
			rep.Spread = append(rep.Spread, row)
		}
	}

	// The vantage learns holdings on the sweep's own cadence (1 minute in
	// production, and meshlab deliberately does not shrink it) — pull-now moves
	// each expected holder to the head of the next round instead of the
	// rotation's leisure. The wait that follows is the actual measurement:
	// "how long until the fetcher KNEW" is a fact about the spread machinery,
	// not about this command.
	for key := range expect {
		_ = vantage.postJSON("/api/admin/federation/discover", map[string]any{"public_key": key}, nil)
	}
	known := map[string]time.Duration{}
	waitStart := time.Now()
	deadline := time.Now().Add(spreadTimeout)
	for len(known) < len(expect) && time.Now().Before(deadline) {
		var resp struct {
			Holders []struct {
				Key string `json:"key"`
			} `json:"holders"`
		}
		if err := vantage.getJSON("/api/madnetwork/holders/"+rep.Hash, &resp); err == nil {
			for _, h := range resp.Holders {
				if _, want := expect[h.Key]; want {
					if _, seen := known[h.Key]; !seen {
						known[h.Key] = time.Since(waitStart)
					}
				}
			}
		}
		if len(known) < len(expect) {
			time.Sleep(2 * time.Second)
		}
	}
	for i := range rep.Spread {
		row := &rep.Spread[i]
		for key, name := range expect {
			if name != row.Node {
				continue
			}
			if d, ok := known[key]; ok {
				row.Known = round(d)
			} else if row.Note == "" {
				row.Note = fmt.Sprintf("the vantage never listed this holder within %s", spreadTimeout)
			}
		}
	}
	rep.Holders = len(known)
	if rep.Holders == 0 {
		return nil, fmt.Errorf("%s knows no holder for %s… — is the catalog synced? (meshlab status)", vantage.name, short(rep.Hash))
	}

	// ── The measured fetch ───────────────────────────────────────────────────
	if err := vantage.dropCachedBlob(rep.Hash); err != nil {
		return nil, fmt.Errorf("clearing the vantage cache: %v", err)
	}
	before, err := vantage.swarmPeerDown()
	if err != nil {
		return nil, fmt.Errorf("swarm peers before: %v", err)
	}

	t0 := time.Now()
	code, _, err := vantage.streamRange(rep.Hash, 0, 65535, timeout)
	switch {
	case err != nil:
		return nil, fmt.Errorf("first chunk: %v", err)
	case code != http.StatusOK && code != http.StatusPartialContent:
		return nil, fmt.Errorf("first chunk = %d", code)
	}
	rep.FirstChunk = round(time.Since(t0))

	t1 := time.Now()
	code, whole, err := vantage.streamBody(rep.Hash, timeout)
	switch {
	case err != nil:
		return nil, fmt.Errorf("whole file: %v", err)
	case code != http.StatusOK:
		return nil, fmt.Errorf("whole file = %d", code)
	}
	rep.Whole = round(time.Since(t1))
	rep.Bytes = len(whole)
	rep.Verified = sha256Hex(whole) == rep.Hash

	// The reader is done, but the TRANSFER may not be: fetchSwarm waits for
	// every worker, so a chunk dispatched to a holder behind a capped link
	// holds the transfer open after the last byte reached us — up to the
	// production per-chunk backstop (2 min) if that dispatch has to time out.
	// The per-peer counters land in the ledger when the transfer closes, so
	// wait for it, and REPORT the wait: a long settle is the price of a
	// straggling dispatch, which is exactly the kind of fact this command
	// exists to surface.
	settleStart := time.Now()
	settleDeadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(settleDeadline) {
		var live struct {
			Active []struct {
				Hash string `json:"hash"`
			} `json:"active"`
		}
		if err := vantage.getJSON("/api/admin/swarm/live", &live); err != nil {
			break
		}
		open := false
		for _, t := range live.Active {
			open = open || t.Hash == rep.Hash
		}
		if !open {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	var after map[string]int64
	settle := time.Now().Add(15 * time.Second)
	for {
		after, err = vantage.swarmPeerDown()
		if err != nil {
			return nil, fmt.Errorf("swarm peers after: %v", err)
		}
		var delta int64
		for key, down := range after {
			delta += down - before[key]
		}
		if delta >= int64(rep.Bytes) || time.Now().After(settle) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if d := time.Since(settleStart); d > 2*time.Second {
		rep.Settled = round(d)
	}

	// ── Balance ──────────────────────────────────────────────────────────────
	keyName := map[string]string{}
	for _, name := range l.names {
		keyName[l.nodes[name].publicKey()] = name
	}
	var total int64
	for key, down := range after {
		delta := down - before[key]
		if delta <= 0 {
			continue
		}
		total += delta
		name, ok := keyName[key]
		if !ok {
			name = "?"
		}
		rep.Balance = append(rep.Balance, swarmBalanceRow{Node: name, Key: key, Bytes: delta})
	}
	sort.Slice(rep.Balance, func(i, j int) bool { return rep.Balance[i].Bytes > rep.Balance[j].Bytes })
	for i := range rep.Balance {
		rep.Balance[i].Share = fmt.Sprintf("%.0f%%", float64(rep.Balance[i].Bytes)/float64(total)*100)
	}

	rep.Elapsed = round(time.Since(started))
	rep.Verdict = swarmVerdict(rep)
	return rep, nil
}

// swarmPeerDown snapshots the per-counterparty SESSION download counters.
// Diffing two snapshots around a fetch attributes its bytes per holder — the
// wire-level truth, counted by the accounting layer rather than claimed by the
// fetcher.
//
// Session, not the row's top-level bytes, deliberately: the top level is the
// STORED ledger, which the flusher writes on a 30-second cadence — an
// instrument that ticks twice a minute cannot time a 200 ms fetch, and the
// first cut of this command read it and reported empty balances. The nested
// session counters are the node's live in-memory table: per-read updates,
// monotone since the process started, never reset by a flush.
func (n *node) swarmPeerDown() (map[string]int64, error) {
	var resp struct {
		Peers []struct {
			Key     string `json:"key"`
			Session struct {
				Down int64 `json:"down_bytes"`
			} `json:"session"`
		} `json:"peers"`
	}
	if err := n.getJSON("/api/admin/swarm/peers", &resp); err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(resp.Peers))
	for _, p := range resp.Peers {
		out[p.Key] += p.Session.Down
	}
	return out, nil
}

func swarmVerdict(rep *swarmReport) string {
	if !rep.Verified {
		return "THE FETCH DID NOT VERIFY — the bytes that arrived do not hash to the subject. That is never acceptable; look at the vantage's log."
	}
	if len(rep.Balance) == 0 {
		return "The fetch completed but no per-peer delta was recorded — most likely it was served " +
			"from the vantage's own cache. The cache is cleared before each run, so seeing this " +
			"repeatedly is worth investigating."
	}
	if len(rep.Balance) == 1 {
		b := rep.Balance[0]
		if rep.Holders > 1 {
			return fmt.Sprintf(
				"%d holders were known but only %s served bytes — the others were dead or refusing, "+
					"and the fetch completed anyway. Their cost shows in the first-64K time, not in the "+
					"outcome.", rep.Holders, b.Node)
		}
		return fmt.Sprintf(
			"One holder (%s) carried the whole fetch. With more nodes in the lab, `meshlab swarm` "+
				"spreads the subject first — a pair lab always reads 100%%.", b.Node)
	}
	lead := rep.Balance[0]
	s := fmt.Sprintf(
		"%d holders served the fetch; %s carried the plurality (%s of %d bytes). The split follows "+
			"the LINKS, not the friend graph: cap a holder (`meshlab link %s-… bandwidth 65536`) or "+
			"kill one mid-fetch and run this again to watch the balance move.",
		len(rep.Balance), lead.Node, lead.Share, rep.Bytes, lead.Node)
	return s
}
