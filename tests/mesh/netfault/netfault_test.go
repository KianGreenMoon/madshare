package netfault

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"
)

// A fault injector that lies is worse than none: every scenario built on this
// package inherits its errors as false confidence about federation's behavior.
// So these tests measure what the proxy actually does to a stream, rather than
// checking that the knobs were stored.
//
// The fast, deterministic cases run on every `go test ./...`. The timing-
// sensitive ones (latency, bandwidth, kill-after-time) are gated behind
// MADSHARE_CHAOS: they measure wall-clock and would flake on a loaded machine.
// See docs/plans/mesh-testing.md §Gating.

func requireChaos(t *testing.T) {
	t.Helper()
	if os.Getenv("MADSHARE_CHAOS") == "" {
		t.Skip("timing-sensitive; set MADSHARE_CHAOS=1 to run")
	}
}

// ── Fixtures ─────────────────────────────────────────────────────────────────

// echoServer accepts connections and echoes bytes back until EOF.
func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().String()
}

// sourceServer writes n deterministic bytes to every connection, then half-closes.
func sourceServer(t *testing.T, n int) (addr string, want []byte) {
	t.Helper()
	want = make([]byte, n)
	for i := range want {
		want[i] = byte(i * 31 % 251)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.Write(want)
			}()
		}
	}()
	return ln.Addr().String(), want
}

// proxyTo starts a proxy in front of addr and registers its cleanup.
func proxyTo(t *testing.T, addr string, f Fault) *Proxy {
	t.Helper()
	p, err := New(addr, f)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

// drain reads until EOF, returning what arrived.
func drain(t *testing.T, c net.Conn) []byte {
	t.Helper()
	b, err := io.ReadAll(c)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("read through proxy: %v", err)
	}
	return b
}

// ── Transparency ─────────────────────────────────────────────────────────────

// TestTransparent is the baseline every other scenario rests on: with a zero
// Fault the proxy must be byte-exact in both directions, under enough volume to
// cross the read buffer and the parcel queue many times over. A proxy that
// corrupts or truncates here would make every downstream failure a mystery.
func TestTransparent(t *testing.T) {
	const size = 4 << 20
	p := proxyTo(t, echoServer(t), Fault{})

	c, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	want := make([]byte, size)
	for i := range want {
		want[i] = byte(i*7 + i/253)
	}

	// Write and read concurrently — an echo of this size deadlocks otherwise,
	// both directions' buffers filling before either side drains.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := c.Write(want); err != nil {
			t.Errorf("write: %v", err)
		}
		if cw, ok := c.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
	}()

	got := drain(t, c)
	wg.Wait()

	if len(got) != len(want) {
		t.Fatalf("echoed %d bytes, want %d", len(got), len(want))
	}
	if !bytes.Equal(got, want) {
		t.Fatal("echoed bytes differ from what was sent — the relay is not transparent")
	}
	if s := p.Stats(); s.BytesUp != size || s.BytesDown != size {
		t.Errorf("stats up=%d down=%d, want %d each", s.BytesUp, s.BytesDown, size)
	}
}

// TestSliceIsTransparent: slicing changes write granularity, never content.
func TestSliceIsTransparent(t *testing.T) {
	const size = 256 << 10
	addr, want := sourceServer(t, size)
	p := proxyTo(t, addr, Fault{Down: Dir{Slice: 1024}})

	c, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if got := drain(t, c); !bytes.Equal(got, want) {
		t.Fatalf("sliced stream corrupted: got %d bytes, want %d", len(got), len(want))
	}
}

// ── Partition ────────────────────────────────────────────────────────────────

