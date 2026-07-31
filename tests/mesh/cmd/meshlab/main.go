//go:build tests && !nofederation

// Command meshlab runs a lab of real madshare processes on one machine, peered
// over faulted links, so availability, fail-open and Materialize can be watched
// in an actual browser under an actual bad network.
//
// It is the complement to the in-process chaos suite: that one asserts what the
// swarm does, in Go, in milliseconds; this one shows what a person sees. The
// open verification in docs/plans/availability.md — "reproduce on a real
// lossy/latent mesh, not loopback, that transfers no longer stall and
// availability doesn't flap" — is the thing it exists to close.
//
// # Usage
//
//	meshlab up -topology hub -nodes 3        # foreground; Ctrl-C tears it down
//	meshlab status                           # from another shell
//	meshlab seed -audio ~/music
//	meshlab link b-a latency 200ms jitter 80ms
//	meshlab kill c ; meshlab restart c
//	meshlab partition b ; meshlab heal b
//	meshlab flap b -down 10s -up 20s
//
// # Safety
//
// meshlab provisions servers with KNOWN, HARDCODED admin credentials
// (node.go), binds every node and its own control API to loopback, and writes
// everything under a disposable lab root. It carries the `tests` build tag for
// the same packaging reason netfaultd does — `go install ./...` would otherwise
// drop it in GOBIN. Never run it on a shared host, and never point it at a data
// directory you care about: `up` starts nodes that will happily migrate and
// write to whatever is there.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"daemonlord.ygg/madshare/tests/mesh/netfault"
)

