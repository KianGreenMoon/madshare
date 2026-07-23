// Package netfault injects network faults into a TCP link by sitting between
// its two ends.
//
// Madshare's federation tests already run real yggdrasil cores peered over a
// loopback TCP underlay (`startNodePair` in federation/transfer_test.go reserves
// a 127.0.0.1:0 port and hands it over as a tcp:// peering URI), so that underlay
// socket is the seam where an otherwise perfect mesh can be made hostile: a Proxy
// listens on loopback, dials the real endpoint, and pumps bytes through a
// per-direction fault pipeline — added latency and jitter, a bandwidth ceiling,
// write slicing, mid-stream connection kills, and full partitions. No root, no
// netem, no containers, and one implementation serves both `go test` and the
// meshlab harness.
//
// What this package deliberately cannot do is emulate packet loss. TCP is a
// reliable byte stream: dropping bytes would corrupt the connection rather than
// model a lossy path, and the kernel's retransmits mean real loss surfaces as
// stalls and resets, not as gaps. Genuine loss, reordering and duplication need
// the UDP (quic://) relay instead. Plan: docs/plans/mesh-testing.md.
package netfault

import (
	"fmt"
	"math/rand/v2"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// ── Fault model ──────────────────────────────────────────────────────────────

// Dir holds the conditions applied to one direction of a link. The zero Dir is
// a perfect wire, so a Fault only has to name what it degrades.
type Dir struct {
	// Latency delays delivery of everything crossing this direction. It is a
	// delay, not a throttle: throughput is unaffected (see Proxy.pump).
	Latency time.Duration
	// Jitter spreads each parcel's delay uniformly over ±Jitter around Latency,
	// clamped at zero. Delivery order is always preserved — see pump.
	Jitter time.Duration
	// Bandwidth caps this direction at bytes/second. 0 means unlimited.
	Bandwidth int64
	// Slice chops writes into at most this many bytes, with SliceDelay between
	// the pieces — a jammed path that dribbles rather than one that pauses.
	// 0 means write whatever arrives, whole.
	Slice      int
	SliceDelay time.Duration
}

// Fault is a complete link condition: per-direction degradation plus the
// link-level knobs that apply to the connection as a whole. The zero Fault is a
// transparent proxy, which is what makes it a usable baseline in tests.
type Fault struct {
	// Up is client→target (the dialing node's traffic); Down is target→client.
	// Keeping them separate is the point: a real bad link is usually asymmetric.
	Up   Dir
	Down Dir

	// Partition refuses new connections and kills live ones. Healing is the same
	// knob flipped back — the proxy keeps listening throughout, so the peer sees
	// a refusing endpoint rather than a vanished one.
	Partition bool

	// KillAfterBytes drops a connection once this many bytes have been delivered
	// across it (both directions summed); KillAfterTime drops it this long after
	// it was accepted. 0 disables each. These model a source that dies
	// mid-transfer, which is the interesting case for swarm failover.
	KillAfterBytes int64
	KillAfterTime  time.Duration
}

// Stats is a snapshot of what a Proxy has done so far.
type Stats struct {
	Accepted  int64 // connections accepted
	Refused   int64 // connections refused because the link was partitioned
	Killed    int64 // connections cut by Partition or a KillAfter trigger
	Active    int64 // connections currently open
	BytesUp   int64 // bytes delivered client→target
	BytesDown int64 // bytes delivered target→client
}

// ── Proxy ────────────────────────────────────────────────────────────────────

// Options tunes how a Proxy binds. The zero value is the safe default: listen on
// an ephemeral loopback port and refuse a non-loopback target.
type Options struct {
	// Listen overrides the bind address (default "127.0.0.1:0").
	Listen string
	// AllowRemote permits a non-loopback listen address or target. This turns the
	// proxy into an open relay reachable from (and pointing at) the network, so
	// it is opt-in, never a default. See docs/plans/mesh-testing.md §Security.
	AllowRemote bool
}

// Proxy relays one TCP endpoint through a mutable Fault.
type Proxy struct {
	target string
	ln     net.Listener

	mu       sync.Mutex
	fault    Fault
	sessions map[*session]struct{}
	script   chan struct{} // closed to cancel the running script, nil if none

	stats struct {
		accepted, refused, killed, active atomic.Int64
		bytesUp, bytesDown                atomic.Int64
	}

	closeOnce sync.Once
	closed    chan struct{}
	done      sync.WaitGroup
}

// New starts a proxy in front of target on an ephemeral loopback port, applying
// f from the first connection onward. The target must be loopback; use
// NewWithOptions to override that deliberately.
func New(target string, f Fault) (*Proxy, error) {
	return NewWithOptions(target, f, Options{})
}

// NewWithOptions is New with explicit bind/safety options.
func NewWithOptions(target string, f Fault, o Options) (*Proxy, error) {
	listen := o.Listen
	if listen == "" {
		listen = "127.0.0.1:0"
	}
	if !o.AllowRemote {
		// Both ends are checked: a non-loopback bind exposes the relay, and a
		// non-loopback target makes it a pivot into whatever the host can reach.
		if err := requireLoopback("listen address", listen); err != nil {
			return nil, err
		}
		if err := requireLoopback("target", target); err != nil {
			return nil, err
		}
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, fmt.Errorf("netfault: listen %s: %w", listen, err)
	}
	p := &Proxy{
		target:   target,
		ln:       ln,
		fault:    f,
		sessions: map[*session]struct{}{},
		closed:   make(chan struct{}),
	}
	p.done.Add(1)
	go p.serve()
	return p, nil
}

// Addr is the address to point the near end at — i.e. what goes into a peering
// URI in place of the real target.
func (p *Proxy) Addr() string { return p.ln.Addr().String() }

// Fault returns the conditions currently in force.
func (p *Proxy) Fault() Fault {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fault
}

// Set replaces the link conditions. Changes apply to live connections, not just
// new ones, so a transfer already in flight feels them — which is the whole
// point of being able to degrade a link mid-scenario. Setting Partition kills
// everything currently open.
func (p *Proxy) Set(f Fault) {
	p.mu.Lock()
	p.fault = f
	var doomed []*session
	if f.Partition {
		for s := range p.sessions {
			doomed = append(doomed, s)
		}
	}
	p.mu.Unlock()
	for _, s := range doomed {
		p.stats.killed.Add(1)
		s.close()
	}
}

// Step is one entry in a Script: apply Fault at At after the script starts.
type Step struct {
	At    time.Duration
	Fault Fault
}

// Script applies a timeline of conditions in the background — "partition at 10s,
// heal at 30s" without a test having to grow its own goroutine and timer. It
// returns immediately; a second call cancels the first. Steps need not be sorted.
func (p *Proxy) Script(steps ...Step) {
	cancel := make(chan struct{})
	p.mu.Lock()
	if p.script != nil {
		close(p.script)
	}
	p.script = cancel
	p.mu.Unlock()

	sorted := append([]Step(nil), steps...)
	for i := 1; i < len(sorted); i++ { // insertion sort; timelines are tiny
		for j := i; j > 0 && sorted[j].At < sorted[j-1].At; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}

	p.done.Add(1)
	go func() {
		defer p.done.Done()
		start := time.Now()
		for _, st := range sorted {
			select {
			case <-time.After(time.Until(start.Add(st.At))):
			case <-cancel:
				return
			case <-p.closed:
				return
			}
			p.Set(st.Fault)
		}
	}()
}

// Stats snapshots the counters.
func (p *Proxy) Stats() Stats {
	return Stats{
		Accepted:  p.stats.accepted.Load(),
		Refused:   p.stats.refused.Load(),
		Killed:    p.stats.killed.Load(),
		Active:    p.stats.active.Load(),
		BytesUp:   p.stats.bytesUp.Load(),
		BytesDown: p.stats.bytesDown.Load(),
	}
}

// Close stops listening, cuts every live connection, and waits for the relay
// goroutines to finish. Idempotent.
func (p *Proxy) Close() error {
	p.closeOnce.Do(func() {
		close(p.closed)
		p.ln.Close()
		p.mu.Lock()
		if p.script != nil {
			close(p.script)
			p.script = nil
		}
		doomed := make([]*session, 0, len(p.sessions))
		for s := range p.sessions {
			doomed = append(doomed, s)
		}
		p.mu.Unlock()
		for _, s := range doomed {
			s.close()
		}
		p.done.Wait()
	})
	return nil
}

// ── Relay ────────────────────────────────────────────────────────────────────

// session is one accepted connection and its dialed counterpart.
type session struct {
	near net.Conn // the accepted side (client)
	far  net.Conn // the dialed side (target)
	once sync.Once
}

func (s *session) close() {
	s.once.Do(func() {
		s.near.Close()
		s.far.Close()
	})
}

func (p *Proxy) serve() {
	defer p.done.Done()
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return // listener closed, or unrecoverable
		}
		if p.Fault().Partition {
			p.stats.refused.Add(1)
			c.Close()
			continue
		}
		p.done.Add(1)
		go func() {
			defer p.done.Done()
			p.handle(c)
		}()
	}
}

