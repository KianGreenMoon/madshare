//go:build !nofederation

package federation

import "time"

// Mesh test deadlines, scaled by testTimeoutScale (1× normally, larger under
// -race). meshDeadline bounds convergence/transfer waits; meshClientTimeout
// bounds a single mesh HTTP probe. Scaling only lengthens how long a failing
// test waits — passing checks return as soon as the condition holds.
const (
	meshDeadline      = 30 * time.Second * testTimeoutScale
	meshClientTimeout = 5 * time.Second * testTimeoutScale
)

// noMemo is the TTL a test passes for a memo it wants rebuilt on every request:
// "I changed the store, now ask, and read what I wrote."
//
// It is NOT scaled, and it is deliberately not `time.Millisecond`, which is what
// these tests used to pass. That millisecond was the whole of the mesh-test
// flakiness chased from 2026-07-30 to 2026-08-01 (.issues/open-issues.md): it
// made TestCacheSeedsToTheCommunityNotOutside fail about one run in five even in
// isolation, and TestMemberIsServedTheMadnetworkScope about one full-package run
// in three. The reasoning behind it was that an intervening mesh round trip must
// surely outlast it, so the memo could not survive. It does not: two in-process
// requests over the gVisor netstack can be well under a millisecond apart, so a
// store write landing between them was read through a memo built before it — a
// freshly vouched member served as an outsider (404), and a catalog served
// without a restriction that had just been applied. Both look exactly like
// access bugs and neither was one.
//
// Zero is not available for this: a zero interval field means "keep the default"
// (Intervals.withDefaults), which is a minute. So the smallest positive duration
// is how a test says no memo at all.
const noMemo = time.Nanosecond