const defaultControl = "127.0.0.1:7788"

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch cmd := os.Args[1]; cmd {
	case "up":
		cmdUp(os.Args[2:])
	case "status":
		cmdStatus(os.Args[2:])
	case "link":
		cmdLink(os.Args[2:])
	case "seed":
		cmdSeed(os.Args[2:])
	case "scope":
		cmdScope(os.Args[2:])
	case "check":
		cmdCheck(os.Args[2:])
	case "friend":
		cmdFriend(os.Args[2:])
	case "reach":
		cmdReach(os.Args[2:])
	case "kill", "restart", "partition", "heal":
		cmdNodeAction(cmd, os.Args[2:])
	case "flap":
		cmdFlap(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "meshlab: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `meshlab — a lab of real madshare processes over faulted links.

  meshlab up [-topology %s] [-nodes N] [-friends all|adjacent|none|a-b,...]
             [-transport tcp|quic] [-seed AUDIODIR] [-root DIR] [-bin PATH]
  meshlab status
  meshlab seed [-audio DIR] [-per-node N]
  meshlab link NAME KNOB VALUE...      e.g. link b-a latency 200ms bandwidth 65536
  meshlab link NAME clear              back to a perfect link
  meshlab kill|restart|partition|heal NODE
  meshlab flap NODE [-down 10s] [-up 20s]
  meshlab scope NODE default DEPTH     node-wide sharing scope (F5)
  meshlab scope NODE tracks DEPTH|guest on|off [-limit N]
  meshlab friend A B                   friend two RUNNING nodes (a-c after a-b,b-c)
  meshlab check                        assert the sharing-scope rules
  meshlab reach [-runs N] [-no-fetch]  what does friendship DISTANCE cost?

'up' runs in the foreground and holds the lab; every other command talks to it
over the control API (-control, default %s).

TWO GRAPHS. -topology chooses the UNDERLAY peering graph; -friends chooses who
is friends with whom, separately. Federation is friends-only and direct, so
'-friends adjacent' on a chain gives you nodes that can route to each other but
must see nothing of each other's libraries — which is a thing worth testing.

SEED AT 'up', not after. A friend's catalog is pulled only when it is older than
the 15-minute sync interval, and that timestamp is stored, so friending an empty
node means waiting 15 minutes to see anything. 'up -seed DIR' seeds before
friending, and the nudge that fires on a new friendship pulls a full catalog at
once. 'meshlab seed' afterwards works, it is just slow to show up.

SHARING SCOPE (F5). 'scope' sets how far content travels — DEPTH is one of
private, friends, network, inherit, or a hop count. 'check' then asserts the
rules from an OUTSIDER's position: it starts a real madnetwork node that is
nobody's friend and asks each server directly, which is the only way to see the
guest-open swarm (a stranger may fetch guest-playable bytes and nothing else).

DISTANCE (F7). 'reach' measures what being far away in the FRIENDSHIP graph
actually costs, on a chain where friendship distance and underlay distance
coincide:

    meshlab up -nodes 7 -topology chain -friends adjacent -seed ./audio
    meshlab reach

It reports the mesh RTT to every node by friendship distance (ping is open to
strangers, so this measures routing alone) and then tries a real content fetch
from the first node. Before F7 every fetch past distance 1 fails — a non-friend
is not a provider — and that failure is the gap stated as a measurement.

SAFETY: known hardcoded admin credentials, loopback-only, disposable lab root.
Never on a shared host.
`, strings.Join(presetNames(), "|"), defaultControl)
}

// ── up ───────────────────────────────────────────────────────────────────────

func cmdUp(args []string) {
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	topName := fs.String("topology", "pair", "underlay shape: "+strings.Join(presetNames(), ", "))
	nodes := fs.Int("nodes", 0, "node count (0 = the preset's default)")
	friends := fs.String("friends", "all", "friendship graph: all, adjacent, none, or a-b,b-c")
	transport := fs.String("transport", "tcp", "underlay transport: tcp or quic (quic is what packet loss needs)")
	root := fs.String("root", "", "lab root directory (default: a fresh temp dir)")
	bin := fs.String("bin", "", "madshare binary (default: build one into the lab root)")
	control := fs.String("control", defaultControl, "control API bind address")
	keep := fs.Bool("keep", false, "keep the lab root on exit instead of deleting it")
	seedDir := fs.String("seed", "", "seed each node from this audio directory before friending "+
		"(strongly preferred over `meshlab seed` later — see -h)")
	perNode := fs.Int("per-node", 0, "files per node when seeding (0 = split evenly)")
	fs.Parse(args)

	if *transport != "tcp" && *transport != "quic" {
		fatalf("-transport must be tcp or quic, got %q", *transport)
	}
	preset, ok := presets[*topName]
	if !ok {
		fatalf("unknown -topology %q (have: %s)", *topName, strings.Join(presetNames(), ", "))
	}
	top, err := preset(*nodes)
	if err != nil {
		fatalf("%v", err)
	}
	if top.friends, err = resolveFriends(*friends, top); err != nil {
		fatalf("%v", err)
	}

	if repoRoot, err = findRepoRoot(); err != nil {
		fatalf("%v", err)
	}
	labRoot := *root
	if labRoot == "" {
		if labRoot, err = os.MkdirTemp("", "meshlab-"); err != nil {
			fatalf("lab root: %v", err)
		}
	} else if err := os.MkdirAll(labRoot, 0o755); err != nil {
		fatalf("lab root: %v", err)
	}

	logger := log.New(os.Stderr, "meshlab: ", log.LstdFlags)
	binPath := *bin
	if binPath == "" {
		logger.Print("building madshare (pass -bin to skip)")
		if binPath, err = buildMadshare(labRoot); err != nil {
			fatalf("%v", err)
		}
	}
	if binPath, err = filepath.Abs(binPath); err != nil {
		fatalf("%v", err)
	}

	logger.Printf("topology %s over %s: nodes %s, links %s", top.name, *transport,
		strings.Join(top.nodes, " "), edgeNames(top.edges))
	logger.Printf("friends (%s): %s", *friends, pairNames(top.friends))
	logger.Printf("lab root %s", labRoot)

	l, err := newLab(labRoot, binPath, *transport, top, logger)
	if err != nil {
		fatalf("%v", err)
	}

	// Tear down on signal *and* on a failed start, so a half-built lab never
	// leaves madshare processes behind holding ports.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	defer func() {
		l.stop()
		if !*keep && *root == "" {
			os.RemoveAll(labRoot)
		} else {
			logger.Printf("lab root kept at %s", labRoot)
		}
	}()

	done := make(chan error, 1)
	go func() { done <- l.start(*seedDir, *perNode) }()
	select {
	case err := <-done:
		if err != nil {
			logger.Printf("startup failed: %v", err)
			return
		}
	case <-stop:
		logger.Print("interrupted during startup")
		return
	}

	if err := requireLoopback(*control); err != nil {
		logger.Printf("control API %v", err)
		return
	}
	ln, err := net.Listen("tcp", *control)
	if err != nil {
		logger.Printf("control API listen %s: %v", *control, err)
		return
	}
	srv := &http.Server{Handler: l.routes()}
	go srv.Serve(ln)
	logger.Printf("control API on http://%s — lab is up, Ctrl-C to tear down", ln.Addr())
	for _, name := range l.names {
		logger.Printf("  %s  http://%s", name, l.nodes[name].httpAdr)
	}

	<-stop
	logger.Print("tearing down")
	srv.Close()
}

// buildMadshare compiles the server the lab will run, from the checkout meshlab
// itself lives in. Building rather than hunting a binary keeps the lab honest:
// it tests the working tree, which is the whole reason to run it.
func buildMadshare(into string) (string, error) {
	out := filepath.Join(into, "madshare")
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Dir = repoRoot
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("building madshare: %w", err)
	}
	return out, nil
}

// findRepoRoot walks up from the working directory to the module root.
func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("meshlab must run inside the madshare checkout (no go.mod found above the working directory)")
		}
		dir = parent
	}
}

// ── Client commands ──────────────────────────────────────────────────────────

func cmdStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	control := fs.String("control", defaultControl, "control API address")
	asJSON := fs.Bool("json", false, "print the raw JSON")
	fs.Parse(args)

	raw, err := call(*control, http.MethodGet, "/status", nil)
	if err != nil {
		fatalf("%v", err)
	}
	if *asJSON {
		os.Stdout.Write(raw)
		return
	}
	printStatus(raw)
}

func printStatus(raw []byte) {
	var st struct {
		Root               string                  `json:"root"`
		ReachableWindowSec int                     `json:"reachable_window_sec"`
		Friends            []string                `json:"friends"`
		Flapping           []string                `json:"flapping"`
		Scope              map[string]scopeSummary `json:"scope"`
		Nodes              []nodeStatus            `json:"nodes"`
		Links              []struct {
			Name      string          `json:"name"`
			Transport string          `json:"transport"`
			Fault     json.RawMessage `json:"fault"`
		} `json:"links"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		fatalf("decoding status: %v", err)
	}
	fmt.Printf("lab root %s   freshness window %ds\n", st.Root, st.ReachableWindowSec)
	fmt.Printf("friends: %s\n", orDefault(strings.Join(st.Friends, " "), "(none)"))
	if len(st.Flapping) > 0 {
		fmt.Printf("flapping: %s\n", strings.Join(st.Flapping, " "))
	}
	fmt.Println("\nNODES")
	for _, n := range st.Nodes {
		state := "up"
		if !n.Running {
			state = "DOWN"
		}
		health := ""
		if n.Running && !n.InboundHealthy {
			health = "  INBOUND DEAD (browse fails open)"
		}
		fmt.Printf("  %-4s %-5s %-24s %-14s library %-3d madnetwork %-3d%s\n",
			n.Name, state, n.URL, n.Key, n.Tracks, n.Madnetwork, health)
		if n.Error != "" {
			fmt.Printf("        ! %s\n", n.Error)
		}
		for _, p := range n.Peers {
			mark := "stale"
			if p.Reachable {
				mark = "fresh"
			}
			age := p.LastSeenAge
			if age == "" {
				age = "never"
			}
			fmt.Printf("        peer %-14s %-16s seen %-8s %s\n", p.Name, p.State, age, mark)
		}
	}
	fmt.Println("\nLINKS")
	for _, l := range st.Links {
		fmt.Printf("  %-8s %-5s %s\n", l.Name, l.Transport, summarizeRaw(l.Fault))
	}
	if len(st.Scope) > 0 {
		fmt.Println("\nSCOPE")
		for _, n := range st.Nodes {
			if s, ok := st.Scope[n.Name]; ok {
				fmt.Printf("  %-4s default %-10s  %d private  %d guest-playable\n",
					n.Name, s.Default, s.Private, s.Guest)
			}
		}
	}
}

