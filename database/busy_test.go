package database

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestWithConnectionPragmas_AddsTxlockImmediate(t *testing.T) {
	got := withConnectionPragmas("file.db")
	for _, want := range []string{"_txlock=immediate", "_pragma=busy_timeout(5000)", "_pragma=foreign_keys(1)"} {
		if !strings.Contains(got, want) {
			t.Errorf("dsn %q missing %q", got, want)
		}
	}
	// An explicit _txlock in the dsn is left untouched (not doubled).
	if got := withConnectionPragmas("file.db?_txlock=exclusive"); strings.Count(got, "_txlock") != 1 {
		t.Errorf("expected the existing _txlock to be preserved, got %q", got)
	}
}

// TestHardDelete_ConcurrentWritesNoBusy reproduces the WAL read-then-write
// deadlock that produced "delete file: database is locked" when pruning a file
// concurrently with other writers. hardDelete does SELECT id … then DELETE; under
// a deferred transaction the DELETE upgrade fails immediately with SQLITE_BUSY if
// another connection commits a write in the gap (busy_timeout cannot wait out a
// deadlock). With _txlock=immediate the delete tx takes the write lock at BEGIN,
// so contenders wait instead. This test hammers hardDelete alongside settings
// writes on a real on-disk DB (where the pool opens >1 connection) and asserts no
// op fails.
func TestHardDelete_ConcurrentWritesNoBusy(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "busy.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	const n = 40
	hashes := make([]string, n)
	for i := range hashes {
		h := fmt.Sprintf("%064x", i+1)
		hashes[i] = h
		seedFile(t, db, h, fmt.Sprintf("f%d.mp3", i))
	}

	ctx := context.Background()
	var wg sync.WaitGroup
	errCh := make(chan error, 4*n)

	// Deleters: the read-then-write transactions under test.
	for _, h := range hashes {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			if _, _, err := db.HardDeleteFileByHash(ctx, h); err != nil {
				errCh <- fmt.Errorf("hardDelete: %w", err)
			}
		}(h)
	}
	// Writers: concurrent committed writes that race the delete upgrades.
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				if err := db.SetSetting(ctx, "k"+strconv.Itoa(w), strconv.Itoa(i)); err != nil {
					errCh <- fmt.Errorf("setSetting: %w", err)
				}
			}
		}(w)
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent op failed: %v", err)
	}
}

// TestAbsorb_ConcurrentWritesNoBusy checks the P3 absorb (a read-then-write
// single transaction: load renditions/appearances → drop tagsets → soft-remove
// files → repair) holds under contention with other writers, taking the write
// lock at BEGIN (_txlock=immediate) rather than failing the upgrade with
// SQLITE_BUSY. Each of N recordings is absorbed concurrently while four writers
// hammer the DB; no op may fail.
func TestAbsorb_ConcurrentWritesNoBusy(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "busy_absorb.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	ctx := context.Background()
	const n = 30
	type pair struct {
		rec  int64
		keep int64
		drop int64
	}
	pairs := make([]pair, n)
	for i := range pairs {
		a := insertTaggedFile(t, db, fmt.Sprintf("%064x", 2*i+1), fmt.Sprintf("keep%d.flac", i), "Band", fmt.Sprintf("Studio %d", i))
		b := insertTaggedFile(t, db, fmt.Sprintf("%064x", 2*i+2), fmt.Sprintf("drop%d.mp3", i), "Band", fmt.Sprintf("Best %d", i))
		rec := groupIntoRecording(t, db, a.ID, b.ID)
		pairs[i] = pair{rec: rec, keep: a.ID, drop: b.ID}
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 5*n)
	for _, p := range pairs {
		wg.Add(1)
		go func(p pair) {
			defer wg.Done()
			if _, err := db.AbsorbRenditions(ctx, p.rec, p.keep, []int64{p.drop}); err != nil {
				errCh <- fmt.Errorf("absorb: %w", err)
			}
		}(p)
	}
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				if err := db.SetSetting(ctx, "k"+strconv.Itoa(w), strconv.Itoa(i)); err != nil {
					errCh <- fmt.Errorf("setSetting: %w", err)
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent op failed: %v", err)
	}
}

// TestMergeRecordings_ConcurrentWritesNoBusy is the P5 counterpart: merge is
// the widest read-then-write transaction (existence checks → load appearances
// of target + sources → move/drop tagsets → move files → drop sources →
// repair), so it must take the write lock at BEGIN. N disjoint merges run
// concurrently while four settings writers hammer the DB; no op may fail.
func TestMergeRecordings_ConcurrentWritesNoBusy(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "busy_merge.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	ctx := context.Background()
	const n = 30
	type pair struct{ target, source int64 }
	pairs := make([]pair, n)
	for i := range pairs {
		a := insertTaggedFile(t, db, fmt.Sprintf("%064x", 4000+2*i+1), fmt.Sprintf("t%d.flac", i), "Band", fmt.Sprintf("Target %d", i))
		b := insertTaggedFile(t, db, fmt.Sprintf("%064x", 4000+2*i+2), fmt.Sprintf("s%d.mp3", i), "Band", fmt.Sprintf("Source %d", i))
		pairs[i] = pair{target: recordingIDOf(t, db, a.ID), source: recordingIDOf(t, db, b.ID)}
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 5*n)
	for _, p := range pairs {
		wg.Add(1)
		go func(p pair) {
			defer wg.Done()
			out, err := db.MergeRecordings(ctx, p.target, []int64{p.source})
			if err != nil {
				errCh <- fmt.Errorf("merge: %w", err)
			} else if !out.Found {
				errCh <- fmt.Errorf("merge %d<-%d reported not found", p.target, p.source)
			}
		}(p)
	}
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				if err := db.SetSetting(ctx, "mk"+strconv.Itoa(w), strconv.Itoa(i)); err != nil {
					errCh <- fmt.Errorf("setSetting: %w", err)
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent op failed: %v", err)
	}
	assertInvariants(t, db)
}

// TestBulkHardDeleteTagsets_ConcurrentWritesNoBusy is the P2 counterpart: the
// tagset-first purge (BulkHardDeleteTagsets) is a longer read-then-write
// transaction (filter trashed ids → delete → reap touched recordings),
// so it is the most exposed to the WAL upgrade deadlock. It must take the write
// lock at BEGIN (_txlock=immediate) so concurrent settings writers wait rather
// than fail. Each trashed appearance is deleted in its own bulk call while four
// writers hammer the DB; no op may fail.
func TestBulkHardDeleteTagsets_ConcurrentWritesNoBusy(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "busy_tagsets.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	ctx := context.Background()
	const n = 40
	hashes := make([]string, n)
	for i := range hashes {
		h := fmt.Sprintf("%064x", i+1)
		hashes[i] = h
		seedFile(t, db, h, fmt.Sprintf("f%d.mp3", i))
		trashAppearancesByHash(t, db, h)
	}
	tagsetIDs := trashedTagsetIDsByHash(t, db, hashes...)
	if len(tagsetIDs) != n {
		t.Fatalf("trashed tagsets = %d, want %d", len(tagsetIDs), n)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 5*n)

	for _, id := range tagsetIDs {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			if _, _, err := db.BulkHardDeleteTagsets(ctx, []int64{id}); err != nil {
				errCh <- fmt.Errorf("bulk hard delete: %w", err)
			}
		}(id)
	}
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				if err := db.SetSetting(ctx, "k"+strconv.Itoa(w), strconv.Itoa(i)); err != nil {
					errCh <- fmt.Errorf("setSetting: %w", err)
				}
			}
		}(w)
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent op failed: %v", err)
	}
}
