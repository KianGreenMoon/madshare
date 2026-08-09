package database

import (
	"context"
	"strings"
	"testing"
	"time"

	"daemonlord.ygg/madshare/federation"
)

// The household's tracker (docs/architecture/federation.md §"The household",
// "Being found") — what a home server records about its own listener devices,
// and how those devices reach the swarm's provider lookup.

func deviceKey(b string) string { return strings.Repeat(b, 32) }

// newTracker returns a DB and a user id to attribute devices to.
func newTracker(t *testing.T) (*DB, int64) {
	t.Helper()
	db := openMem(t)
	id, err := db.CreateUser(context.Background(), "kian", "x", false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return db, id
}

// TestListenerHoldingsReplaceWholesale: a push is a complete statement about
// what is in a cache right now, so what it omits stops being advertised. The
// empty push is the case that matters — a swept cache must not keep offering
// what it deleted.
func TestListenerHoldingsReplaceWholesale(t *testing.T) {
	db, user := newTracker(t)
	ctx := context.Background()
	dev := deviceKey("ab")
	one, two := deviceKey("11"), deviceKey("22")
	now := time.Now().Unix()

	if err := db.PutListenerHoldings(ctx, dev, user, "kian's phone", []string{one, two}, now); err != nil {
		t.Fatalf("first push: %v", err)
	}
	if got, _ := db.ListenerBlobProviders(ctx, one); len(got) != 1 {
		t.Fatalf("holders of the first hash = %d, want 1", len(got))
	}

	// Second push drops `two` and keeps `one`.
	if err := db.PutListenerHoldings(ctx, dev, user, "kian's phone", []string{one}, now); err != nil {
		t.Fatalf("second push: %v", err)
	}
	if got, _ := db.ListenerBlobProviders(ctx, two); len(got) != 0 {
		t.Errorf("a hash left out of the push still has %d holder(s)", len(got))
	}
	if got, _ := db.ListenerBlobProviders(ctx, one); len(got) != 1 {
		t.Errorf("a hash still in the push has %d holder(s), want 1", len(got))
	}

	if err := db.PutListenerHoldings(ctx, dev, user, "kian's phone", nil, now); err != nil {
		t.Fatalf("empty push: %v", err)
	}
	if got, _ := db.ListenerBlobProviders(ctx, one); len(got) != 0 {
		t.Errorf("after an empty push the device still holds %d hash(es)", len(got))
	}
}

// TestListenerHoldingsGoStaleWithoutAPush: retention is the freshness window and
// nothing else — no heartbeat endpoint, no sweep. A phone that goes away stops
// being offered because it stops pushing.
func TestListenerHoldingsGoStaleWithoutAPush(t *testing.T) {
	db, user := newTracker(t)
	ctx := context.Background()
	dev, hash := deviceKey("ab"), deviceKey("11")

	stale := time.Now().Add(-federation.ListenerHoldingsTTL - time.Minute).Unix()
	if err := db.PutListenerHoldings(ctx, dev, user, "old phone", []string{hash}, stale); err != nil {
		t.Fatalf("push: %v", err)
	}
	if got, err := db.ListenerBlobProviders(ctx, hash); err != nil || len(got) != 0 {
		t.Errorf("a device silent past the TTL is still offered (%d holders, err %v)", len(got), err)
	}

	// It is not deleted, only unadvertised: one push brings it straight back.
	if err := db.PutListenerHoldings(ctx, dev, user, "old phone", []string{hash}, time.Now().Unix()); err != nil {
		t.Fatalf("re-push: %v", err)
	}
	if got, _ := db.ListenerBlobProviders(ctx, hash); len(got) != 1 {
		t.Errorf("a device that pushed again is offered %d times, want 1", len(got))
	}
}

// TestListenerDevicesJoinTheProviderLookup is the point of the whole table: a
// device seeding its cache is a holder like any other from the fetching side, so
// it has to come back from the same call the swarm already asks.
func TestListenerDevicesJoinTheProviderLookup(t *testing.T) {
	db, user := newTracker(t)
	ctx := context.Background()
	hash := deviceKey("11")

	// A cached-catalog source holding the same hash — an ordinary node.
	srcKey := deviceKey("cc")
	srcID := insertSource(t, db, srcKey)
	if err := db.ReplaceSourceHoldings(ctx, srcID, []string{hash}); err != nil {
		t.Fatalf("ReplaceSourceHoldings: %v", err)
	}
	// A node we are in touch with. Advertising a hash is not on its own evidence
	// that anybody can reach it, so a fetch plan drops a source nothing has
	// observed inside StaleHolderWindow — in production that clock is moved by
	// the catalog pull, a delivered transfer or the friendship ping.
	if err := db.TouchCatalogSourceSeen(ctx, srcID, time.Now().Unix(), "a node"); err != nil {
		t.Fatalf("touch source: %v", err)
	}
	// And one of this server's own devices.
	dev := deviceKey("ab")
	if err := db.PutListenerHoldings(ctx, dev, user, "kian's phone", []string{hash}, time.Now().Unix()); err != nil {
		t.Fatalf("push: %v", err)
	}

	_, holders, err := db.MadnetworkBlobProviders(ctx, hash)
	if err != nil {
		t.Fatalf("MadnetworkBlobProviders: %v", err)
	}
	seen := map[string]bool{}
	for _, p := range holders {
		seen[p.PublicKey] = true
	}
	if !seen[srcKey] || !seen[dev] {
		t.Errorf("holders = %v, want both the source and the device", seen)
	}
	if len(holders) != 2 {
		t.Errorf("holders = %d, want 2 — keyed by public key, so a device with no "+
			"source id must not fold into one entry", len(holders))
	}
}

// TestListenerDevicesFollowTheirAccount: the advertisement exists because this
// server authenticated somebody, so deleting that account withdraws it. Anything
// else leaves rows pointing at a device nobody can vouch for.
func TestListenerDevicesFollowTheirAccount(t *testing.T) {
	db, user := newTracker(t)
	ctx := context.Background()
	dev, hash := deviceKey("ab"), deviceKey("11")

	if err := db.PutListenerHoldings(ctx, dev, user, "kian's phone", []string{hash}, time.Now().Unix()); err != nil {
		t.Fatalf("push: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM users WHERE id = ?`, user); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if got, _ := db.ListenerBlobProviders(ctx, hash); len(got) != 0 {
		t.Errorf("device survived its account with %d advertisement(s)", len(got))
	}
}
