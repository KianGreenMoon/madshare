//go:build tests

// Command netfaultd runs netfault relays as a standalone process with a small
// JSON control API, so link conditions can be changed while a browser session is
// open against a running madshare — the thing an in-process Proxy cannot do.
//
// It carries the `tests` build tag for one concrete reason: `go install ./...`,
// unlike `go build ./...`, writes every main package into GOBIN, and this is
// precisely the binary that must not appear next to `madshare` in someone's
// /usr/bin. The tag is a packaging safeguard and nothing more — see
// docs/plans/mesh-testing.md §Gating, and do not extend it to the netfault
// library or the chaos suite, which must keep compiling on every `go test ./...`.
//
// # Safety
//
// This is structurally an open relay with a control API that can retarget it.
// Both the control listener and every relay bind loopback, and every target must
// be loopback, unless -allow-remote is passed — which logs a warning naming the
// risk. There is no config file, no daemonization and no init script: it is a
// foreground process you start and kill, and nothing about it should look
// installable.
//
//	go build -tags tests -o tests/mesh/bin/ ./tests/mesh/cmd/...
//	tests/mesh/bin/netfaultd -link a-b=127.0.0.1:9001 -link b-c=quic://127.0.0.1:9002
//
//	curl localhost:7777/links
//	curl -X PUT localhost:7777/links/a-b -d '{"down":{"latency":"200ms"},"partition":false}'
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"daemonlord.ygg/madshare/tests/mesh/netfault"
)

func main() {
	var (
		links       linkFlags
		control     = flag.String("control", "127.0.0.1:7777", "control API bind address")
		allowRemote = flag.Bool("allow-remote", false,
			"permit non-loopback binds and targets — turns this into an open relay; see -h")
	)
	flag.Var(&links, "link", "a relay, as name=target (target may carry a tcp:// or quic:// scheme; repeatable)")
	flag.Usage = usage
	flag.Parse()

	if len(links) == 0 {
		fmt.Fprintln(os.Stderr, "netfaultd: at least one -link is required")
		flag.Usage()
		os.Exit(2)
	}
	logger := log.New(os.Stderr, "netfaultd: ", log.LstdFlags)
	if *allowRemote {
		logger.Printf("WARNING: -allow-remote is set. This process is an open relay with a "+
			"control API that can retarget it; anything that reaches %s can make it "+
			"forward traffic to any address this host can dial.", *control)
	}

	set, err := openLinks(links, *allowRemote, logger)
	if err != nil {
		logger.Fatal(err)
	}
	defer set.closeAll()

	srv := &http.Server{Addr: *control, Handler: set.routes(logger)}
	if !*allowRemote {
		if err := requireLoopback(*control); err != nil {
			logger.Fatalf("control API %v", err)
		}
	}
	ln, err := net.Listen("tcp", *control)
	if err != nil {
		logger.Fatalf("control API listen %s: %v", *control, err)
	}
	logger.Printf("control API on http://%s", ln.Addr())

	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			logger.Printf("control API: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	logger.Print("shutting down")
	srv.Close()
}

func usage() {
	out := flag.CommandLine.Output()
	fmt.Fprint(out, `netfaultd — standalone netfault relays with a JSON control API.

  netfaultd -link NAME=TARGET [-link NAME=TARGET ...] [-control ADDR] [-allow-remote]

A TARGET may carry a scheme: tcp://host:port (default) relays a byte stream,
quic://host:port relays datagrams. The two accept different knobs — a stream
cannot express packet loss and a datagram cannot express write slicing.

Control API:
  GET  /links            every link's fault and counters
  GET  /links/{name}     one link
  PUT  /links/{name}     replace one link's fault (JSON body)

Fault JSON (durations are strings: "200ms", "1.5s"):
  tcp    {"up":{...},"down":{...},"partition":bool,
          "kill_after_bytes":int,"kill_after_time":"dur"}
         dir: latency, jitter, bandwidth (bytes/s), slice, slice_delay
  quic   {"up":{...},"down":{...},"partition":bool}
         dir: latency, jitter, bandwidth, loss, duplicate, reorder, reorder_delay
              (loss/duplicate/reorder are probabilities in [0,1])

SAFETY: this is an open relay with a control API that can retarget it. Binds and
targets are loopback-only unless -allow-remote is given. Never run it on a shared
host. Full options:
`)
	flag.PrintDefaults()
}

// ── Links ────────────────────────────────────────────────────────────────────

// linkFlags collects repeated -link name=target values in order.
type linkFlags []string

func (l *linkFlags) String() string     { return strings.Join(*l, ",") }
func (l *linkFlags) Set(v string) error { *l = append(*l, v); return nil }

// link is one relay, either transport. Exactly one of stream/datagram is set;
// keeping them in one type is what lets the control API address every link
// uniformly while still refusing knobs the transport cannot honour.
type link struct {
	name     string
	target   string
	stream   *netfault.Proxy
	datagram *netfault.UDPProxy
}

func (l *link) addr() string {
	if l.stream != nil {
		return l.stream.Addr()
	}
	return l.datagram.Addr()
}

func (l *link) transport() string {
	if l.stream != nil {
		return "tcp"
	}
	return "quic"
}

func (l *link) close() {
	if l.stream != nil {
		l.stream.Close()
		return
	}
	l.datagram.Close()
}

type linkSet struct {
	order  []string
	byName map[string]*link
}

func openLinks(specs []string, allowRemote bool, logger *log.Logger) (*linkSet, error) {
	set := &linkSet{byName: map[string]*link{}}
	opts := netfault.Options{AllowRemote: allowRemote}
	for _, spec := range specs {
		name, target, ok := strings.Cut(spec, "=")
		if !ok || name == "" || target == "" {
			set.closeAll()
			return nil, fmt.Errorf("bad -link %q, want name=target", spec)
		}
		if _, dup := set.byName[name]; dup {
			set.closeAll()
			return nil, fmt.Errorf("duplicate link name %q", name)
		}
		l := &link{name: name, target: target}
		scheme, hostport, hasScheme := strings.Cut(target, "://")
		if !hasScheme {
			scheme, hostport = "tcp", target
		}
		l.target = hostport

		var err error
		switch scheme {
		case "tcp", "tls", "ws", "wss":
			l.stream, err = netfault.NewWithOptions(hostport, netfault.Fault{}, opts)
		case "quic":
			l.datagram, err = netfault.NewUDPWithOptions(hostport, netfault.DatagramFault{}, opts)
		default:
			err = fmt.Errorf("unknown transport %q (want tcp or quic)", scheme)
		}
		if err != nil {
			set.closeAll()
			// The library's loopback refusal names its Go field; from a command
			// line the reader needs the flag.
			msg := strings.ReplaceAll(err.Error(), "set Options.AllowRemote", "pass -allow-remote")
			return nil, fmt.Errorf("link %s: %s", name, msg)
		}
		set.order = append(set.order, name)
		set.byName[name] = l
		logger.Printf("link %s: %s://%s -> %s (transparent)", name, l.transport(), l.addr(), hostport)
	}
	return set, nil
}

func (s *linkSet) closeAll() {
	for _, name := range s.order {
		s.byName[name].close()
	}
}

// ── Control API ──────────────────────────────────────────────────────────────

func (s *linkSet) routes(logger *log.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/links", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeErr(w, http.StatusMethodNotAllowed, "GET only")
			return
		}
		out := make([]any, 0, len(s.order))
		for _, name := range s.order {
			out = append(out, s.byName[name].describe())
		}
		writeJSON(w, http.StatusOK, map[string]any{"links": out})
	})
	mux.HandleFunc("/links/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/links/")
		l, ok := s.byName[name]
		if !ok {
			writeErr(w, http.StatusNotFound, "no link %q (have: %s)", name, strings.Join(s.order, ", "))
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, l.describe())
		case http.MethodPut:
			applied, err := l.apply(r)
			if err != nil {
				writeErr(w, http.StatusBadRequest, "%v", err)
				return
			}
			logger.Printf("link %s <- %s", name, applied)
			writeJSON(w, http.StatusOK, l.describe())
		default:
			writeErr(w, http.StatusMethodNotAllowed, "GET or PUT")
		}
	})
	return mux
}

