//go:build !nofederation

package federation

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"daemonlord.ygg/madshare/tests/mesh/netfault"
)

// The 2026-08-15 tester report, run as a scenario (docs/plans rule: run it,
// don't reason about it): a sole-holder node loses its underlay link
// mid-listening, the listener keeps pressing Play, the link comes back — how
// long until a fetch succeeds again, and what does each failed attempt look
// like from the fetcher's side?
//
// The journal evidence was one line — `fetch <hash> ... mesh dial failed:
// context deadline exceeded` — and the observed lockout "~2 minutes, retrying
// earlier does not help, the track then plays fine". The suspected mechanism is
// NOT madshare state (the per-hash dedupe map holds no negative result; a
// failed transfer deletes its entry) but yggdrasil's link redial backoff, which
// grows with how long the peer stayed unreachable on its own wall clock
// (TestChaosFlappingLinkStaysFresh records the same fact from the anti-flap
// side). That backoff is real time regardless of the shrunk chaos clock, which
// is what makes this measurable here.
//
// A MEASUREMENT, not an assertion suite (same contract as depthmeasure_test.go):
// gated on MADSHARE_MEASURE, asserts only that the harness worked, exists to be
// run and read.
//
//	MADSHARE_MEASURE=1 go test -run TestMeasureDialRecoveryAfterOutage -v -timeout 30m ./federation/
type outageAttempt struct {
	at      time.Duration // since the cut (negative would be nonsense; heal-relative for recovery)
	took    time.Duration
	err     error
	dialErr bool // connect-class, i.e. the journal line the tester saw
}

func TestMeasureDialRecoveryAfterOutage(t *testing.T) {
	requireMeasure(t)
	for _, outage := range []time.Duration{10 * time.Second, 30 * time.Second, 90 * time.Second} {
		outage := outage
		t.Run(fmt.Sprintf("outage_%s", outage), func(t *testing.T) {
			measureDialRecovery(t, outage)
		})
	}
}

func measureDialRecovery(t *testing.T, outage time.Duration) {
	storeA, storeB := newMemStore(), newMemStore()
	// Two distinct blobs: track 0 warms the mesh session before the cut (the
	// tester was mid-listening, not cold), track 1 is the one that fails.
	blobs := [][]byte{fillBytes(1 << 20), fillBytes(1<<20 + 1)}
	hashes, resolver := publishBlobs(t, storeA, blobs)
	cacheB := t.TempDir()

	a, b, link := startFaultedPair(t, storeA, storeB,
		chaosOpts(resolver),
		chaosOpts(WithCacheDir(cacheB)))
	makeFriends(t, a, b, storeA, storeB)

	// B's cached catalog must advertise both hashes so EnsureBlob finds the
	// holder; the real sync fills it from storeA's published set.
	src, err := storeB.EnsureCatalogSource(context.Background(), a.PublicKeyHex(), time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "B's catalog pull to advertise both tracks", func() bool {
		return len(storeB.cachedCatalog(src.ID)) == 2
	})

	// Warm fetch: the state the tester was in when the flap hit.
	warmStart := time.Now()
	tr, err := b.EnsureBlob(context.Background(), hashes[0])
	if err != nil {
		t.Fatalf("warm EnsureBlob: %v", err)
	}
	if err := awaitTransfer(t, tr); err != nil {
		t.Fatalf("warm fetch failed on a clean link: %v\n%s", err, describe(tr.Stats()))
	}
	warmTook := time.Since(warmStart)

	// Cut. settleLastSeen gives the liveness baseline the recovery check needs.
	cutAt := time.Now()
	link.Set(partitioned)
	settled := settleLastSeen(t, storeB, a, drainQuiet)

	// The tester pressing Play during the outage: repeated fetch attempts, each
	// a fresh transfer (the dedupe map holds no failed entry — that is part of
	// what this records).
	var during []outageAttempt
	for time.Since(cutAt) < outage {
		at := time.Since(cutAt)
		start := time.Now()
		tr, err := b.EnsureBlob(context.Background(), hashes[1])
		if err == nil {
			err = awaitTransfer(t, tr)
		}
		during = append(during, outageAttempt{
			at: at, took: time.Since(start), err: err, dialErr: errors.Is(err, errMeshDial),
		})
		if err == nil {
			t.Fatalf("fetch succeeded across a partitioned link at t+%v", at)
		}
		remaining := outage - time.Since(cutAt)
		if remaining <= 0 {
			break
		}
		if pause := 1500 * time.Millisecond; pause < remaining {
			time.Sleep(pause)
		} else {
			time.Sleep(remaining)
		}
	}

	// Heal, then keep pressing Play. Recovery is over when a fetch SUCCEEDS —
	// the user-visible event — with liveness recovery recorded beside it.
	healAt := time.Now()
	link.Set(netfault.Fault{})
	var (
		recovery  []outageAttempt
		succeeded time.Duration = -1
		liveAt    time.Duration = -1
	)
	failsafe := time.Now().Add(5 * time.Minute)
	for time.Now().Before(failsafe) {
		if liveAt < 0 && lastSeenOf(t, storeB, a) > settled {
			liveAt = time.Since(healAt)
		}
		at := time.Since(healAt)
		start := time.Now()
		tr, err := b.EnsureBlob(context.Background(), hashes[1])
		if err == nil {
			err = awaitTransfer(t, tr)
		}
		recovery = append(recovery, outageAttempt{
			at: at, took: time.Since(start), err: err, dialErr: errors.Is(err, errMeshDial),
		})
		if err == nil {
			succeeded = time.Since(healAt)
			break
		}
		time.Sleep(1 * time.Second)
	}
	if liveAt < 0 && succeeded >= 0 {
		liveAt = time.Since(healAt) // the successful fetch itself is liveness
	}

	// ── Readout ──────────────────────────────────────────────────────────────
	t.Logf("outage %v: warm fetch %v; %d attempts during the outage, %d after the heal",
		outage, warmTook.Round(time.Millisecond), len(during), len(recovery))
	for _, a := range during {
		t.Logf("  cut+%6.1fs  took %6.2fs  dial-class=%v  err=%v",
			a.at.Seconds(), a.took.Seconds(), a.dialErr, a.err)
	}
	for _, a := range recovery {
		t.Logf("  heal+%5.1fs  took %6.2fs  dial-class=%v  err=%v",
			a.at.Seconds(), a.took.Seconds(), a.dialErr, a.err)
	}
	if succeeded < 0 {
		t.Fatalf("no fetch succeeded within %v of the heal — recovery exceeds the failsafe", 5*time.Minute)
	}
	t.Logf("RESULT outage=%v  first-successful-fetch=heal+%v  liveness-recovered=heal+%v",
		outage, succeeded.Round(100*time.Millisecond), liveAt.Round(100*time.Millisecond))
	// Keep CI honest if someone un-gates this by accident: the harness "worked"
	// means every during-outage attempt ended promptly and was connect-class.
	for i, a := range during {
		if !a.dialErr {
			t.Logf("note: attempt %d during the outage was not connect-class: %v", i, a.err)
		}
	}
}