// summarizeRaw renders a fault on one line. The control API pretty-prints its
// JSON, so this re-compacts before comparing — otherwise every link reads as
// degraded because the indented form never equals the transparent one.
func summarizeRaw(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return strings.TrimSpace(string(raw))
	}
	s := buf.String()
	if s == "" || s == `{"up":{},"down":{}}` {
		return "transparent"
	}
	return s
}

// cmdLink builds a fault from KNOB VALUE pairs, so the common cases need no
// JSON at the prompt: `meshlab link b-a latency 200ms bandwidth 65536`.
func cmdLink(args []string) {
	fs := flag.NewFlagSet("link", flag.ExitOnError)
	control := fs.String("control", defaultControl, "control API address")
	dir := fs.String("dir", "both", "which direction the knobs apply to: up, down, both")
	// Knobs are positional after the link name, so parse flags from the tail.
	name, rest := splitLeading(args)
	fs.Parse(flagsIn(rest))
	if name == "" {
		fatalf("link needs a name, e.g. `meshlab link b-a latency 200ms` (see `meshlab status` for names)")
	}
	knobs := knobsIn(rest)

	body, err := buildFault(knobs, *dir, *control, name)
	if err != nil {
		fatalf("%v", err)
	}
	raw, err := call(*control, http.MethodPut, "/links/"+name, body)
	if err != nil {
		fatalf("%v", err)
	}
	var out struct {
		Fault json.RawMessage `json:"fault"`
	}
	json.Unmarshal(raw, &out)
	fmt.Printf("%s <- %s\n", name, summarizeRaw(out.Fault))
}

