package netfault

import (
	"encoding/binary"
	"net"
	"testing"
	"time"
)

// The datagram injector under the same standard as the stream one: a fault
// proxy that lies is worse than none, and these faults are the ones nothing else
// in the suite can cross-check. If the relay silently drops 20 % when asked for
// 5 %, a "5 % loss doesn't flap availability" scenario is measuring fiction.
//
// So the assertions are statistical where the fault is (loss, duplication,
// reorder are draws, not schedules) and exact where it is not (a transparent
// relay is byte-exact and in-order; a partition passes nothing). The fast cases
// run on every `go test ./...`; the wall-clock ones carry the MADSHARE_CHAOS
// skip, like their stream counterparts.

// ── Fixtures ─────────────────────────────────────────────────────────────────

// udpEcho echoes every datagram back to its sender.
func udpEcho(t *testing.T) string {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	go func() {
		buf := make([]byte, datagramBuf)
		for {
			n, from, err := c.ReadFrom(buf)
			if err != nil {
				return
			}
			c.WriteTo(buf[:n], from)
		}
	}()
	return c.LocalAddr().String()
}

// udpSink collects datagrams and reports them in arrival order.
func udpSink(t *testing.T) (addr string, got func() [][]byte) {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	recv := make(chan []byte, 1<<16)
	go func() {
		buf := make([]byte, datagramBuf)
		for {
			n, _, err := c.ReadFrom(buf)
			if err != nil {
				close(recv)
				return
			}
			b := make([]byte, n)
			copy(b, buf[:n])
			recv <- b
		}
	}()
	return c.LocalAddr().String(), func() [][]byte {
		var out [][]byte
		for {
			select {
			case b := <-recv:
				out = append(out, b)
			default:
				return out
			}
		}
	}
}

