package netstack

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Arceliar/ironwood/types"
)

// fakeReader feeds runInboundReader a scripted sequence of reads. Each step is
// either a payload (returned as data) or an error. Once the script is exhausted
// it reports types.ErrClosed so the loop under test always terminates.
type fakeReader struct {
	steps []readStep
	idx   int
}

type readStep struct {
	data []byte
	err  error
}

func (f *fakeReader) Read(p []byte) (int, error) {
	if f.idx >= len(f.steps) {
		return 0, types.ErrClosed
	}
	s := f.steps[f.idx]
	f.idx++
	if s.err != nil {
		return 0, s.err
	}
	return copy(p, s.data), nil
}

// waitDone runs f in a goroutine and fails the test if it does not return
// promptly. A regression that hot-spins or hangs the reader shows up as this
// timeout rather than wedging the whole suite.
func waitDone(t *testing.T, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runInboundReader did not return within 5s (loop stuck?)")
	}
}

// A transient (non-terminal, non-closing) read error must NOT kill the loop:
// the reader keeps going and still delivers packets that arrive afterwards.
// This is the core of issue #398 — one hiccup used to break inbound forever.
func TestRunInboundReader_TransientErrorRecovers(t *testing.T) {
	r := &fakeReader{steps: []readStep{
		{data: []byte("first")},
		{err: errors.New("transient boom")},
		{data: []byte("second")},
		{err: types.ErrClosed},
	}}
	var got [][]byte
	deliver := func(b []byte) { got = append(got, append([]byte(nil), b...)) }

	waitDone(t, func() {
		runInboundReader(r, make([]byte, 1500), func() bool { return false }, deliver)
	})

	if len(got) != 2 {
		t.Fatalf("delivered %d packets, want 2: %q", len(got), got)
	}
	if string(got[0]) != "first" || string(got[1]) != "second" {
		t.Fatalf("delivered = %q, %q; want first, second", got[0], got[1])
	}
}

// types.ErrClosed is the mesh's definitive shutdown signal (core.Stop closes the
// PacketConn); the reader must exit cleanly on it even when our own close flag
// was never set — the actual shutdown path (federation/node.go calls
// core.Stop(), not YggdrasilNIC.Close()).
func TestRunInboundReader_ErrClosedExitsClean(t *testing.T) {
	r := &fakeReader{steps: []readStep{
		{data: []byte("one")},
		{err: types.ErrClosed},
	}}
	var got [][]byte
	deliver := func(b []byte) { got = append(got, append([]byte(nil), b...)) }

	waitDone(t, func() {
		runInboundReader(r, make([]byte, 1500), func() bool { return false }, deliver)
	})

	if len(got) != 1 || string(got[0]) != "one" {
		t.Fatalf("delivered = %q; want [one]", got)
	}
}

// When Close() has fired (closing() true), even a generic/transient error must
// terminate the loop cleanly rather than backoff-and-continue — the deliberate
// teardown wins over the recovery path.
func TestRunInboundReader_ClosingExitsOnAnyError(t *testing.T) {
	stop := false
	r := &fakeReader{steps: []readStep{
		{data: []byte("hello")},
		{err: errors.New("generic error")}, // reached only after closing is set
		{data: []byte("must-not-deliver")},
	}}
	var got [][]byte
	deliver := func(b []byte) {
		got = append(got, append([]byte(nil), b...))
		stop = true // request shutdown after the first delivered packet
	}

	waitDone(t, func() {
		runInboundReader(r, make([]byte, 1500), func() bool { return stop }, deliver)
	})

	if len(got) != 1 || string(got[0]) != "hello" {
		t.Fatalf("delivered = %q; want [hello] (loop should stop on the error while closing)", got)
	}
}

// The backoff must actually delay a retry (so a permanent error can't hot-spin a
// core) yet stay capped. Two consecutive transient errors should cost at least
// the first two backoff steps before the next good packet is delivered.
func TestRunInboundReader_BackoffDelaysRetry(t *testing.T) {
	r := &fakeReader{steps: []readStep{
		{err: errors.New("e1")},
		{err: errors.New("e2")},
		{data: []byte("ok")},
		{err: types.ErrClosed},
	}}
	var got [][]byte
	deliver := func(b []byte) { got = append(got, append([]byte(nil), b...)) }

	start := time.Now()
	waitDone(t, func() {
		runInboundReader(r, make([]byte, 1500), func() bool { return false }, deliver)
	})
	elapsed := time.Since(start)

	if len(got) != 1 || string(got[0]) != "ok" {
		t.Fatalf("delivered = %q; want [ok]", got)
	}
	// First two backoffs are inboundBackoffMin (50ms) then 100ms = 150ms floor.
	if min := inboundBackoffMin + 2*inboundBackoffMin; elapsed < min {
		t.Fatalf("elapsed %s < expected backoff floor %s (loop did not back off?)", elapsed, min)
	}
}

// The liveness flag InboundReaderAlive reports must be observable-true while the
// reader runs and flip to false once it returns — mirroring exactly how
// NewYggdrasilNIC wires the goroutine (set true before launch, cleared in a
// defer). This is the signal the availability watchdog reads to fail open.
func TestReaderAlive_FlipsOnExit(t *testing.T) {
	var alive atomic.Bool
	r := &fakeReader{steps: []readStep{
		{data: []byte("x")},
		{err: types.ErrClosed},
	}}
	var aliveDuringDeliver bool
	deliver := func(b []byte) { aliveDuringDeliver = alive.Load() }

	alive.Store(true)
	waitDone(t, func() {
		defer alive.Store(false)
		runInboundReader(r, make([]byte, 64), func() bool { return false }, deliver)
	})

	if !aliveDuringDeliver {
		t.Fatal("alive flag should be true while the reader is delivering")
	}
	if alive.Load() {
		t.Fatal("alive flag should be false after the reader returns")
	}
}