func (p *Proxy) handle(near net.Conn) {
	far, err := net.Dial("tcp", p.target)
	if err != nil {
		near.Close()
		return
	}
	s := &session{near: near, far: far}

	p.mu.Lock()
	p.sessions[s] = struct{}{}
	p.mu.Unlock()
	p.stats.accepted.Add(1)
	p.stats.active.Add(1)

	defer func() {
		s.close()
		p.stats.active.Add(-1)
		p.mu.Lock()
		delete(p.sessions, s)
		p.mu.Unlock()
	}()

	// Byte and time triggers are read once per session: a mid-session change to
	// KillAfter* applies to connections opened afterwards, which keeps a live
	// transfer's fate decided by the conditions it started under.
	f := p.Fault()
	var delivered atomic.Int64
	if f.KillAfterTime > 0 {
		timer := time.AfterFunc(f.KillAfterTime, func() {
			p.stats.killed.Add(1)
			s.close()
		})
		defer timer.Stop()
	}
	killAt := f.KillAfterBytes

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); p.pump(s, near, far, true, &delivered, killAt) }()
	go func() { defer wg.Done(); p.pump(s, far, near, false, &delivered, killAt) }()
	wg.Wait()
}

// parcel is a chunk of bytes with the time it becomes deliverable.
type parcel struct {
	b  []byte
	at time.Time
}

