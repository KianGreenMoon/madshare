package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"daemonlord.ygg/madshare/federation"
)

// Federation F5 — sharing scope (docs/architecture/federation-access.md §Sharing
// scope). Every mesh request that reveals or delivers library content is
// answered *for an audience*, and catalog and bytes read the same rule, so the
// node never advertises what it would not serve.
//
// Two predicates make up that rule:
//
//   - share depth — how far along the friendship chain a recording travels
//     (recordings.share_depth, NULL = inherit the node default). Visible to an
//     audience iff depth >= the audience's distance, which makes DepthPrivate
//     (-1) invisible to everyone and DepthUnlimited visible to any reach we ever
//     grow into.
//   - guest-only — the per-friend half, a plain flag on the peer row
//     (federation.Peer.GuestOnly, set by the admin): a demoted friend sees
//     exactly what an anonymous local visitor sees (the guest-playable /
//     license policy), and so does an outsider on a node that opted to answer
//     guests at all (F7). The audience derives from the row in
//     federation.serveAudienceKey — no resolver lives here any more.

// shareDepthClause is the depth predicate over the recording aliased `r`. It
// binds two parameters in order — the node's default depth (for recordings that
// carry none) and the audience's distance — which scopeArgs supplies.
const shareDepthClause = `COALESCE(r.share_depth, ?) >= ?`

// audienceClause is the full scope predicate for the recording aliased `r`:
// depth, plus the guest-playable / license policy when the audience is limited
// to guest-accessible content. Pair it with scopeArgs for the bind values.
//
// An audience that is served nothing (the zero value — an outsider, or a
// resolution that failed) yields a constant-false predicate and binds no
// parameters. This is the SQL half of [federation.Class]'s fail-closed zero
// value. When Distance was a stored field a bare Audience{} read as distance 0
// — a *direct friend*, the widest audience in the system — so this guard was
// the only thing standing; the derived Distance() now answers beyond every
// legal depth for an outsider, but the constant-false clause stays as the
// first layer, because it also skips the depth arithmetic entirely.
func audienceClause(aud federation.Audience) string {
	if !aud.Serves() {
		return `0`
	}
	clause := shareDepthClause
	if aud.GuestOnly {
		clause += ` AND ` + accessClause
	}
	return clause
}

// scopeArgs returns the bind values audienceClause expects, in order — none for
// an audience the clause refuses outright. The two are always used together and
// must stay in step; that is why the empty case lives here rather than at the
// call sites.
func scopeArgs(defaultDepth int, aud federation.Audience) []any {
	if !aud.Serves() {
		return nil
	}
	return []any{defaultDepth, aud.Distance()}
}

// nodeDefaultDepth reads the node-level sharing scope every recording without an
// explicit share_depth inherits. Resolved in Go rather than as a SQL subquery on
// purpose: SQLite's CAST of a malformed settings value yields 0, which reads as
// "friends only" — a corrupt row must not silently narrow the node's sharing.
func (db *DB) nodeDefaultDepth(ctx context.Context) (int, error) {
	p, err := db.GetMadnetworkPolicy(ctx)
	if err != nil {
		return federation.DepthUnlimited, fmt.Errorf("node default share depth: %w", err)
	}
	return p.DefaultShareDepth, nil
}

// BlobVisibleTo reports whether the blob with the given content hash may be
// served to aud — the mesh-side counterpart of BlobPubliclyVisible, which stays
// the audience-free gate for the local /files/* server. The blob must be a
// surviving rendition of a recording with an approved, non-trashed appearance
// (the published predicate) *and* pass the audience's scope. found is false (no
// error) on an unknown hash.
func (db *DB) BlobVisibleTo(ctx context.Context, hash string, aud federation.Audience) (visible, found bool, err error) {
	defaultDepth, err := db.nodeDefaultDepth(ctx)
	if err != nil {
		return false, false, err
	}
	args := append([]any{}, scopeArgs(defaultDepth, aud)...)
	args = append(args, hash)
	var v bool
	err = db.QueryRowContext(ctx, `
		SELECT f.deleted_at IS NULL
		   AND EXISTS (SELECT 1 FROM tagsets t WHERE t.recording_id = f.recording_id
		                 AND t.deleted_at IS NULL AND t.review_state = 'approved')
		   AND (`+audienceClause(aud)+`)
		FROM files f
		JOIN recordings r ON r.id = f.recording_id
		WHERE f.hash = ?`, args...).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("blob visible to audience: %w", err)
	}
	return v, true, nil
}

// ShareDepthUpdate expresses the three states a share-depth edit can be in,
// which a plain *int cannot: leave it alone, clear it back to the node default,
// or pin it to a depth. Clearing and "no opinion" are genuinely different edits
// — one un-does an override, the other must not touch it — and the API's JSON
// spells them absent / null / number.
type ShareDepthUpdate struct {
	Set     bool // false = leave the column unchanged
	Inherit bool // true (with Set) = NULL, i.e. inherit the node default
	Depth   int  // the pinned depth when Set && !Inherit
}

// column renders the update as a value for recordings.share_depth.
func (u ShareDepthUpdate) column() sql.NullInt64 {
	if u.Inherit {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(u.Depth), Valid: true}
}

// Valid reports whether the update is well formed (a pinned depth must be in
// range).
func (u ShareDepthUpdate) Valid() bool {
	return !u.Set || u.Inherit || federation.ValidDepth(u.Depth)
}

// BulkSetShareDepthByTagsets sets the share depth of the recordings behind the
// given appearances — the bulk arm of the Recordings / All Appearances lenses,
// which select tagsets. Shares the guarded chunked setter the license and
// guest-playable bulk edits use, so it inherits the same live-approved-only rule
// and the same matched-appearance count.
func (db *DB) BulkSetShareDepthByTagsets(ctx context.Context, tagsetIDs []int64, depth ShareDepthUpdate) (int, error) {
	if !depth.Set {
		return 0, nil
	}
	if !depth.Valid() {
		return 0, errors.New("invalid share depth")
	}
	return db.bulkSetRecordingColumnByTagsets(ctx, tagsetIDs,
		`UPDATE recordings SET share_depth = ? WHERE id IN `, depth.column())
}
