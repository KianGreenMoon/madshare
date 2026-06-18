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