// queueDepth bounds how many parcels may be in flight per direction. It exists
// so a fast source cannot buffer without limit behind a slow link; the backlog
// is what lets latency stay a delay instead of becoming a throttle.
const queueDepth = 128

// readBuf is the per-read chunk size — the granularity at which latency and
// jitter are applied.
const readBuf = 32 << 10

// pump copies src→dst applying the direction's conditions.
//
// Latency uses a delay QUEUE rather than a sleep before each write. Sleeping
// inline would couple delay to throughput (a 300 ms link would deliver one
// buffer per 300 ms ≈ 100 KiB/s), whereas a real link delays delivery while
// still carrying a full window in flight. Delivery times are also forced
// monotonic: this is a byte stream, so reordering it would corrupt the
// connection rather than model a jittery path — jitter varies the delay, never
// the order.
func (p *Proxy) pump(s *session, src, dst net.Conn, up bool, delivered *atomic.Int64, killAt int64) {
	q := make(chan parcel, queueDepth)
	written := make(chan struct{})

	go func() {
		defer close(written)
		var rl *rateLimiter
		var rlRate int64 = -1
		for pc := range q {
			if !p.sleepUntil(pc.at) {
				return
			}
			d := p.dir(up)
			if d.Bandwidth != rlRate {
				// Rebuilt on change so live re-tuning takes effect; the lost
				// token accumulation is immaterial next to the new rate.
				rl, rlRate = newRateLimiter(d.Bandwidth), d.Bandwidth
			}
			if err := p.write(dst, pc.b, d, rl); err != nil {
				s.close()
				return
			}
			n := int64(len(pc.b))
			if up {
				p.stats.bytesUp.Add(n)
			} else {
				p.stats.bytesDown.Add(n)
			}
			if killAt > 0 && delivered.Add(n) >= killAt {
				p.stats.killed.Add(1)
				s.close()
				return
			}
		}
	}()

	buf := make([]byte, readBuf)
	var last time.Time
	for {
		n, err := src.Read(buf)
		if n > 0 {
			d := p.dir(up)
			at := time.Now().Add(delayOf(d))
			if at.Before(last) {
				at = last // preserve stream order under jitter
			}
			last = at
			b := make([]byte, n)
			copy(b, buf[:n])
			select {
			case q <- parcel{b: b, at: at}:
			case <-p.closed:
				close(q)
				<-written
				return
			}
		}
		if err != nil {
			break
		}
	}
	close(q)
	<-written
	// Half-close so the far side sees a clean EOF instead of waiting out the
	// whole session; a reset here would look like a fault we did not inject.
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
}

