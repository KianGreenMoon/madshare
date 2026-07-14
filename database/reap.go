package database

import (
	"context"
	"fmt"
	"log"
	"time"
)

// This file implements the reaper of the GC deletion model
// (docs/architecture/gc-model.md): delete operations only unlink or mark
// single rows, and whatever they leave unreferenced is collected here.
//
// Safety invariant: the reaper only DEMOTES garbage into Trash; it never
// destroys user-visible data. The only rows it deletes outright are recording
// husks — rows with no tagsets and no files, which carry nothing but
// license/access flags. Destruction of trashed rows (and blobs) is purge's
// job, and purge only touches rows already in Trash.
//
// While the cascade-era write paths still exist (GC-model phase P2 pending),
// a healthy library gives the reaper nothing to do — any non-zero pass count
// is a bug signal and is logged.

// ReapStats reports what one reap pass collected.
type ReapStats struct {
	// QuarantinedFiles is how many live files of appearance-less recordings
	// were soft-removed into Trash › Files (pass 1).
	QuarantinedFiles int
	// TrashedTagsets is how many live appearances of file-less recordings
	// were trashed into Trash › Appearances (pass 2).
	TrashedTagsets int
	// DeletedHusks is how many empty recording rows (no tagsets, no files)
	// were removed (pass 3).
	DeletedHusks int
}

// Total is the number of rows the reap touched; 0 means the library was
// already converged.
func (s ReapStats) Total() int {
	return s.QuarantinedFiles + s.TrashedTagsets + s.DeletedHusks
}

// Reap runs the three collection passes in one transaction and returns what
// they collected. Idempotent; a converged library is a fast no-op. Non-zero
// counts are logged — with the cascades still in place they indicate a bug,
// after phase P2 they are the normal cleanup channel.
//
// Note there is no pass for files/tagsets with a NULL recording_id: both
// columns are NOT NULL (migration 024 RAISE triggers), so the state cannot
// be created.
func (db *DB) Reap(ctx context.Context) (ReapStats, error) {
	var stats ReapStats
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return stats, fmt.Errorf("reap: begin: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().Unix()

	// Pass 1: a recording with no tagset rows at all is unreachable — no
	// catalog entry can ever list it. Its blobs are quarantined (soft-removed
	// into Trash › Files), not destroyed. Recordings whose tagsets are merely
	// trashed are untouched: they are reachable through Trash.
	res, err := tx.ExecContext(ctx,
		`UPDATE files SET deleted_at = ?
		  WHERE deleted_at IS NULL
		    AND NOT EXISTS (SELECT 1 FROM tagsets t WHERE t.recording_id = files.recording_id)`,
		now)
	if err != nil {
		return stats, fmt.Errorf("reap: quarantine files: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		stats.QuarantinedFiles = int(n)
	}

	// Pass 2: a recording with no file rows at all has nothing to play and
	// nothing on disk. Its appearances are trashed (Trash › Appearances),
	// not deleted — restore re-enters the prior review state as usual.
	res, err = tx.ExecContext(ctx,
		`UPDATE tagsets SET deleted_at = ?
		  WHERE deleted_at IS NULL
		    AND NOT EXISTS (SELECT 1 FROM files f WHERE f.recording_id = tagsets.recording_id)`,
		now)
	if err != nil {
		return stats, fmt.Errorf("reap: trash tagsets: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		stats.TrashedTagsets = int(n)
	}

	// Pass 3: an empty husk — no tagset rows, no file rows — holds nothing
	// user-visible and is removed. (Purging the last trashed row of a
	// recording is what normally produces one.)
	res, err = tx.ExecContext(ctx,
		`DELETE FROM recordings
		  WHERE NOT EXISTS (SELECT 1 FROM tagsets t WHERE t.recording_id = recordings.id)
		    AND NOT EXISTS (SELECT 1 FROM files f WHERE f.recording_id = recordings.id)`)
	if err != nil {
		return stats, fmt.Errorf("reap: delete husks: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		stats.DeletedHusks = int(n)
	}

	if err := tx.Commit(); err != nil {
		return stats, fmt.Errorf("reap: commit: %w", err)
	}
	if stats.Total() > 0 {
		log.Printf("reap: quarantined %d file(s), trashed %d appearance(s), removed %d empty recording(s)",
			stats.QuarantinedFiles, stats.TrashedTagsets, stats.DeletedHusks)
	}
	return stats, nil
}

// Reaper is the single-flight background runner: write paths call Nudge after
// committing a delete/purge/move and the loop coalesces bursts into one Reap.
// Correctness never depends on the nudge — the reap also runs at startup and
// behind the prune backstop — so a missed nudge only delays collection.
type Reaper struct {
	db   *DB
	kick chan struct{}
	done chan struct{}
}

// NewReaper starts the reap loop; it stops when ctx is cancelled. Stop waits
// for a reap in flight.
func NewReaper(ctx context.Context, db *DB) *Reaper {
	r := &Reaper{
		db:   db,
		kick: make(chan struct{}, 1),
		done: make(chan struct{}),
	}
	go func() {
		defer close(r.done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-r.kick:
				if _, err := db.Reap(context.Background()); err != nil {
					log.Printf("reap: %v", err)
				}
			}
		}
	}()
	return r
}

// Nudge schedules a reap; never blocks. Nudges arriving while one is queued
// or running coalesce into the next run.
func (r *Reaper) Nudge() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// Stop waits for the loop (stopped via the constructor context) to exit.
func (r *Reaper) Stop() {
	<-r.done
}
