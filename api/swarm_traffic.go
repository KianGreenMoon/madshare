package api

import (
	"context"
	"log"
	"time"

	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/federation"
)

// Persisting the swarm's byte accounting (docs/architecture/swarm-admin.md).
//
// The counting happens in `federation`, in memory; the writing happens here.
// That split is the cache index's, for the same reason: moving bytes must not
// require a database, so nothing on a chunk-fetch path ever waits on SQLite.
//
// The flusher drains and commits — it never reads the row back, adds and writes
// it. Every write is an increment, so two flushers, a retry, or a commit racing
// a fetch can only ever be additive; there is no lost-update window to reason
// about.

// TrafficFlushInterval is how often un-persisted deltas are written. A crash
// loses at most this much accounting, which is the trade that keeps database
// writes off the transfer path. Thirty seconds is far finer than anything the
// page reports (the live view reads the in-memory session counters, which are
// exact and need no flush at all).
const TrafficFlushInterval = 30 * time.Second

// TrafficFlusher moves the running node's traffic deltas into the database on a
// timer, and once more on the way out.
type TrafficFlusher struct {
	Node     FederationNode
	Repo     database.Repository
	Interval time.Duration
	Logger   *log.Logger
}

// NewTrafficFlusher builds a flusher over a running node. Returns nil when there
// is no node — a server with federation off moves nothing, so there is nothing
// to persist, and the caller's `if f != nil` is the whole of the gate.
func NewTrafficFlusher(node FederationNode, repo database.Repository) *TrafficFlusher {
	if node == nil || repo == nil {
		return nil
	}
	return &TrafficFlusher{Node: node, Repo: repo, Interval: TrafficFlushInterval}
}

// Run flushes on a ticker until ctx ends, then flushes one last time so a clean
// shutdown persists everything the node moved. The final flush deliberately does
// NOT use ctx — it is already cancelled — and takes its own short deadline
// instead, because losing the last interval to a context that just expired would
// make every graceful shutdown a small data loss.
func (f *TrafficFlusher) Run(ctx context.Context) {
	if f == nil {
		return
	}
	interval := f.Interval
	if interval <= 0 {
		interval = TrafficFlushInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			final, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			f.Flush(final)
			return
		case <-ticker.C:
			f.Flush(ctx)
		}
	}
}

// Flush writes whatever the node has counted since the last one. Returns the
// number of hashes written, which is what the tests assert on.
func (f *TrafficFlusher) Flush(ctx context.Context) int {
	if f == nil {
		return 0
	}
	deltas := f.Node.DrainTraffic()
	if len(deltas) == 0 {
		return 0
	}
	rows := make([]database.SwarmTrafficDelta, 0, len(deltas))
	for _, d := range deltas {
		rows = append(rows, database.SwarmTrafficDelta{
			Hash: d.Hash, Up: d.Up, Down: d.Down, Wasted: d.Wasted,
		})
	}
	if err := f.Repo.AddSwarmTraffic(ctx, rows, time.Now().Unix()); err != nil {
		// The deltas are gone from the node's pending set, so this interval's
		// bytes are lost rather than retried. That is deliberate: re-queuing them
		// would need a second buffer that could itself grow without bound if the
		// database stayed unavailable, and the figure being described is a
		// cumulative total whose value does not depend on any one interval.
		f.logf("swarm traffic flush: %v", err)
		return 0
	}
	return len(rows)
}

func (f *TrafficFlusher) logf(format string, args ...any) {
	if f.Logger != nil {
		f.Logger.Printf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// sessionTraffic is the running node's in-memory accounting, or an empty
// snapshot when no node is running. The summary and live endpoints read it
// directly rather than through the flusher: it is exact, needs no interval, and
// is the half of the page that the database deliberately does not hold.
func (h *handler) sessionTraffic() federation.TrafficSnapshot {
	if h.federation == nil {
		return federation.TrafficSnapshot{Hashes: map[string]federation.TrafficCounters{}}
	}
	return h.federation.Traffic()
}
