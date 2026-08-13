package database

// Federation F6 — contradicted identity claims (docs/architecture/federation-trust.md
// §Trust graph). A cached catalog advertises a content hash together with the
// head of its own acoustic fingerprint; when we hold the same bytes, or hold both
// halves of a grouping it asserts, the claim is checkable arithmetic rather than
// something to take on trust.
//
// The checks live here, in the layer that already owns fingerprint comparison
// (recordings.go, the same maxBitErrorRate the local resolver groups renditions
// by). Reusing that threshold is what makes a finding explainable in one
// sentence: the claim would not group with our own bytes by the very standard
// this node uses to decide that two files are the same audio.
//
// Nothing here decides anything. Detection writes rows; an admin reads them.

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"sort"
	"time"

	"daemonlord.ygg/madshare/federation"
	"daemonlord.ygg/madshare/media"
)

// localPrint is our own fingerprint for a blob we hold: the raw words (already
// truncated to the compared head) and the build that produced them.
type localPrint struct {
	words   []uint32
	head    string // base64 of the compared head, for the stored evidence
	version string
}

// CheckSourceClaims runs both contradiction checks over one source's cached
// catalog and records what they find. Returns the number of reports that are open
// (not dismissed or acted on) after the run — what a notification badge counts.
func (db *DB) CheckSourceClaims(ctx context.Context, sourceID int64) (int, error) {
	if err := db.checkHeldBlobClaims(ctx, sourceID); err != nil {
		return 0, err
	}
	if err := db.checkGroupingClaims(ctx, sourceID); err != nil {
		return 0, err
	}
	var open int
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM federation_claim_reports
		WHERE node_key = (SELECT public_key FROM federation_nodes WHERE id = ?) AND disposition = ?`, sourceID, federation.ClaimNew).Scan(&open)
	if err != nil {
		return 0, fmt.Errorf("count claim reports: %w", err)
	}
	return open, nil
}

// checkHeldBlobClaims compares every advertised fingerprint against our own copy
// of the same bytes. The join is the whole point: it only ever considers hashes
// present on both sides, so the work is proportional to the overlap rather than
// to either library.
func (db *DB) checkHeldBlobClaims(ctx context.Context, sourceID int64) error {
	rows, err := db.QueryContext(ctx, `
		SELECT r.value->>'hash',
		       r.value->'fingerprint'->>'head',
		       COALESCE(r.value->'fingerprint'->>'version', ''),
		       SUBSTR(af.fingerprint, 1, ?), COALESCE(af.algo_version, '')
		FROM federation_catalog c, json_each(c.renditions) r
		JOIN files f ON f.hash = r.value->>'hash' AND f.deleted_at IS NULL
		JOIN audio_fingerprints af ON af.file_id = f.id
		WHERE c.node_key = (SELECT public_key FROM federation_nodes WHERE id = ?)
		  AND r.value->'fingerprint'->>'head' IS NOT NULL`,
		federation.ClaimHeadWords*4, sourceID)
	if err != nil {
		return fmt.Errorf("check held-blob claims: %w", err)
	}
	defer rows.Close()

	seen := map[string]bool{} // one report per hash, however many rows advertise it
	var found []*federation.ClaimReport
	for rows.Next() {
		var hash, theirHead, theirVersion, ourVersion string
		var ourHead []byte
		if err := rows.Scan(&hash, &theirHead, &theirVersion, &ourHead, &ourVersion); err != nil {
			return fmt.Errorf("scan held-blob claim: %w", err)
		}
		if seen[hash] {
			continue
		}
		seen[hash] = true
		theirs, err := base64.StdEncoding.DecodeString(theirHead)
		if err != nil {
			continue // malformed remote input is uncheckable, not evidence
		}
		ber, words, ok := compareHeads(media.DecodeFingerprint(ourHead), media.DecodeFingerprint(theirs))
		if !ok || ber <= maxBitErrorRate {
			continue
		}
		found = append(found, &federation.ClaimReport{
			SourceID: sourceID, Kind: federation.ClaimHeldBlob, Hash: hash,
			BER: ber, Words: words,
			OurHead:    base64.StdEncoding.EncodeToString(ourHead),
			TheirHead:  theirHead,
			OurVersion: ourVersion, TheirVersion: theirVersion,
		})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("held-blob claim rows: %w", err)
	}
	return db.recordClaimReports(ctx, found)
}

// checkGroupingClaims tests the peer's own assertion that a set of renditions is
// one recording, using only fingerprints we computed ourselves. It needs no wire
// claim and no cooperation from the peer: hold two blobs it groups, and the
// assertion is either true of our bytes or not.
func (db *DB) checkGroupingClaims(ctx context.Context, sourceID int64) error {
	rows, err := db.QueryContext(ctx, `
		SELECT c.recording_key, r.value->>'hash',
		       SUBSTR(af.fingerprint, 1, ?), COALESCE(af.algo_version, '')
		FROM federation_catalog c, json_each(c.renditions) r
		JOIN files f ON f.hash = r.value->>'hash' AND f.deleted_at IS NULL
		JOIN audio_fingerprints af ON af.file_id = f.id
		WHERE c.node_key = (SELECT public_key FROM federation_nodes WHERE id = ?) AND c.recording_key <> ''
		ORDER BY c.recording_key, r.value->>'hash'`,
		federation.ClaimHeadWords*4, sourceID)
	if err != nil {
		return fmt.Errorf("check grouping claims: %w", err)
	}
	defer rows.Close()

	groups := map[string]map[string]localPrint{} // recording key -> hash -> our print
	for rows.Next() {
		var key, hash, version string
		var head []byte
		if err := rows.Scan(&key, &hash, &head, &version); err != nil {
			return fmt.Errorf("scan grouping claim: %w", err)
		}
		if groups[key] == nil {
			groups[key] = map[string]localPrint{}
		}
		groups[key][hash] = localPrint{
			words:   media.DecodeFingerprint(head),
			head:    base64.StdEncoding.EncodeToString(head),
			version: version,
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("grouping claim rows: %w", err)
	}

	var found []*federation.ClaimReport
	for _, byHash := range groups {
		if len(byHash) < 2 {
			continue
		}
		// Sorted so a pair always reports under the same (hash, other_hash) key and
		// the upsert stays idempotent across runs.
		hashes := make([]string, 0, len(byHash))
		for h := range byHash {
			hashes = append(hashes, h)
		}
		sort.Strings(hashes)
		for i := 0; i < len(hashes); i++ {
			for j := i + 1; j < len(hashes); j++ {
				a, b := byHash[hashes[i]], byHash[hashes[j]]
				ber, words, ok := compareHeads(a.words, b.words)
				if !ok || ber <= maxBitErrorRate {
					continue
				}
				found = append(found, &federation.ClaimReport{
					SourceID: sourceID, Kind: federation.ClaimGrouping,
					Hash: hashes[i], OtherHash: hashes[j],
					BER: ber, Words: words,
					OurHead: a.head, TheirHead: b.head,
					OurVersion: a.version, TheirVersion: b.version,
				})
			}
		}
	}
	return db.recordClaimReports(ctx, found)
}

// compareHeads is the one place a contradiction is measured: a start-aligned
// bit-error rate over the words both sides carry, exactly like the local resolver.
// ok is false when there is too little overlap to say anything — a claim we cannot
// check is not a claim we distrust.
func compareHeads(ours, theirs []uint32) (ber float64, words int, ok bool) {
	// minCompareWords keeps a truncated head from producing a confident number
	// out of a handful of bits. 16 words is ~4 seconds of audio and 512 bits.
	const minCompareWords = 16
	n := min(len(ours), len(theirs))
	if n < minCompareWords {
		return 0, n, false
	}
	return media.BitErrorRate(ours[:n], theirs[:n]), n, true
}

// recordClaimReports upserts findings. The (source, kind, hash, other_hash) key is
// what makes a repeating check silent: an existing row keeps its disposition and
// only its measurement and last_seen move, so an admin who dismissed something
// is not asked again every fifteen minutes.
func (db *DB) recordClaimReports(ctx context.Context, reports []*federation.ClaimReport) error {
	if len(reports) == 0 {
		return nil
	}
	now := time.Now().Unix()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("record claim reports: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO federation_claim_reports
			(node_key, kind, hash, other_hash, ber, words, our_head, their_head,
			 our_version, their_version, disposition, first_seen, last_seen)
		VALUES ((SELECT public_key FROM federation_nodes WHERE id = ?), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (node_key, kind, hash, other_hash) DO UPDATE SET
			ber = excluded.ber, words = excluded.words,
			our_head = excluded.our_head, their_head = excluded.their_head,
			our_version = excluded.our_version, their_version = excluded.their_version,
			last_seen = excluded.last_seen`)
	if err != nil {
		return fmt.Errorf("prepare claim report insert: %w", err)
	}
	defer stmt.Close()
	for _, r := range reports {
		if _, err := stmt.ExecContext(ctx, r.SourceID, r.Kind, r.Hash, r.OtherHash,
			r.BER, r.Words, r.OurHead, r.TheirHead, r.OurVersion, r.TheirVersion,
			federation.ClaimNew, now, now); err != nil {
			return fmt.Errorf("insert claim report: %w", err)
		}
	}
	return tx.Commit()
}