func (l *link) describe() map[string]any {
	m := map[string]any{
		"name": l.name, "transport": l.transport(),
		"addr": l.addr(), "target": l.target,
	}
	if l.stream != nil {
		m["fault"], m["stats"] = l.stream.Fault().ToJSON(), l.stream.Stats()
	} else {
		m["fault"], m["stats"] = l.datagram.Fault().ToJSON(), l.datagram.Stats()
	}
	return m
}

// apply replaces a link's fault from a request body. A PUT is a full
// replacement, not a merge: a partial merge would make "heal this link" depend
// on what was set before, and the one operation a chaos session runs most often
// is putting a link back to perfect.
func (l *link) apply(r *http.Request) (string, error) {
	r.Body = http.MaxBytesReader(nil, r.Body, 8<<10)
	if l.stream != nil {
		var in netfault.FaultJSON
		if err := decodeStrict(r, &in); err != nil {
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
	if err := decodeStrict(r, &in); err != nil {
		return "", err
	}
	f, err := in.Fault()
	if err != nil {
		return "", err
	}
	l.datagram.Set(f)
	return summarize(f.ToJSON()), nil
}

// summarize renders a fault for the log line — its JSON with the zero fields
// dropped, which is what every omitempty above is for.
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

// decodeStrict rejects unknown fields, which is how a knob aimed at the wrong
// transport is caught: "loss" on a tcp link is a typo with a silent, invisible
// effect otherwise — the link would simply stay perfect and the session would
// draw conclusions from it.
func decodeStrict(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid fault JSON: %w", err)
	}
	return nil
}

// ── Helpers ──────────────────────────────────────────────────────────────────

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

// requireLoopback mirrors the library's check for the control listener, which
// the library never sees.
func requireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("address %q: %w", addr, err)
	}
	if host == "" {
		return fmt.Errorf("address %q binds every interface; pass -allow-remote if that is truly intended", addr)
	}
	ips := []net.IP{}
	if ip := net.ParseIP(host); ip != nil {
		ips = append(ips, ip)
	} else {
		resolved, err := net.LookupIP(host)
		if err != nil {
			return fmt.Errorf("address %q: %w", addr, err)
		}
		ips = resolved
	}
	for _, ip := range ips {
		if !ip.IsLoopback() {
			return fmt.Errorf("address %q resolves to non-loopback %s; pass -allow-remote if that is truly intended", addr, ip)
		}
	}
	return nil
}
