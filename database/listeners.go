package database

import (
	"context"
	"fmt"
	"strings"
	"time"

	"daemonlord.ygg/madshare/federation"
)

// Listener devices (migration 045, docs/architecture/federation.md
// §"The household", "Being found") — this server's side of the household.
//
// household.go is the other side, and the two are easy to confuse: that one is
// what a DEVICE records about the servers it signs in to, this one is what a
// SERVER records about the devices signed in to it. Both exist because a
// listener node cannot be reached by any walk, so each end has to be told about
// the other explicitly.
//
// What a server does with these rows is narrow on purpose. They make a device
// findable BY THIS SERVER and by the other devices this server vouches for —
// nothing here ever reaches the mesh catalog or GET /madnetwork/v0/holdings,
// which read federation_holdings.

// PutListenerHoldings records what one device holds, replacing whatever it said
// last time.
//
// Wholesale replacement, like ReplaceSourceHoldings, because a push is a
// complete statement about what is in a cache right now — a delta would need
// both ends to agree about a history neither keeps. In one transaction for the
// same reason: a device whose rows were half-replaced would be advertised as
// holding a mixture of two moments.
//
// An empty hash list is meaningful and not a no-op: it is a device saying its
// cache is empty, which must stop it being offered as a holder.
func (db *DB) PutListenerHoldings(ctx context.Context, deviceKey string, userID int64, name string, hashes []string, at int64) error {
	key, err := federation.NormalizeKey(deviceKey)
	if err != nil {
		return fmt.Errorf("listener holdings: %w", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("listener holdings: begin: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO federation_listener_devices (public_key, user_id, name, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(public_key) DO UPDATE SET
			user_id    = excluded.user_id,
			name       = excluded.name,
			updated_at = excluded.updated_at`,
		key, userID, strings.TrimSpace(name), at); err != nil {
		return fmt.Errorf("upsert federation_listener_devices: %w", err)
	}
	// The user_id is overwritten along with the rest: a device handed to somebody
	// else in the same household is one row changing owner, and the alternative —
	// refusing — would leave it advertised under an account that no longer has it.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM federation_listener_holdings WHERE device_key = ?`, key); err != nil {
		return fmt.Errorf("clear federation_listener_holdings: %w", err)
	}
	for _, h := range hashes {
		hash := strings.ToLower(strings.TrimSpace(h))
		if !hashDirPattern.MatchString(hash) {
			// Skipped rather than fatal: one malformed entry in a list of
			// thousands should not cost a device its whole advertisement.
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO federation_listener_holdings (device_key, hash) VALUES (?, ?)`,
			key, hash); err != nil {
			return fmt.Errorf("insert federation_listener_holdings: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("listener holdings: commit: %w", err)
	}
	return nil
}

// ListenerBlobProviders returns the devices of this server that currently
// advertise hash, freshest first.
//
// Fresh means "pushed within federation.ListenerHoldingsTTL". A device that
// stopped pushing is not deleted, it simply stops being offered — the same
// passive-liveness shape the availability windows use, and the reason there is
// no heartbeat endpoint and nothing to sweep on a timer.
//
// SourceID and PeerID are zero because a device is neither: it is not a node we
// pull a catalog from and not a peer anyone accepted. That is load-bearing
// downstream — observePeerAlive skips a zero source rather than touching a row
// that does not exist.
func (db *DB) ListenerBlobProviders(ctx context.Context, hash string) ([]*federation.BlobProvider, error) {
	return db.listenerProviders(ctx, hash, time.Now().Add(-federation.ListenerHoldingsTTL).Unix())
}

func (db *DB) listenerProviders(ctx context.Context, hash string, since int64) ([]*federation.BlobProvider, error) {
	hash = strings.ToLower(strings.TrimSpace(hash))
	if !hashDirPattern.MatchString(hash) {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT d.public_key, d.name, d.updated_at
		FROM federation_listener_holdings h
		JOIN federation_listener_devices d ON d.public_key = h.device_key
		WHERE h.hash = ? AND d.updated_at >= ?
		ORDER BY d.updated_at DESC, d.public_key`, hash, since)
	if err != nil {
		return nil, fmt.Errorf("listener providers: %w", err)
	}
	defer rows.Close()

	var out []*federation.BlobProvider
	for rows.Next() {
		var p federation.BlobProvider
		// The device's own name is a HeardName, not a Name: nobody administering
		// this server chose it, the device claimed it. BlobProvider.Display already
		// ranks the two that way.
		if err := rows.Scan(&p.PublicKey, &p.HeardName, &p.LastSeen); err != nil {
			return nil, fmt.Errorf("scan listener provider: %w", err)
		}
		cp := p
		out = append(out, &cp)
	}
	return out, rows.Err()
}

// ForgetListenerDevice drops a device and its advertisements. Signing out, or a
// person removing a phone they no longer have.
func (db *DB) ForgetListenerDevice(ctx context.Context, deviceKey string) error {
	key, err := federation.NormalizeKey(deviceKey)
	if err != nil {
		return fmt.Errorf("forget listener device: %w", err)
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM federation_listener_devices WHERE public_key = ?`, key); err != nil {
		return fmt.Errorf("delete federation_listener_devices: %w", err)
	}
	return nil
}