// buildFault turns KNOB VALUE pairs into the wire form for the link's
// transport, which it learns by asking — so `loss` against a tcp link fails
// here with a readable message rather than as an unknown JSON field.
func buildFault(knobs []string, dir, control, name string) ([]byte, error) {
	if len(knobs) == 1 && (knobs[0] == "clear" || knobs[0] == "heal") {
		return []byte(`{"up":{},"down":{}}`), nil
	}
	if len(knobs)%2 != 0 {
		return nil, fmt.Errorf("knobs come in pairs: %s", strings.Join(knobs, " "))
	}
	raw, err := call(control, http.MethodGet, "/links/"+name, nil)
	if err != nil {
		return nil, err
	}
	var probe struct {
		Transport string `json:"transport"`
	}
	json.Unmarshal(raw, &probe)

	fields := map[string]any{}
	for i := 0; i < len(knobs); i += 2 {
		k, v := knobs[i], knobs[i+1]
		if k == "partition" {
			fields["partition"] = v == "true" || v == "on" || v == "1"
			continue
		}
		parsed, err := knobValue(k, v)
		if err != nil {
			return nil, err
		}
		fields[k] = parsed
	}
	partition, _ := fields["partition"].(bool)
	delete(fields, "partition")

	out := map[string]any{"up": map[string]any{}, "down": map[string]any{}, "partition": partition}
	switch dir {
	case "up":
		out["up"] = fields
	case "down":
		out["down"] = fields
	case "both":
		out["up"], out["down"] = fields, fields
	default:
		return nil, fmt.Errorf("-dir must be up, down or both, got %q", dir)
	}
	b, _ := json.Marshal(out)
	// Validate locally against the right type so the error names the knob.
	if probe.Transport == "quic" {
		var in netfault.DatagramFaultJSON
		if err := strictUnmarshal(b, &in); err != nil {
			return nil, fmt.Errorf("%w — this is a quic link; loss/duplicate/reorder yes, slice no", err)
		}
		if _, err := in.Fault(); err != nil {
			return nil, err
		}
	} else {
		var in netfault.FaultJSON
		if err := strictUnmarshal(b, &in); err != nil {
			return nil, fmt.Errorf("%w — this is a tcp link; slice/kill_after yes, loss no "+
				"(a stream cannot lose a packet, the kernel resends it)", err)
		}
		if _, err := in.Fault(); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// knobValue types a value by knob name: durations stay strings, rates and sizes
// become numbers, probabilities become floats.
func knobValue(k, v string) (any, error) {
	switch k {
	case "latency", "jitter", "slice_delay", "reorder_delay", "kill_after_time":
		if _, err := time.ParseDuration(v); err != nil {
			return nil, fmt.Errorf("%s: %w (want a duration like 200ms)", k, err)
		}
		return v, nil
	case "bandwidth", "slice", "kill_after_bytes":
		var n int64
		if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
			return nil, fmt.Errorf("%s: want an integer, got %q", k, v)
		}
		return n, nil
	case "loss", "duplicate", "reorder":
		var f float64
		if _, err := fmt.Sscanf(v, "%g", &f); err != nil {
			return nil, fmt.Errorf("%s: want a probability in [0,1], got %q", k, v)
		}
		return f, nil
	default:
		return nil, fmt.Errorf("unknown knob %q (latency, jitter, bandwidth, slice, slice_delay, "+
			"loss, duplicate, reorder, reorder_delay, kill_after_bytes, kill_after_time, partition, clear)", k)
	}
}

func cmdNodeAction(action string, args []string) {
	fs := flag.NewFlagSet(action, flag.ExitOnError)
	control := fs.String("control", defaultControl, "control API address")
	name, rest := splitLeading(args)
	fs.Parse(rest)
	if name == "" {
		fatalf("%s needs a node name", action)
	}
	if _, err := call(*control, http.MethodPost, "/nodes/"+name+"/"+action, nil); err != nil {
		fatalf("%v", err)
	}
	fmt.Printf("%s %s\n", action, name)
}

func cmdFlap(args []string) {
	fs := flag.NewFlagSet("flap", flag.ExitOnError)
	control := fs.String("control", defaultControl, "control API address")
	down := fs.String("down", "10s", "how long each outage lasts")
	up := fs.String("up", "20s", "how long each recovery window lasts")
	name, rest := splitLeading(args)
	fs.Parse(rest)
	if name == "" {
		fatalf("flap needs a node name")
	}
	body, _ := json.Marshal(map[string]string{"down": *down, "up": *up})
	if _, err := call(*control, http.MethodPost, "/nodes/"+name+"/flap", body); err != nil {
		fatalf("%v", err)
	}
	fmt.Printf("flapping %s: %s down / %s up (`meshlab heal %s` to stop)\n", name, *down, *up, name)
}

func cmdSeed(args []string) {
	fs := flag.NewFlagSet("seed", flag.ExitOnError)
	control := fs.String("control", defaultControl, "control API address")
	audio := fs.String("audio", "", "audio directory (default: $TEST_AUDIO_DIR)")
	perNode := fs.Int("per-node", 0, "files per node (0 = split what is there evenly)")
	fs.Parse(args)

	body, _ := json.Marshal(map[string]any{"dir": *audio, "per_node": *perNode})
	raw, err := call(*control, http.MethodPost, "/seed", body)
	if err != nil {
		fatalf("%v", err)
	}
	var rep seedReport
	json.Unmarshal(raw, &rep)
	fmt.Printf("seeded %d files from %s (%d per node)\n", rep.Total, rep.Dir, rep.PerNode)
	for name, n := range rep.Nodes {
		fmt.Printf("  %s: %d\n", name, n)
	}
	for name, e := range rep.Errors {
		fmt.Printf("  %s: ERROR %s\n", name, e)
	}
	if rep.Total > 0 {
		fmt.Println("\nNote: friends pull a catalog only when theirs is older than the " +
			"15-minute sync interval, so these tracks may take that long to appear on\n" +
			"the other nodes' /madnetwork. `meshlab up -seed DIR` avoids the wait by " +
			"seeding before friending.")
	}
}

// cmdScope drives the F5 knobs: `meshlab scope a default private`,
// `meshlab scope a tracks guest on -limit 1`.
func cmdScope(args []string) {
	fs := flag.NewFlagSet("scope", flag.ExitOnError)
	control := fs.String("control", defaultControl, "control API address")
	limit := fs.Int("limit", 0, "with `tracks`: touch only the first N recordings (0 = all)")
	words := knobsIn(args)
	fs.Parse(flagsIn(args))

	if len(words) == 0 {
		raw, err := call(*control, http.MethodGet, "/status", nil)
		if err != nil {
			fatalf("%v", err)
		}
		printScope(raw)
		return
	}
	if len(words) < 3 {
		fatalf("scope NODE default DEPTH  |  scope NODE tracks DEPTH  |  scope NODE tracks guest on|off")
	}
	req := scopeRequest{Node: words[0], Target: words[1], Limit: *limit}
	switch {
	case words[2] == "guest" && len(words) >= 4:
		req.Guest = words[3]
	case words[2] == "guest":
		fatalf("guest needs on or off")
	default:
		req.Depth = words[2]
	}
	body, _ := json.Marshal(req)
	raw, err := call(*control, http.MethodPost, "/scope", body)
	if err != nil {
		fatalf("%v", err)
	}
	var rep scopeReport
	json.Unmarshal(raw, &rep)
	if rep.Affected > 0 {
		fmt.Printf("%s: %d recording(s) -> %s\n", rep.Node, rep.Affected, rep.Applied)
		for _, t := range rep.Titles {
			fmt.Printf("  %s\n", t)
		}
		return
	}
	fmt.Printf("%s: %s\n", rep.Node, rep.Applied)
}

func printScope(raw []byte) {
	var st struct {
		Scope map[string]scopeSummary `json:"scope"`
		Nodes []nodeStatus            `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		fatalf("decoding status: %v", err)
	}
	fmt.Println("SHARING SCOPE")
	for _, n := range st.Nodes {
		s, ok := st.Scope[n.Name]
		if !ok {
			fmt.Printf("  %-4s (down)\n", n.Name)
			continue
		}
		fmt.Printf("  %-4s default %-10s  %d private  %d guest-playable  (%d recording(s) override the default)\n",
			n.Name, s.Default, s.Private, s.Guest, s.Pinned)
	}
}

// cmdCheck runs the scope assertion pass and exits non-zero on a failure, so it
// is usable from a script as well as by eye.
// cmdFriend adds a friendship to a running lab. `up -friends` fixes the graph at
// startup; this is how the friend-of-a-friend case is built, which is the one an
// admin actually meets:
//
//	meshlab up -nodes 3 -topology chain -friends a-b,b-c -seed ./audio
//	meshlab friend a c
func cmdFriend(args []string) {
	fs := flag.NewFlagSet("friend", flag.ExitOnError)
	control := fs.String("control", defaultControl, "control API address")
	// Positionals may precede the flags here, as in `scope`, so the two are
	// separated before parsing rather than left to flag's stop-at-first-operand.
	words := knobsIn(args)
	fs.Parse(flagsIn(args))
	if len(words) != 2 {
		fatalf("usage: meshlab friend NODE NODE")
	}
	a, b := words[0], words[1]
	body, _ := json.Marshal(map[string]string{"a": a, "b": b})
	if _, err := call(*control, http.MethodPost, "/friend", body); err != nil {
		fatalf("%v", err)
	}
	fmt.Printf("%s and %s are friends\n", a, b)
}

func cmdCheck(args []string) {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	control := fs.String("control", defaultControl, "control API address")
	asJSON := fs.Bool("json", false, "print the raw JSON")
	fs.Parse(args)

	raw, err := call(*control, http.MethodPost, "/check", nil)
	if err != nil {
		fatalf("%v", err)
	}
	if *asJSON {
		os.Stdout.Write(raw)
		return
	}
	var rep checkReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		fatalf("decoding report: %v", err)
	}
	for _, c := range rep.Cases {
		mark := "PASS"
		switch {
		case c.Skipped:
			mark = "SKIP"
		case !c.OK:
			mark = "FAIL"
		}
		fmt.Printf("%s  %-46s %s\n", mark, c.Name, c.Detail)
	}
	fmt.Printf("\n%d passed, %d failed, %d skipped in %s\n", rep.Passed, rep.Failed, rep.Skipped, rep.Elapsed)
	if rep.Failed > 0 {
		os.Exit(1)
	}
}

// ── Control client ───────────────────────────────────────────────────────────

func call(control, method, path string, body []byte) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = strings.NewReader(string(body))
	}
	req, err := http.NewRequest(method, "http://"+control+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("no lab at %s — is `meshlab up` running? (%w)", control, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		json.Unmarshal(raw, &e)
		if e.Error != "" {
			return nil, errors.New(e.Error)
		}
		return nil, fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	return raw, nil
}

// ── Arg helpers ──────────────────────────────────────────────────────────────

// splitLeading peels the first non-flag argument off, so `meshlab kill c
// -control X` and `meshlab kill -control X c` both work.
func splitLeading(args []string) (string, []string) {
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a, append(append([]string{}, args[:i]...), args[i+1:]...)
		}
	}
	return "", args
}

// flagsIn / knobsIn split `latency 200ms -dir down` into knobs and flags.
func flagsIn(args []string) []string {
	for i, a := range args {
		if strings.HasPrefix(a, "-") {
			return args[i:]
		}
	}
	return nil
}

func knobsIn(args []string) []string {
	for i, a := range args {
		if strings.HasPrefix(a, "-") {
			return args[:i]
		}
	}
	return args
}

// requireLoopback keeps the control API off the network — it can kill and
// restart servers and retarget links.
func requireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("address %q: %w", addr, err)
	}
	if host == "" {
		return fmt.Errorf("address %q binds every interface; meshlab is loopback-only by design", addr)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		ips, err := net.LookupIP(host)
		if err != nil || len(ips) == 0 {
			return fmt.Errorf("address %q does not resolve", addr)
		}
		ip = ips[0]
	}
	if !ip.IsLoopback() {
		return fmt.Errorf("address %q is not loopback; meshlab provisions servers with known "+
			"admin credentials and must never face a network", addr)
	}
	return nil
}

func edgeNames(edges []edge) string {
	out := make([]string, len(edges))
	for i, e := range edges {
		out[i] = e.from + "-" + e.to
	}
	return strings.Join(out, " ")
}

func pairNames(pairs [][2]string) string {
	if len(pairs) == 0 {
		return "(none)"
	}
	out := make([]string, len(pairs))
	for i, p := range pairs {
		out[i] = p[0] + "-" + p[1]
	}
	return strings.Join(out, " ")
}
