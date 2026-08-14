//go:build !nofederation

package federation

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A local os.Rename failure on already-verified bytes must end the transfer,
// not trigger the whole-file fallback: every holder's attempt ends at the same
// rename, so retrying over the network re-downloads the entire blob once per
// holder to answer a disk error (.issues §"fetch-path dig findings"; measured
// 3.7× the blob off the wire with two holders before the fix).
//
// The injection is a real rename(2) failure, not a fault hook: a DIRECTORY at
// the destination path. Two traps, both learned reproducing this:
//   - the directory must be created AFTER the fetch starts, because ensureBlob
//     stats that path first and a directory there reads as a cache hit;
//   - the errno is EEXIST, not the EISDIR the shape suggests — never assert on
//     the error text.
//
// The measurement is the WIRE (the netfault proxies), not TransferStats:
// resetAttempt archives an abandoned attempt's mode and chunk counts but not
// its per-holder bytes, so the stats snapshot cannot show a re-download.
func TestChaosARenameFailureIsNotAnsweredOverTheMesh(t *testing.T) {
	requireChaos(t)
	content := fillBytes(2 << 20)
	storeA, storeB, storeC := newMemStore(), newMemStore(), newMemStore()
	hash, resolveA := publishBlob(t, storeA, content)
	_, resolveC := publishBlob(t, storeC, content)
	cacheB := t.TempDir()

	a, b, c, linkA, linkC := startFaultedTrio(t, storeA, storeB, storeC,
		chaosOpts(resolveA), chaosOpts(WithCacheDir(cacheB)), chaosOpts(resolveC), 0, 0)
	makeFriends(t, a, b, storeA, storeB)
	makeFriends(t, c, b, storeC, storeB)
	seedBlobCatalog(t, storeB, a, hash, int64(len(content)))
	seedBlobCatalog(t, storeB, c, hash, int64(len(content)))

	// Slow both links enough that the rename is comfortably later than the mkdir
	// below — the window is the whole transfer, this just makes it wide.
	linkA.Set(slowDown(1 << 20))
	linkC.Set(slowDown(1 << 20))

	ctx, cancel := context.WithTimeout(context.Background(), chaosDeadline)
	defer cancel()

	// Wire-level baseline: the links also carry pings and catalog syncs, so the
	// blob's cost is the delta, not the total.
	baseA, baseC := linkA.Stats().BytesDown, linkC.Stats().BytesDown

	started := time.Now()
	tr, err := b.EnsureBlob(ctx, hash)
	if err != nil {
		t.Fatalf("EnsureBlob: %v", err)
	}
	// The disk error: the finished blob cannot be renamed into place.
	final := filepath.Join(cacheB, hash)
	if err := os.Mkdir(final, 0o755); err != nil {
		t.Fatalf("inject rename failure: %v", err)
	}

	waitErr := awaitTransfer(t, tr)
	st := tr.Stats()

	got := (linkA.Stats().BytesDown - baseA) + (linkC.Stats().BytesDown - baseC)
	t.Logf("rename blocked by a directory at the destination\n"+
		"  transfer err : %v\n"+
		"  elapsed      : %v\n"+
		"  blob size    : %d bytes\n"+
		"  off the wire : %d bytes (%.1f× the blob), %d abandoned attempt(s)\n%s",
		waitErr, time.Since(started).Round(time.Millisecond), len(content), got,
		float64(got)/float64(len(content)), len(st.Prior), describe(st))

	if waitErr == nil {
		t.Fatal("the transfer reported success although the rename could not have worked")
	}
	// One pass measures 1.3–1.4×, not 1.0 — hedging duplicates a few chunks on
	// the two equally-slowed links, and the underlay adds framing. The failure
	// mode re-downloads the whole blob at least once more (measured 3.9×), so
	// 2× separates them with margin on both sides.
	if limit := int64(len(content)) * 2; got > limit {
		t.Errorf("%d bytes pulled off the mesh (%.1f× the blob) to answer a LOCAL "+
			"rename failure on already-verified bytes — the fallback re-downloaded "+
			"what was already here", got, float64(got)/float64(len(content)))
	}
}
