package netfault

// The datagram half of the injector, in front of a quic:// peering.
//
// It exists for the three faults a TCP relay structurally cannot express — loss,
// reordering, duplication. On a byte stream those are not injectable at all:
// dropping bytes corrupts the connection instead of modelling a lossy path, and
// the kernel's retransmits mean real loss reaches the application as stalls and
// resets rather than as gaps. One layer down, on the datagrams QUIC rides, a
// dropped packet is simply a dropped packet, and everything above it — QUIC's
// own loss recovery, yggdrasil's link, the swarm's chunk fetches — has to cope
// for real.
//
// The knob sets are therefore deliberately NOT the same on both relays, and
// neither type accepts the other's: Slice/SliceDelay (chop a write) is
// meaningless for a datagram, Loss/Duplicate/Reorder would be a lie on a stream.
// Everything else — the shape of the API, Set applying to live traffic, the
// Script timeline, loopback-only defaults — is identical, so a scenario reads
// the same whichever transport it degrades.

import (
	"fmt"
	"math/rand/v2"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// ── Fault model ──────────────────────────────────────────────────────────────

// DatagramDir holds the conditions applied to one direction of a datagram link.
// The zero value is a perfect path.
type DatagramDir struct {
	// Latency delays delivery of each datagram; Jitter spreads that delay
	// uniformly over ±Jitter, clamped at zero. Unlike the stream relay, delivery
	// order is NOT forced monotonic — a datagram path is allowed to reorder, and
	// jitter alone will do it.
	Latency time.Duration
	Jitter  time.Duration

	// Bandwidth caps this direction at bytes/second. 0 means unlimited. A
	// datagram that arrives while the transmit queue is full is dropped rather
	// than buffered, which is what a saturated link does (and is counted
	// separately, as Overflowed, so a scenario can tell induced loss from
	// congestion loss).
	Bandwidth int64

	// Loss is the probability in [0,1] that a datagram is dropped outright.
	Loss float64
	// Duplicate is the probability in [0,1] that a datagram is delivered twice.
	// The copy is scheduled with the same delay, so it follows immediately.
	Duplicate float64
	// Reorder is the probability in [0,1] that a datagram is held back by an
	// extra ReorderDelay, letting its successors overtake it. Jitter reorders
	// too, but only within its own spread; this is the deliberate, targeted
	// version, and it does nothing without ReorderDelay.
	Reorder      float64
	ReorderDelay time.Duration
}

// DatagramFault is a complete datagram-link condition. The zero value is a
// transparent relay.
type DatagramFault struct {
	// Up is client→target, Down is target→client. As with the stream relay, the
	// asymmetry is the point.
	Up   DatagramDir
	Down DatagramDir

	// Partition drops every datagram in both directions, and is the only way to
	// remove a source here. Note the difference from the stream relay, and that
	// it is not an oversight: there, a partition also kills the live
	// connections, because a TCP peer must be *told* the path is gone. On a
	// datagram link there is nothing to kill — a cut path is exactly "packets
	// stop arriving" — and letting the flows stand is both more faithful and
	// kinder to the layer above, which then recovers on heal without a redial
	// (QUIC gives up on its own after MaxIdleTimeout, one minute in yggdrasil's
	// config, if the outage outlasts it).
	Partition bool

	// There is deliberately no KillAfterBytes/KillAfterTime here. Closing a
	// flow's socket would not remove a source: the client's next datagram simply
	// opens a new flow, and to QUIC that is a NAT rebinding it migrates across
	// without dropping the connection. T2 reached the same conclusion from the
	// other side — on the stream relay, KillAfterBytes cuts the underlay session
	// and yggdrasil redials, so Partition was already the knob that means "this
	// seeder is gone". See docs/plans/mesh-testing.md §Phase T2.
}

// DatagramStats is a snapshot of what a UDPProxy has done so far. Loss counters
// are summed across both directions; scenarios degrade one direction at a time,
// which is what makes that readable.
type DatagramStats struct {
	Flows       int64 // flows opened (one per distinct client address)
	Active      int64 // flows currently open
	Refused     int64 // datagrams dropped because the link was partitioned
	Lost        int64 // datagrams dropped by the Loss knob
	Overflowed  int64 // datagrams dropped because the transmit queue was full
	Duplicated  int64 // datagrams delivered a second time
	Reordered   int64 // datagrams held back by the Reorder knob
	PacketsUp   int64 // datagrams delivered client→target
	PacketsDown int64 // datagrams delivered target→client
	BytesUp     int64
	BytesDown   int64
}

// DatagramStep is one entry in a UDPProxy Script.
type DatagramStep struct {
	At    time.Duration
	Fault DatagramFault
}

// ── Proxy ────────────────────────────────────────────────────────────────────

// UDPProxy relays one UDP endpoint through a mutable DatagramFault.
//
// Datagrams are demultiplexed by source address into flows, each with its own
// socket toward the target, so the far end sees one source per near-end peer
// exactly as it would without the relay in the middle. A flow lives until its
// far socket errors or the proxy closes; there is no idle reaper, because a
// proxy's lifetime is one scenario's and an unused flow costs a goroutine pair
// and an fd.
type UDPProxy struct {
	target *net.UDPAddr
	conn   *net.UDPConn // the near-side socket every client sends to

	mu     sync.Mutex
	fault  DatagramFault
	flows  map[string]*flow
	script chan struct{} // closed to cancel the running script, nil if none

	stats struct {
		flows, active                              atomic.Int64
		refused, lost, overflowed                  atomic.Int64
		duplicated, reordered                      atomic.Int64
		packetsUp, packetsDown, bytesUp, bytesDown atomic.Int64
	}

	closeOnce sync.Once
	closed    chan struct{}
	done      sync.WaitGroup
}

// NewUDP starts a datagram relay in front of target on an ephemeral loopback
// port. The target must be loopback; use NewUDPWithOptions to override that
// deliberately.
func NewUDP(target string, f DatagramFault) (*UDPProxy, error) {
	return NewUDPWithOptions(target, f, Options{})
}

// NewUDPWithOptions is NewUDP with explicit bind/safety options. It shares
// Options — and its loopback-by-default rule — with the stream relay: an open
// datagram relay is no less of an SSRF pivot for being connectionless.
func NewUDPWithOptions(target string, f DatagramFault, o Options) (*UDPProxy, error) {
	listen := o.Listen
	if listen == "" {
		listen = "127.0.0.1:0"
	}
	if !o.AllowRemote {
		if err := requireLoopback("listen address", listen); err != nil {
			return nil, err
		}
		if err := requireLoopback("target", target); err != nil {
			return nil, err
		}
	}
	dst, err := net.ResolveUDPAddr("udp", target)
	if err != nil {
		return nil, fmt.Errorf("netfault: resolve target %s: %w", target, err)
	}
	bind, err := net.ResolveUDPAddr("udp", listen)
	if err != nil {
		return nil, fmt.Errorf("netfault: resolve listen address %s: %w", listen, err)
	}
	conn, err := net.ListenUDP("udp", bind)
	if err != nil {
		return nil, fmt.Errorf("netfault: listen %s: %w", listen, err)
	}
	widenBuffers(conn)
	p := &UDPProxy{
		target: dst,
		conn:   conn,
		fault:  f,
		flows:  map[string]*flow{},
		closed: make(chan struct{}),
	}
	p.done.Add(1)
	go p.serve()
	return p, nil
}

// Addr is the address to point the near end at — what goes into a peering URI
// in place of the real target.
func (p *UDPProxy) Addr() string { return p.conn.LocalAddr().String() }

// Fault returns the conditions currently in force.
func (p *UDPProxy) Fault() DatagramFault {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fault
}

// Set replaces the link conditions, effective on traffic already in flight.
func (p *UDPProxy) Set(f DatagramFault) {
	p.mu.Lock()
	p.fault = f
	p.mu.Unlock()
}

// Script applies a timeline of conditions in the background, exactly like the
// stream relay's: it returns immediately, a second call cancels the first, and
// steps need not be sorted.
func (p *UDPProxy) Script(steps ...DatagramStep) {
	cancel := make(chan struct{})
	p.mu.Lock()
	if p.script != nil {
		close(p.script)
	}
	p.script = cancel
	p.mu.Unlock()

	sorted := append([]DatagramStep(nil), steps...)
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
func (p *UDPProxy) Stats() DatagramStats {
	return DatagramStats{
		Flows:       p.stats.flows.Load(),
		Active:      p.stats.active.Load(),
		Refused:     p.stats.refused.Load(),
		Lost:        p.stats.lost.Load(),
		Overflowed:  p.stats.overflowed.Load(),
		Duplicated:  p.stats.duplicated.Load(),
		Reordered:   p.stats.reordered.Load(),
		PacketsUp:   p.stats.packetsUp.Load(),
		PacketsDown: p.stats.packetsDown.Load(),
		BytesUp:     p.stats.bytesUp.Load(),
		BytesDown:   p.stats.bytesDown.Load(),
	}
}

// Close stops relaying, tears down every flow, and waits for the goroutines to
// finish. Idempotent.
func (p *UDPProxy) Close() error {
	p.closeOnce.Do(func() {
		close(p.closed)
		p.conn.Close()
		p.mu.Lock()
		if p.script != nil {
			close(p.script)
			p.script = nil
		}
		doomed := make([]*flow, 0, len(p.flows))
		for _, f := range p.flows {
			doomed = append(doomed, f)
		}
		p.mu.Unlock()
		for _, f := range doomed {
			f.close()
		}
		p.done.Wait()
	})
	return nil
}

// ── Flows ────────────────────────────────────────────────────────────────────

// datagramBuf is the receive buffer size. QUIC packets are ~1.2 KiB and its
// path-MTU probes reach ~1.5 KiB, but a short read would silently truncate a
// datagram — a corruption the injector would then be blamed for — so the buffer
// is sized for anything a UDP socket can hand over.
const datagramBuf = 64 << 10

// widenBuffers asks the kernel for room the relay would otherwise not have.
//
// It matters more here than the one line suggests: the relay adds a hop, and a
// hop with a default-sized socket buffer starts dropping under a burst the two
// real endpoints would have absorbed. Those drops are indistinguishable from
// injected loss in the counters and would make a "5 % loss" scenario run at some
// other rate entirely. Best-effort by design — the request is clamped to
// net.core.rmem_max, and there is nothing useful to do if it is refused.
func widenBuffers(c *net.UDPConn) {
	const want = 4 << 20
	_ = c.SetReadBuffer(want)
	_ = c.SetWriteBuffer(want)
}

// sendQueue bounds a direction's transmit queue. Overflow is a drop, not a
// block: a link with a full queue drops, and blocking here would turn a
// bandwidth cap into unbounded buffering (the "latency must not throttle" rule,
// one layer down).
const sendQueue = 512

// flow is one near-end peer: its client address and the socket this relay uses
// to speak to the target on its behalf.
type flow struct {
	p      *UDPProxy
	key    string
	client *net.UDPAddr
	far    *net.UDPConn

	up   chan []byte // toward the target
	down chan []byte // toward the client

	done chan struct{}
	once sync.Once
}

func (f *flow) close() {
	f.once.Do(func() {
		close(f.done)
		f.far.Close()
		f.p.mu.Lock()
		if f.p.flows[f.key] == f {
			delete(f.p.flows, f.key)
		}
		f.p.mu.Unlock()
		f.p.stats.active.Add(-1)
	})
}

// serve is the near-side read loop: every client's datagrams arrive here.
func (p *UDPProxy) serve() {
	defer p.done.Done()
	buf := make([]byte, datagramBuf)
	for {
		n, from, err := p.conn.ReadFromUDP(buf)
		if err != nil {
			return // socket closed
		}
		if n == 0 {
			continue
		}
		if p.Fault().Partition {
			p.stats.refused.Add(1)
			continue
		}
		f := p.flowFor(from)
		if f == nil {
			continue
		}
		b := make([]byte, n)
		copy(b, buf[:n])
		f.inject(b, true)
	}
}

// flowFor returns the flow for a client address, opening one on first sight.
func (p *UDPProxy) flowFor(from *net.UDPAddr) *flow {
	key := from.String()
	p.mu.Lock()
	if f, ok := p.flows[key]; ok {
		p.mu.Unlock()
		return f
	}
	p.mu.Unlock()

	far, err := net.DialUDP("udp", nil, p.target)
	if err != nil {
		return nil
	}
	widenBuffers(far)
	f := &flow{
		p: p, key: key, client: from, far: far,
		up:   make(chan []byte, sendQueue),
		down: make(chan []byte, sendQueue),
		done: make(chan struct{}),
	}

	p.mu.Lock()
	if existing, ok := p.flows[key]; ok { // lost the race; keep the winner
		p.mu.Unlock()
		far.Close()
		return existing
	}
	p.flows[key] = f
	p.mu.Unlock()

	p.stats.flows.Add(1)
	p.stats.active.Add(1)

	p.done.Add(3)
	go func() { defer p.done.Done(); f.readFar() }()
	go func() { defer p.done.Done(); f.transmit(true) }()
	go func() { defer p.done.Done(); f.transmit(false) }()
	return f
}

// readFar is the far-side read loop for one flow.
func (f *flow) readFar() {
	buf := make([]byte, datagramBuf)
	for {
		n, err := f.far.Read(buf)
		if err != nil {
			f.close()
			return
		}
		if n == 0 {
			continue
		}
		if f.p.Fault().Partition {
			f.p.stats.refused.Add(1)
			continue
		}
		b := make([]byte, n)
		copy(b, buf[:n])
		f.inject(b, false)
	}
}

// inject applies the per-datagram faults and schedules delivery.
//
// Loss is decided first — a dropped datagram costs nothing further — then the
// delay is drawn, then reorder and duplication ride on top of it. Delivery is
// scheduled by timer rather than queued in arrival order, which is what lets a
// reordered datagram actually be overtaken instead of holding up everything
// behind it.
func (f *flow) inject(b []byte, up bool) {
	d := f.p.dir(up)
	if d.Loss > 0 && rand.Float64() < d.Loss {
		f.p.stats.lost.Add(1)
		return
	}
	delay := jitterDelay(d.Latency, d.Jitter)
	if d.Reorder > 0 && d.ReorderDelay > 0 && rand.Float64() < d.Reorder {
		delay += d.ReorderDelay
		f.p.stats.reordered.Add(1)
	}
	f.schedule(b, up, delay)
	if d.Duplicate > 0 && rand.Float64() < d.Duplicate {
		f.p.stats.duplicated.Add(1)
		f.schedule(b, up, delay)
	}
}

func (f *flow) schedule(b []byte, up bool, delay time.Duration) {
	if delay <= 0 {
		f.enqueue(b, up)
		return
	}
	// The timer is deliberately untracked: enqueue is a no-op once the flow or
	// the proxy is closed, so a late firing cannot write to a dead socket, and
	// Close must not have to wait out a delay queue it is throwing away.
	time.AfterFunc(delay, func() { f.enqueue(b, up) })
}

func (f *flow) enqueue(b []byte, up bool) {
	q := f.down
	if up {
		q = f.up
	}
	select {
	case <-f.done:
		return
	case <-f.p.closed:
		return
	default:
	}
	select {
	case q <- b:
	default:
		f.p.stats.overflowed.Add(1)
	}
}

// transmit drains one direction's queue, applying the bandwidth cap and writing.
// The cap lives here, after the delay stage, so it shapes the link's transmit
// rate without reordering: whatever the network did to the packets, the wire
// still carries them one at a time, in the order they reached it.
func (f *flow) transmit(up bool) {
	q := f.down
	if up {
		q = f.up
	}
	var rl *rateLimiter
	var rlRate int64 = -1
	for {
		var b []byte
		select {
		case b = <-q:
		case <-f.done:
			return
		case <-f.p.closed:
			return
		}
		d := f.p.dir(up)
		if d.Bandwidth != rlRate {
			// Rebuilt on change so live re-tuning takes effect; the lost token
			// accumulation is immaterial next to the new rate.
			rl, rlRate = newRateLimiter(d.Bandwidth), d.Bandwidth
		}
		if err := rl.wait(f.p.closed, len(b)); err != nil {
			return
		}
		var err error
		if up {
			_, err = f.far.Write(b)
		} else {
			_, err = f.p.conn.WriteToUDP(b, f.client)
		}
		if err != nil {
			f.close()
			return
		}
		n := int64(len(b))
		if up {
			f.p.stats.packetsUp.Add(1)
			f.p.stats.bytesUp.Add(n)
		} else {
			f.p.stats.packetsDown.Add(1)
			f.p.stats.bytesDown.Add(n)
		}
	}
}

func (p *UDPProxy) dir(up bool) DatagramDir {
	f := p.Fault()
	if up {
		return f.Up
	}
	return f.Down
}