func udpProxyTo(t *testing.T, addr string, f DatagramFault) *UDPProxy {
	t.Helper()
	p, err := NewUDP(addr, f)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

// blast sends n sequence-numbered datagrams at the proxy and returns the socket
// so a caller can keep using the same source address (one flow).
//
// It paces itself, which matters more than it looks: an unpaced sender overruns
// the receive buffer of a loopback UDP socket in a few hundred datagrams, and
// the kernel's drops would then be counted as the injector's — a loss test that
// measures the machine instead of the knob. Every sender here goes through
// pace for that reason.
func blast(t *testing.T, p *UDPProxy, n int) net.Conn {
	t.Helper()
	c, err := net.Dial("udp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	buf := make([]byte, 64)
	for i := range n {
		binary.BigEndian.PutUint32(buf, uint32(i))
		if _, err := c.Write(buf); err != nil {
			t.Fatalf("send datagram %d: %v", i, err)
		}
		pace(i)
	}
	return c
}

// pace yields briefly every so often so the relay and the sink keep up.
func pace(i int) {
	if i%32 == 31 {
		time.Sleep(time.Millisecond)
	}
}

// seqs decodes the sequence numbers out of collected datagrams.
func seqs(pkts [][]byte) []uint32 {
	out := make([]uint32, 0, len(pkts))
	for _, b := range pkts {
		out = append(out, binary.BigEndian.Uint32(b))
	}
	return out
}

// settle waits for delivery to stop, so a statistical assertion is made against
// the whole population rather than whatever had landed when the test looked.
// Datagrams have no EOF, so quiescence is the only end-of-stream there is.
func settle(t *testing.T, got func() [][]byte, quiet time.Duration) [][]byte {
	t.Helper()
	var all [][]byte
	deadline := time.Now().Add(30 * time.Second)
	last := time.Now()
	for time.Since(last) < quiet {
		if time.Now().After(deadline) {
			t.Fatalf("datagrams never stopped arriving (%d so far)", len(all))
		}
		time.Sleep(10 * time.Millisecond)
		if batch := got(); len(batch) > 0 {
			all = append(all, batch...)
			last = time.Now()
		}
	}
	return all
}

// ── Transparency ─────────────────────────────────────────────────────────────

// TestDatagramTransparent is the baseline: a zero fault must deliver every
// datagram, byte-exact, in order, and at sizes well past a QUIC packet — a
// short read in the relay would truncate a datagram, which is a corruption the
// injector would then be blamed for.
func TestDatagramTransparent(t *testing.T) {
	const n = 500
	addr, got := udpSink(t)
	p := udpProxyTo(t, addr, DatagramFault{})

	c, err := net.Dial("udp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Four bytes is the floor (the sequence number lives there); the top end is
	// well past QUIC's ~1.5 KiB path-MTU probes, so a truncating read shows up.
	sizes := []int{4, 64, 1200, 1452, 8 << 10, 60 << 10}
	sent := make([][]byte, n)
	for i := range n {
		b := make([]byte, sizes[i%len(sizes)])
		binary.BigEndian.PutUint32(b, uint32(i))
		for j := 4; j < len(b); j++ {
			b[j] = byte(i*31 + j)
		}
		sent[i] = b
		if _, err := c.Write(b); err != nil {
			t.Fatalf("send datagram %d: %v", i, err)
		}
		if i%4 == 3 { // these are large; pace harder than pace() does
			time.Sleep(time.Millisecond)
		}
	}

	all := settle(t, got, 150*time.Millisecond)

	// Every datagram is checked against the one carrying its sequence number,
	// not against its position — see mostlyDelivered for why a count on this
	// side of the wire cannot be exact. Content and order are still exact: those
	// are the relay's job, and neither survives a bug quietly.
	var last int64 = -1
	for _, b := range all {
		seq := binary.BigEndian.Uint32(b)
		if int(seq) >= n {
			t.Fatalf("a datagram arrived with sequence %d, past the %d sent", seq, n)
		}
		if string(b) != string(sent[seq]) {
			t.Fatalf("datagram %d differs (len %d, want %d) — the relay is not "+
				"transparent", seq, len(b), len(sent[seq]))
		}
		if int64(seq) <= last {
			t.Fatalf("a zero fault reordered the stream: %d arrived after %d", seq, last)
		}
		last = int64(seq)
	}
	mostlyDelivered(t, len(all), n, "a zero fault")
	if s := p.Stats(); s.Flows != 1 {
		t.Errorf("Stats.Flows = %d, want 1", s.Flows)
	}
}

// mostlyDelivered is the delivery floor every sink-side population check uses.
//
// It is a floor rather than an equality because both hops here are loopback UDP,
// and neither the kernel nor a busy machine promises to carry all of it: a run
// competing with a -race build lost 26 of 400 datagrams and failed an exact
// count that had nothing to do with the relay. Anything the *relay* did is
// asserted exactly, off its own counters; what reaches the sink is asserted
// loosely enough to survive the host and tightly enough that a knob dropping a
// quarter of the traffic still fails.
func mostlyDelivered(t *testing.T, got, want int, what string) {
	t.Helper()
	if floor := want * 9 / 10; got < floor {
		t.Errorf("%s delivered %d of %d datagrams, below the %d floor", what, got, want, floor)
	}
	if got > want {
		t.Errorf("%s delivered %d datagrams for %d expected — the relay invented traffic",
			what, got, want)
	}
}

// ── The three faults TCP cannot express ──────────────────────────────────────

// TestDatagramLoss measures the drop rate against the knob. A relay that drops
// the wrong share makes every loss scenario meaningless, and the direction of
// the error matters as much as its size — too little loss and a scenario proves
// nothing, too much and it fails for reasons the code under test never caused.
func TestDatagramLoss(t *testing.T) {
	const n = 4000
	const loss = 0.25
	addr, got := udpSink(t)
	p := udpProxyTo(t, addr, DatagramFault{Up: DatagramDir{Loss: loss}})
	blast(t, p, n)

	all := settle(t, got, 150*time.Millisecond)
	s := p.Stats()

	// The rate is measured over what the relay actually saw, not over what was
	// sent: a datagram the kernel dropped before it reached the proxy is not the
	// injector's doing, and folding it in would make this test grade the host's
	// socket buffers. That the host stayed out of the way is asserted separately.
	saw := s.Lost + s.PacketsUp
	if saw < n*9/10 {
		t.Fatalf("only %d of %d datagrams reached the relay — the sender is "+
			"overrunning the socket, so the measurement below means nothing", saw, n)
	}
	rate := float64(s.Lost) / float64(saw)
	// ±5 points over ~4000 draws: sampling error at p=0.25 is well under one
	// point, so a band this wide only fails on a real mis-wiring.
	if rate < loss-0.05 || rate > loss+0.05 {
		t.Errorf("measured loss %.3f, want ≈%.2f (%d dropped of %d seen)", rate, loss, s.Lost, saw)
	}
	if int64(len(all)) > s.PacketsUp {
		t.Errorf("%d datagrams arrived but the relay only forwarded %d", len(all), s.PacketsUp)
	}
	// Loss must not corrupt or reorder what survives.
	last := int64(-1)
	for _, seq := range seqs(all) {
		if int64(seq) <= last {
			t.Fatalf("loss reordered the stream: %d after %d", seq, last)
		}
		last = int64(seq)
	}
}

// TestDatagramDuplicate: a duplicated datagram is delivered twice, verbatim.
// This is the fault the swarm's per-chunk sha256 is supposed to shrug off, so
// the injector has to actually produce it rather than merely count it.
func TestDatagramDuplicate(t *testing.T) {
	const n = 200
	addr, got := udpSink(t)
	p := udpProxyTo(t, addr, DatagramFault{Up: DatagramDir{Duplicate: 1}})
	blast(t, p, n)

	all := settle(t, got, 150*time.Millisecond)

	// The relay's side is exact: at Duplicate=1 it copies everything it saw, and
	// forwards twice as much as it took in.
	s := p.Stats()
	if s.Duplicated != s.PacketsUp/2 {
		t.Errorf("Stats: duplicated=%d of packets=%d — at Duplicate=1 exactly half "+
			"of what was forwarded should be copies", s.Duplicated, s.PacketsUp)
	}
	// The sink's side is not (see mostlyDelivered), so the claim is that copies
	// really reached the wire, and that nothing arrived three times.
	seen := map[uint32]int{}
	for _, seq := range seqs(all) {
		seen[seq]++
	}
	twice := 0
	for i := range n {
		switch seen[uint32(i)] {
		case 2:
			twice++
		case 0, 1:
		default:
			t.Fatalf("datagram %d arrived %d times, want at most 2", i, seen[uint32(i)])
		}
	}
	if floor := n * 9 / 10; twice < floor {
		t.Errorf("only %d of %d datagrams arrived twice at Duplicate=1 (floor %d)",
			twice, n, floor)
	}
	mostlyDelivered(t, len(all), 2*n, "Duplicate=1")
}

// TestDatagramReorder pins both halves: without the knob the relay preserves
// order exactly (so any reordering a scenario sees is one it asked for), and
// with it, datagrams genuinely arrive out of sequence — held back rather than
// dropped, which is why the population is still complete.
func TestDatagramReorder(t *testing.T) {
	const n = 400
	addr, got := udpSink(t)
	p := udpProxyTo(t, addr, DatagramFault{})

	blast(t, p, n)
	if out := outOfOrder(seqs(settle(t, got, 150*time.Millisecond))); out != 0 {
		t.Fatalf("a zero fault reordered %d datagrams", out)
	}

	p.Set(DatagramFault{Up: DatagramDir{Reorder: 0.2, ReorderDelay: 20 * time.Millisecond}})
	blast(t, p, n)
	all := settle(t, got, 150*time.Millisecond)
	// Reorder holds datagrams back; it must not drop them. The floor is what
	// distinguishes "delayed" from "discarded" without asserting a count the
	// kernel does not guarantee.
	mostlyDelivered(t, len(all), n, "Reorder=0.2")
	if s := p.Stats(); s.Lost != 0 {
		t.Errorf("Stats.Lost = %d with no loss configured — reorder is dropping", s.Lost)
	}
	if out := outOfOrder(seqs(all)); out == 0 {
		t.Error("Reorder=0.2 delivered a perfectly ordered stream — the knob does nothing")
	}
}

// TestDatagramFaultsApplyDownstream pins the direction wiring, which is easy to
// get backwards and expensive to debug once a mesh is on top: the faults on the
// return path must degrade only the return path. A scenario that means to slow
// a seeder and slows the requests instead measures nothing it thinks it does.
func TestDatagramFaultsApplyDownstream(t *testing.T) {
	p := udpProxyTo(t, udpEcho(t), DatagramFault{Down: DatagramDir{Loss: 1}})

	c, err := net.Dial("udp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, err := c.Read(buf); err == nil {
		t.Error("an echo crossed a return path set to 100 % loss")
	}
	s := p.Stats()
	if s.PacketsUp != 1 {
		t.Errorf("PacketsUp = %d, want 1 — Down loss swallowed the outbound half too", s.PacketsUp)
	}
	if s.PacketsDown != 0 || s.Lost != 1 {
		t.Errorf("PacketsDown=%d Lost=%d, want 0 and 1", s.PacketsDown, s.Lost)
	}
}

// outOfOrder counts datagrams that arrived after a higher-numbered one.
func outOfOrder(s []uint32) int {
	out, high := 0, uint32(0)
	for i, v := range s {
		if i > 0 && v < high {
			out++
		}
		high = max(high, v)
	}
	return out
}

// ── Partition ────────────────────────────────────────────────────────────────

// TestDatagramPartitionDropsBothWays: nothing crosses a cut path, and the flow
// survives it — the deliberate difference from the stream relay, where a
// partition must also kill the connections. Healing therefore needs no redial.
func TestDatagramPartitionDropsBothWays(t *testing.T) {
	p := udpProxyTo(t, udpEcho(t), DatagramFault{})

	c, err := net.Dial("udp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	roundTrip := func(msg string) bool {
		t.Helper()
		if _, err := c.Write([]byte(msg)); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 64)
		c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, err := c.Read(buf)
		return err == nil && string(buf[:n]) == msg
	}

	if !roundTrip("hello") {
		t.Fatal("baseline round trip failed through a zero fault")
	}
	flows := p.Stats().Flows

	p.Set(DatagramFault{Partition: true})
	if roundTrip("cut") {
		t.Error("a datagram crossed a partitioned link")
	}

	p.Set(DatagramFault{}) // heal
	if !roundTrip("back") {
		t.Error("the link did not recover after healing")
	}
	if s := p.Stats(); s.Flows != flows {
		t.Errorf("healing opened a new flow (%d → %d); a datagram partition must "+
			"leave the flow standing", flows, s.Flows)
	}
	if p.Stats().Refused == 0 {
		t.Error("Stats.Refused counted nothing during a partition")
	}
}

// ── Safety ───────────────────────────────────────────────────────────────────

// TestDatagramLoopbackOnlyByDefault — same posture as the stream relay, for the
// same reason: connectionless does not mean harmless.
func TestDatagramLoopbackOnlyByDefault(t *testing.T) {
	if _, err := NewUDP("192.0.2.1:9", DatagramFault{}); err == nil {
		t.Error("a non-loopback target was accepted by default")
	}
	if _, err := NewUDPWithOptions("127.0.0.1:9", DatagramFault{}, Options{Listen: "0.0.0.0:0"}); err == nil {
		t.Error("a wildcard bind was accepted by default")
	}
	p, err := NewUDPWithOptions("127.0.0.1:9", DatagramFault{}, Options{AllowRemote: true, Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("AllowRemote refused a loopback setup: %v", err)
	}
	p.Close()
}

// TestDatagramCloseIsIdempotent — Close runs from t.Cleanup and from teardown.
func TestDatagramCloseIsIdempotent(t *testing.T) {
	p := udpProxyTo(t, udpEcho(t), DatagramFault{})
	blast(t, p, 5)
	if err := p.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestDatagramCloseRacesNewFlows hammers the window between a datagram arriving
// and its flow being registered. Close collects the live flows under the lock
// and tears them down; a flow that registers itself *after* that collection is
// unreachable, and its readFar goroutine is parked in a socket read that only
// far.Close can end — so Close waits on it forever. The dial that opens a flow
// is long enough for this to happen, and it did: the whole package hung under
// -race until `closing` was checked at registration.
//
// One iteration reproduces it only sometimes, hence the loop; and Close runs on
// its own goroutine so a regression reports the hang instead of taking the
// package timeout down with it.
func TestDatagramCloseRacesNewFlows(t *testing.T) {
	echo := udpEcho(t)
	for i := range 100 {
		p, err := NewUDP(echo, DatagramFault{})
		if err != nil {
			t.Fatal(err)
		}
		c, err := net.Dial("udp", p.Addr())
		if err != nil {
			t.Fatal(err)
		}
		// In flight, but almost certainly not yet demultiplexed into a flow.
		c.Write([]byte("open a flow"))

		done := make(chan struct{})
		go func() { defer close(done); p.Close() }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Fatalf("Close hung on iteration %d — a flow opened after Close took "+
				"its list is never torn down, so the WaitGroup never drains", i)
		}
		c.Close()
	}
}

// ── Timing (gated) ───────────────────────────────────────────────────────────

// TestDatagramLatencyIsPerDirection measures added delay one direction at a
// time, proving the two are independent.
func TestDatagramLatencyIsPerDirection(t *testing.T) {
	requireChaos(t)
	const lat = 200 * time.Millisecond
	p := udpProxyTo(t, udpEcho(t), DatagramFault{})

	c, err := net.Dial("udp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	rtt := func() time.Duration {
		t.Helper()
		buf := make([]byte, 8)
		start := time.Now()
		if _, err := c.Write([]byte("ping")); err != nil {
			t.Fatal(err)
		}
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Read(buf); err != nil {
			t.Fatalf("no echo came back: %v", err)
		}
		return time.Since(start)
	}

	if base := rtt(); base > 50*time.Millisecond {
		t.Fatalf("baseline RTT %v is too slow to measure against", base)
	}
	for _, tc := range []struct {
		name string
		f    DatagramFault
	}{
		{"up only", DatagramFault{Up: DatagramDir{Latency: lat}}},
		{"down only", DatagramFault{Down: DatagramDir{Latency: lat}}},
	} {
		p.Set(tc.f)
		if got := rtt(); got < lat || got > lat+150*time.Millisecond {
			t.Errorf("%s: RTT %v, want ≈%v (one direction's delay)", tc.name, got, lat)
		}
	}
	p.Set(DatagramFault{Up: DatagramDir{Latency: lat}, Down: DatagramDir{Latency: lat}})
	if got := rtt(); got < 2*lat || got > 2*lat+150*time.Millisecond {
		t.Errorf("both directions: RTT %v, want ≈%v", got, 2*lat)
	}
}

// TestDatagramLatencyDoesNotThrottle is the same trap the stream relay's delay
// queue exists for, one layer down: delivery is scheduled per datagram, so a
// latent path still carries a full window in flight. Were each datagram slept
// on in turn, 2000 of them across a 100 ms path would take over three minutes
// instead of a fraction of a second — and every QUIC latency scenario would be
// measuring the injector.
func TestDatagramLatencyDoesNotThrottle(t *testing.T) {
	requireChaos(t)
	const n = 2000
	const lat = 100 * time.Millisecond
	addr, got := udpSink(t)
	p := udpProxyTo(t, addr, DatagramFault{Up: DatagramDir{Latency: lat}})

	start := time.Now()
	blast(t, p, n)
	all := settle(t, got, 150*time.Millisecond)
	elapsed := time.Since(start)

	mostlyDelivered(t, len(all), n, "a latent path")
	if budget := lat + 3*time.Second; elapsed > budget {
		t.Errorf("%d datagrams across a %v path took %v (budget %v) — latency is "+
			"serializing delivery instead of delaying it", n, lat, elapsed, budget)
	}
}

// TestDatagramBandwidthCap checks the token bucket against the clock. Overflow
// drops are expected here and are the point: a saturated datagram link drops,
// it does not buffer without limit.
func TestDatagramBandwidthCap(t *testing.T) {
	requireChaos(t)
	const rate = 512 << 10 // 512 KiB/s
	const size = 1200
	const n = 400 // ≈470 KiB, just under a second's worth

	addr, got := udpSink(t)
	p := udpProxyTo(t, addr, DatagramFault{Up: DatagramDir{Bandwidth: rate}})

	c, err := net.Dial("udp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	buf := make([]byte, size)
	for i := range n {
		binary.BigEndian.PutUint32(buf, uint32(i))
		if _, err := c.Write(buf); err != nil {
			t.Fatalf("send datagram %d: %v", i, err)
		}
		pace(i)
	}
	all := settle(t, got, 150*time.Millisecond)
	elapsed := time.Since(start) - 150*time.Millisecond // discount the quiet period

	delivered := int64(len(all)) * size
	// The bucket starts with a tenth of a second's tokens, so the floor is
	// whatever is left over that rate.
	ideal := time.Duration(float64(delivered-rate/10) / float64(rate) * float64(time.Second))
	if elapsed < time.Duration(float64(ideal)*0.75) {
		t.Errorf("%d bytes at %d B/s took %v, faster than the %v floor — the cap leaks",
			delivered, rate, elapsed, ideal)
	}
	if elapsed > time.Duration(float64(ideal)*1.6)+300*time.Millisecond {
		t.Errorf("%d bytes at %d B/s took %v, well past the %v ideal — the cap over-throttles",
			delivered, rate, elapsed, ideal)
	}
}

// TestDatagramScript walks a partition/heal timeline without the caller owning
// a timer.
func TestDatagramScript(t *testing.T) {
	requireChaos(t)
	p := udpProxyTo(t, udpEcho(t), DatagramFault{})
	p.Script(
		DatagramStep{At: 200 * time.Millisecond, Fault: DatagramFault{Partition: true}},
		DatagramStep{At: 600 * time.Millisecond, Fault: DatagramFault{}},
	)

	time.Sleep(400 * time.Millisecond)
	if !p.Fault().Partition {
		t.Error("script did not partition the link at 200ms")
	}
	time.Sleep(500 * time.Millisecond)
	if p.Fault().Partition {
		t.Error("script did not heal the link at 600ms")
	}
}