// write applies bandwidth and slicing to one parcel.
func (p *Proxy) write(dst net.Conn, b []byte, d Dir, rl *rateLimiter) error {
	if d.Slice <= 0 {
		if err := rl.wait(p.closed, len(b)); err != nil {
			return err
		}
		_, err := dst.Write(b)
		return err
	}
	for len(b) > 0 {
		n := min(d.Slice, len(b))
		if err := rl.wait(p.closed, n); err != nil {
			return err
		}
		if _, err := dst.Write(b[:n]); err != nil {
			return err
		}
		b = b[n:]
		if d.SliceDelay > 0 && len(b) > 0 && !p.sleepFor(d.SliceDelay) {
			return errClosed
		}
	}
	return nil
}

func (p *Proxy) dir(up bool) Dir {
	f := p.Fault()
	if up {
		return f.Up
	}
	return f.Down
}

// delayOf draws this parcel's delay: Latency ± Jitter, never negative.
func delayOf(d Dir) time.Duration {
	if d.Jitter <= 0 {
		return max(d.Latency, 0)
	}
	return max(d.Latency+rand.N(2*d.Jitter)-d.Jitter, 0)
}

// sleepUntil waits for a delivery deadline, reporting false if the proxy closed.
func (p *Proxy) sleepUntil(at time.Time) bool { return p.sleepFor(time.Until(at)) }

func (p *Proxy) sleepFor(d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-p.closed:
		return false
	}
}

// ── Helpers ──────────────────────────────────────────────────────────────────

var errClosed = fmt.Errorf("netfault: proxy closed")

// rateLimiter is a byte-rate token bucket, the same math as the seeding cap in
// federation/swarm.go — opposite side of the wire.
//
// It differs in one deliberate way: burst is a tenth of a second's traffic,
// where the seeding cap allows a full second. That cap is a fairness limit, so
// letting a peer burst is harmless; this one emulates a *link*, and a link does
// not save up a second of unused capacity to spend at once. A wide burst would
// also make short transfers appear unthrottled, which is exactly the regime the
// swarm tests care about. Nil is unlimited.
type rateLimiter struct {
	mu     sync.Mutex
	rate   float64 // bytes/sec
	burst  float64
	tokens float64
	last   time.Time
}

func newRateLimiter(bytesPerSec int64) *rateLimiter {
	if bytesPerSec <= 0 {
		return nil
	}
	r := float64(bytesPerSec)
	burst := r / 10
	return &rateLimiter{rate: r, burst: burst, tokens: burst, last: time.Now()}
}

// wait blocks until n bytes' worth of tokens are available, or stop closes.
func (rl *rateLimiter) wait(stop <-chan struct{}, n int) error {
	if rl == nil {
		return nil
	}
	rl.mu.Lock()
	now := time.Now()
	rl.tokens += rl.rate * now.Sub(rl.last).Seconds()
	if rl.tokens > rl.burst {
		rl.tokens = rl.burst
	}
	rl.last = now
	rl.tokens -= float64(n)
	var sleep time.Duration
	if rl.tokens < 0 {
		sleep = time.Duration(-rl.tokens / rl.rate * float64(time.Second))
	}
	rl.mu.Unlock()
	if sleep <= 0 {
		return nil
	}
	t := time.NewTimer(sleep)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-stop:
		return errClosed
	}
}

// requireLoopback refuses an address that is not unambiguously loopback. A
// hostname must resolve to loopback addresses only — "localhost" qualifies, a
// name that also resolves to a routable address does not.
func requireLoopback(what, addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("netfault: %s %q: %w", what, addr, err)
	}
	if host == "" {
		return fmt.Errorf("netfault: %s %q binds every interface; "+
			"set Options.AllowRemote if that is truly intended", what, addr)
	}
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsLoopback() {
			return fmt.Errorf("netfault: %s %q is not loopback; "+
				"set Options.AllowRemote if that is truly intended", what, addr)
		}
		return nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("netfault: %s %q: %w", what, addr, err)
	}
	for _, ip := range ips {
		if !ip.IsLoopback() {
			return fmt.Errorf("netfault: %s %q resolves to non-loopback %s; "+
				"set Options.AllowRemote if that is truly intended", what, addr, ip)
		}
	}
	return nil
}