// TestPartitionRefusesAndKills covers both halves of the knob: new connections
// are refused, existing ones are cut, and healing restores service — the proxy
// keeps listening throughout, so the peer sees a refusing endpoint rather than a
// vanished one (which is what a partition actually looks like).
func TestPartitionRefusesAndKills(t *testing.T) {
	p := proxyTo(t, echoServer(t), Fault{})

	live, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if _, err := live.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(live, buf); err != nil {
		t.Fatalf("pre-partition echo: %v", err)
	}

	p.Set(Fault{Partition: true})

	// The live connection must die without us writing anything more.
	live.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := live.Read(buf); err == nil {
		t.Error("live connection survived a partition")
	} else if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Error("live connection was neither closed nor reset by a partition")
	}

	// A new connection is accepted by the listener but immediately closed, so
	// the failure surfaces as EOF on first read rather than a dial error.
	c2, err := net.Dial("tcp", p.Addr())
	if err == nil {
		defer c2.Close()
		c2.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := c2.Read(buf); err == nil {
			t.Error("partitioned proxy served a new connection")
		}
	}

	p.Set(Fault{}) // heal

	c3, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatalf("dial after heal: %v", err)
	}
	defer c3.Close()
	if _, err := c3.Write([]byte("world")); err != nil {
		t.Fatal(err)
	}
	c3.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(c3, buf); err != nil {
		t.Fatalf("echo after heal: %v", err)
	}
	if !bytes.Equal(buf, []byte("world")) {
		t.Errorf("after heal echoed %q, want %q", buf, "world")
	}
	if s := p.Stats(); s.Killed == 0 {
		t.Error("partition killed no connections in Stats")
	}
}

// ── Kill triggers ────────────────────────────────────────────────────────────

// TestKillAfterBytes: a source that dies mid-transfer — the case swarm failover
// exists for. The stream must stop early rather than complete.
func TestKillAfterBytes(t *testing.T) {
	const size = 1 << 20
	const killAt = 128 << 10
	addr, _ := sourceServer(t, size)
	p := proxyTo(t, addr, Fault{KillAfterBytes: killAt})

	c, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(30 * time.Second))

	got := drain(t, c)
	if len(got) >= size {
		t.Fatalf("got the whole %d-byte stream; KillAfterBytes=%d did not cut it", size, killAt)
	}
	// The cut lands on a parcel boundary at or past the threshold, never before.
	if int64(len(got)) < killAt {
		t.Errorf("cut at %d bytes, before the %d-byte threshold", len(got), killAt)
	}
	if int64(len(got)) > killAt+readBuf {
		t.Errorf("cut at %d bytes, more than one read buffer past the %d-byte threshold",
			len(got), killAt)
	}
	if s := p.Stats(); s.Killed != 1 {
		t.Errorf("Stats.Killed = %d, want 1", s.Killed)
	}
}

// ── Safety ───────────────────────────────────────────────────────────────────