const claimReportColumns = `
	cr.id, s.id, cr.kind, cr.hash, cr.other_hash, cr.ber, cr.words,
	cr.our_head, cr.their_head, cr.our_version, cr.their_version,
	cr.disposition, cr.first_seen, cr.last_seen`

// ListClaimReports returns the findings an admin has not settled yet (disposition
// 'new'), newest first, with the reported node's display name and key attached —
// the card needs both, since a name never identifies a node here. The node need
// not be a peer: since F7 item 5 we cache (and therefore check) the catalogs of
// members nobody here has decided anything about, and the key is what an admin
// blocks by.
func (db *DB) ListClaimReports(ctx context.Context) ([]*federation.ClaimReport, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT `+claimReportColumns+`, `+sourceLabelExpr+`, s.public_key
		FROM federation_claim_reports cr
		JOIN federation_nodes s ON s.public_key = cr.node_key
		WHERE cr.disposition = ?
		ORDER BY cr.last_seen DESC, cr.id DESC`, federation.ClaimNew)
	if err != nil {
		return nil, fmt.Errorf("list claim reports: %w", err)
	}
	defer rows.Close()
	var out []*federation.ClaimReport
	for rows.Next() {
		var r federation.ClaimReport
		if err := rows.Scan(&r.ID, &r.SourceID, &r.Kind, &r.Hash, &r.OtherHash, &r.BER,
			&r.Words, &r.OurHead, &r.TheirHead, &r.OurVersion, &r.TheirVersion,
			&r.Disposition, &r.FirstSeen, &r.LastSeen, &r.PeerName, &r.PeerKey); err != nil {
			return nil, fmt.Errorf("scan claim report: %w", err)
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// CountOpenClaimReports is the dashboard badge: how many findings are waiting for
// a decision. This is the whole notification design, deliberately — a count
// beside the pending-peer one, and nothing that becomes mail.
func (db *DB) CountOpenClaimReports(ctx context.Context) (int64, error) {
	var n int64
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM federation_claim_reports WHERE disposition = ?`,
		federation.ClaimNew).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count open claim reports: %w", err)
	}
	return n, nil
}

// SetClaimReportDisposition records the admin's decision. Detection never
// overwrites it, so dismissing a finding settles it for good unless the row is
// re-created from scratch (a forgotten source, a re-imported card).
func (db *DB) SetClaimReportDisposition(ctx context.Context, id int64, disposition string) error {
	switch disposition {
	case federation.ClaimNew, federation.ClaimDismissed, federation.ClaimActed:
	default:
		return fmt.Errorf("unknown claim disposition %q", disposition)
	}
	res, err := db.ExecContext(ctx,
		`UPDATE federation_claim_reports SET disposition = ? WHERE id = ?`, disposition, id)
	if err != nil {
		return fmt.Errorf("set claim disposition: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