// TestLoopbackOnlyByDefault pins the security posture: the relay forwards
// arbitrary bytes and can be retargeted, so exposing it — or aiming it off-host —
// must take an explicit opt-in. See docs/plans/mesh-testing.md §Security posture.
func TestLoopbackOnlyByDefault(t *testing.T) {
	if _, err := New("192.0.2.1:9", Fault{}); err == nil {
		t.Error("a non-loopback target was accepted by default")
	}
	if _, err := NewWithOptions("127.0.0.1:9", Fault{}, Options{Listen: "0.0.0.0:0"}); err == nil {
		t.Error("a wildcard bind was accepted by default")
	}
	// The escape hatch still works — but only when asked for by name.
	p, err := NewWithOptions("127.0.0.1:9", Fault{}, Options{AllowRemote: true, Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("AllowRemote refused a loopback setup: %v", err)
	}
	p.Close()
}

// TestCloseIsIdempotent — Close runs from t.Cleanup and from scenario teardown.
func TestCloseIsIdempotent(t *testing.T) {
	p := proxyTo(t, echoServer(t), Fault{})
	if err := p.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// ── Timing (gated) ───────────────────────────────────────────────────────────

// TestLatencyIsPerDirection measures added delay on a ping-pong, one direction
// at a time — which also proves the two directions are independent, the property
// that makes an asymmetric link expressible.
func TestLatencyIsPerDirection(t *testing.T) {
	requireChaos(t)
	const lat = 200 * time.Millisecond
	p := proxyTo(t, echoServer(t), Fault{})

	rtt := func() time.Duration {
		c, err := net.Dial("tcp", p.Addr())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		buf := make([]byte, 1)
		start := time.Now()
		if _, err := c.Write([]byte{42}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(c, buf); err != nil {
			t.Fatal(err)
		}
		return time.Since(start)
	}

	p.Set(Fault{})
	if base := rtt(); base > 50*time.Millisecond {
		t.Fatalf("baseline RTT %v is too slow to measure against", base)
	}
	for _, tc := range []struct {
		name string
		f    Fault
	}{
		{"up only", Fault{Up: Dir{Latency: lat}}},
		{"down only", Fault{Down: Dir{Latency: lat}}},
	} {
		p.Set(tc.f)
		got := rtt()
		if got < lat || got > lat+150*time.Millisecond {
			t.Errorf("%s: RTT %v, want ≈%v (one direction's delay)", tc.name, got, lat)
		}
	}

	// Both directions delayed: the round trip pays twice.
	p.Set(Fault{Up: Dir{Latency: lat}, Down: Dir{Latency: lat}})
	if got := rtt(); got < 2*lat || got > 2*lat+150*time.Millisecond {
		t.Errorf("both directions: RTT %v, want ≈%v", got, 2*lat)
	}
}

// TestLatencyDoesNotThrottle is the subtle one, and the reason pump uses a delay
// queue instead of sleeping before each write. A naive implementation couples
// delay to throughput — a 200 ms link would deliver one buffer per 200 ms, about
// 160 KiB/s — which would silently invalidate every latency scenario built on
// it: transfers would look slow because of a bug in the injector, not because of
// anything federation did.
func TestLatencyDoesNotThrottle(t *testing.T) {
	requireChaos(t)
	const size = 4 << 20
	const lat = 200 * time.Millisecond

	addr, want := sourceServer(t, size)
	p := proxyTo(t, addr, Fault{Down: Dir{Latency: lat}})

	c, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(60 * time.Second))

	start := time.Now()
	got := drain(t, c)
	elapsed := time.Since(start)

	if !bytes.Equal(got, want) {
		t.Fatalf("stream corrupted under latency: %d bytes, want %d", len(got), len(want))
	}
	// The whole transfer should cost roughly one added delay, not one per buffer.
	// 4 MiB at 32 KiB per read is 128 buffers; the naive implementation would
	// need 128 × 200 ms ≈ 25 s.
	if budget := lat + 5*time.Second; elapsed > budget {
		t.Errorf("4 MiB under %v latency took %v (budget %v) — latency is throttling "+
			"throughput, so the delay queue is not doing its job", lat, elapsed, budget)
	}
}

// TestBandwidthCap checks the token bucket against the clock.
func TestBandwidthCap(t *testing.T) {
	requireChaos(t)
	const rate = 1 << 20 // 1 MiB/s
	const size = 3 << 20

	addr, want := sourceServer(t, size)
	p := proxyTo(t, addr, Fault{Down: Dir{Bandwidth: rate}})

	c, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(60 * time.Second))

	start := time.Now()
	got := drain(t, c)
	elapsed := time.Since(start)

	if !bytes.Equal(got, want) {
		t.Fatalf("stream corrupted under a bandwidth cap: %d bytes, want %d", len(got), len(want))
	}
	// The bucket starts with a tenth of a second's worth of tokens, so the ideal
	// time is (size - burst) / rate.
	payable := float64(size - rate/10) // bytes past the initial burst
	ideal := time.Duration(payable / float64(rate) * float64(time.Second))
	if elapsed < time.Duration(float64(ideal)*0.75) {
		t.Errorf("3 MiB at %d B/s took %v, faster than the %v floor — the cap leaks",
			rate, elapsed, ideal)
	}
	if elapsed > time.Duration(float64(ideal)*1.6) {
		t.Errorf("3 MiB at %d B/s took %v, well past the %v ideal — the cap over-throttles",
			rate, elapsed, ideal)
	}
}

// TestKillAfterTime and TestScript cover the timeline knobs meshlab drives.
func TestKillAfterTime(t *testing.T) {
	requireChaos(t)
	p := proxyTo(t, echoServer(t), Fault{KillAfterTime: 300 * time.Millisecond})

	c, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(10 * time.Second))

	start := time.Now()
	io.ReadAll(c) // returns when the proxy cuts the connection
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("connection survived %v, want a cut at ≈300ms", elapsed)
	}
	if s := p.Stats(); s.Killed != 1 {
		t.Errorf("Stats.Killed = %d, want 1", s.Killed)
	}
}

// TestScript walks a partition/heal timeline without the caller owning a timer.
func TestScript(t *testing.T) {
	requireChaos(t)
	p := proxyTo(t, echoServer(t), Fault{})
	p.Script(
		Step{At: 200 * time.Millisecond, Fault: Fault{Partition: true}},
		Step{At: 600 * time.Millisecond, Fault: Fault{}},
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
