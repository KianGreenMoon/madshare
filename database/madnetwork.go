package database

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"daemonlord.ygg/madshare/federation"
)

// Federation F2 — the catalog (docs/architecture/federation.md §Catalog): what
// this node publishes (PublishedCatalog), the per-source cached copies pulled
// from other nodes (ReplaceSourceCatalog), and the merged browse queries behind
// /api/madnetwork/*. *DB satisfies the catalog half of federation.PeerStore here.
//
// Since F7 item 5 a cached copy hangs off a *source* (federation_catalog_sources,
// madnetwork_sources.go) rather than off a friendship, because this node pulls
// from every member of its community and not only from the nodes an admin
// hand-picked. The one trust condition the browse queries still carry is the
// block: a blocked node's rows are kept but hidden, so an unblock restores the
// view without a resync.

// PublishedCatalog builds this node's own catalog *for one audience*: every
// approved, live appearance (the visibleTagset predicate — exactly what the
// local library shows) that the audience's scope admits (F5: share depth, and
// the guest-playable policy for a guest-only audience), with resolved display
// names and its recording's live renditions. Ordered by tagset id so the
// snapshot serial is deterministic.
//
// The audience is a parameter rather than a post-filter because the serial must
// hash the snapshot the peer actually receives: two audiences legitimately have
// two serials, and the node memoizes one snapshot per audience class.
func (db *DB) PublishedCatalog(ctx context.Context, aud federation.Audience) ([]federation.CatalogEntry, error) {
	defaultDepth, err := db.nodeDefaultDepth(ctx)
	if err != nil {
		return nil, err
	}
	scope := audienceClause(aud)
	rows, err := db.QueryContext(ctx, `
		SELECT m.id, m.recording_id, m.title,
		       COALESCE(par.name, m.artist, ''),
		       COALESCE(aar.name, m.album_artist, ''),
		       COALESCE(al.title, m.album, ''),
		       COALESCE(m.genre, ''), m.year, m.track_number, m.disc_number,
		       COALESCE(r.license, ''), r.guest_playable
		FROM tagsets m`+recordingJoin+`
		LEFT JOIN artists par ON par.id = m.artist_id
		LEFT JOIN artists aar ON aar.id = m.album_artist_id
		LEFT JOIN albums al   ON al.id  = m.album_id
		WHERE `+visibleTagset+`
		  AND (`+scope+`)
		ORDER BY m.id`, scopeArgs(defaultDepth, aud)...)
	if err != nil {
		return nil, fmt.Errorf("published catalog: %w", err)
	}
	defer rows.Close()

	var entries []federation.CatalogEntry
	recordings := map[string][]int{} // recording key -> entry indexes awaiting renditions
	for rows.Next() {
		var e federation.CatalogEntry
		var tagsetID, recordingID int64
		var year, track, disc sql.NullInt64
		if err := rows.Scan(&tagsetID, &recordingID, &e.Title, &e.Artist, &e.AlbumArtist,
			&e.Album, &e.Genre, &year, &track, &disc, &e.License, &e.GuestPlayable); err != nil {
			return nil, fmt.Errorf("scan catalog entry: %w", err)
		}
		e.Key = strconv.FormatInt(tagsetID, 10)
		e.RecordingKey = strconv.FormatInt(recordingID, 10)
		e.Year, e.TrackNumber, e.DiscNumber = nullInt(year), nullInt(track), nullInt(disc)
		e.Renditions = []federation.CatalogRendition{}
		recordings[e.RecordingKey] = append(recordings[e.RecordingKey], len(entries))
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("published catalog rows: %w", err)
	}
	if len(entries) == 0 {
		return []federation.CatalogEntry{}, nil
	}

	// Attach each recording's live renditions (hash = the swarm id, plus the
	// quality facts the ladder ranks by). Scoped by the same audience clause as
	// the entries above: only entries in `recordings` receive renditions anyway,
	// so this is not what makes the result correct — it is what keeps the two
	// queries obviously the same rule, and what stops an out-of-scope recording's
	// hashes from being read at all.
	//
	// The fingerprint head rides along per rendition (F6): what this node says the
	// audio *is*, in a form a friend holding the same bytes can check. substr on a
	// BLOB counts bytes, so this reads the first ClaimHeadWords packed words and
	// never the whole fingerprint — see federation.ClaimHeadWords for why.
	rrows, err := db.QueryContext(ctx, `
		SELECT f.recording_id, f.hash, f.byte_size,
		       COALESCE(mm.codec, ''), COALESCE(mm.bitrate, 0),
		       COALESCE(mm.sample_rate, 0), COALESCE(mm.bit_depth, 0),
		       COALESCE(mm.duration_seconds, 0),
		       COALESCE(af.algo, ''), COALESCE(af.algo_version, ''),
		       COALESCE(LENGTH(af.fingerprint), 0),
		       SUBSTR(af.fingerprint, 1, ?)
		FROM files f
		JOIN recordings r ON r.id = f.recording_id
		LEFT JOIN media_metadata mm ON mm.file_id = f.id
		LEFT JOIN audio_fingerprints af ON af.file_id = f.id
		WHERE f.deleted_at IS NULL AND (`+scope+`) AND EXISTS (
			SELECT 1 FROM tagsets m WHERE m.recording_id = f.recording_id AND `+visibleTagset+`)
		ORDER BY f.recording_id, f.id`,
		append([]any{federation.ClaimHeadWords * 4}, scopeArgs(defaultDepth, aud)...)...)
	if err != nil {
		return nil, fmt.Errorf("catalog renditions: %w", err)
	}
	defer rrows.Close()
	for rrows.Next() {
		var recordingID int64
		var rd federation.CatalogRendition
		var algo, algoVersion string
		var fpBytes int
		var head []byte
		if err := rrows.Scan(&recordingID, &rd.Hash, &rd.Size, &rd.Codec,
			&rd.Bitrate, &rd.SampleRate, &rd.BitDepth, &rd.Duration,
			&algo, &algoVersion, &fpBytes, &head); err != nil {
			return nil, fmt.Errorf("scan catalog rendition: %w", err)
		}
		if len(head) >= 4 {
			rd.Fingerprint = &federation.FingerprintClaim{
				Algo:    algo,
				Version: algoVersion,
				Words:   fpBytes / 4,
				Head:    base64.StdEncoding.EncodeToString(head),
			}
		}
		for _, i := range recordings[strconv.FormatInt(recordingID, 10)] {
			entries[i].Renditions = append(entries[i].Renditions, rd)
			if entries[i].Duration == 0 && rd.Duration > 0 {
				entries[i].Duration = rd.Duration
			}
		}
	}
	return entries, rrows.Err()
}

// ReplaceSourceCatalog atomically replaces the cached copy of one source's
// catalog with a fresh snapshot and records the snapshot serial + sync time.
//
// The replace is wholesale, but first_seen is NOT: the dates of the entries this
// source already offered are read before the delete and re-applied to the ones
// that survive (migration 037). Without that, every sync that changed anything
// would re-date the source's whole library, and "New on the network" would be a
// list of whoever synced most recently.
func (db *DB) ReplaceSourceCatalog(ctx context.Context, sourceID int64, serial string, syncedAt int64, entries []federation.CatalogEntry) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("replace source catalog: %w", err)
	}
	defer tx.Rollback()

	seen, err := catalogFirstSeen(ctx, tx, sourceID)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM federation_catalog WHERE source_id = ?`, sourceID); err != nil {
		return fmt.Errorf("clear source catalog: %w", err)
	}
	ins, err := tx.PrepareContext(ctx, `
		INSERT INTO federation_catalog (source_id, entry_key, recording_key, title, artist,
			album_artist, album, genre, year, track_number, disc_number, duration,
			license, guest_playable, renditions, first_seen)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare source catalog insert: %w", err)
	}
	defer ins.Close()
	for _, e := range entries {
		if e.Key == "" || e.Title == "" {
			continue // remote input — skip rows that cannot be displayed or re-keyed
		}
		renditions, err := json.Marshal(e.Renditions)
		if err != nil {
			return fmt.Errorf("marshal renditions: %w", err)
		}
		firstSeen := syncedAt
		if prior, ok := seen[e.Key]; ok && prior > 0 {
			firstSeen = prior
		}
		if _, err := ins.ExecContext(ctx, sourceID, e.Key, e.RecordingKey, e.Title, e.Artist,
			e.AlbumArtist, e.Album, e.Genre, e.Year, e.TrackNumber, e.DiscNumber,
			nullFloat(e.Duration), e.License, e.GuestPlayable, string(renditions), firstSeen); err != nil {
			return fmt.Errorf("insert source catalog entry: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE federation_catalog_sources
		   SET catalog_serial = ?, catalog_synced_at = ?, last_seen = MAX(last_seen, ?)
		 WHERE id = ?`, serial, syncedAt, syncedAt, sourceID); err != nil {
		return fmt.Errorf("update source sync state: %w", err)
	}
	return tx.Commit()
}

// catalogFirstSeen reads one source's entry_key → first_seen map inside the
// replace transaction. A source's catalog is bounded by what we chose to cache,
// so holding it in memory for the length of the replace is the same order of
// cost as the snapshot being applied.
func catalogFirstSeen(ctx context.Context, tx *sql.Tx, sourceID int64) (map[string]int64, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT entry_key, first_seen FROM federation_catalog WHERE source_id = ?`, sourceID)
	if err != nil {
		return nil, fmt.Errorf("read source catalog dates: %w", err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var key string
		var at int64
		if err := rows.Scan(&key, &at); err != nil {
			return nil, fmt.Errorf("scan source catalog date: %w", err)
		}
		out[key] = at
	}
	return out, rows.Err()
}

// MarkSourceCatalogChecked records a sync round that confirmed the cached copy
// is still fresh (the not-modified path). An answer is contact, so it advances
// last_seen exactly as a full snapshot does — for a member we never ping, this
// and the transfer path are the only liveness this node ever observes.
func (db *DB) MarkSourceCatalogChecked(ctx context.Context, sourceID int64, serial string, syncedAt int64) error {
	_, err := db.ExecContext(ctx, `
		UPDATE federation_catalog_sources
		   SET catalog_serial = ?, catalog_synced_at = ?, last_seen = MAX(last_seen, ?)
		 WHERE id = ?`, serial, syncedAt, syncedAt, sourceID)
	if err != nil {
		return fmt.Errorf("mark source catalog checked: %w", err)
	}
	return nil
}

// ReplaceSourceHoldings atomically replaces the cached list of what one source
// holds in its download cache and will seed (federation F4 holdings tracker,
// GET /madnetwork/v0/holdings). Invalid entries are skipped; duplicates collapse
// on the composite primary key.
func (db *DB) ReplaceSourceHoldings(ctx context.Context, sourceID int64, hashes []string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("replace source holdings: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM federation_holdings WHERE source_id = ?`, sourceID); err != nil {
		return fmt.Errorf("clear source holdings: %w", err)
	}
	ins, err := tx.PrepareContext(ctx,
		`INSERT OR IGNORE INTO federation_holdings (source_id, hash) VALUES (?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare holdings insert: %w", err)
	}
	defer ins.Close()
	for _, h := range hashes {
		if !isContentHash(h) {
			continue // remote input — a content hash is 64 lowercase hex
		}
		if _, err := ins.ExecContext(ctx, sourceID, h); err != nil {
			return fmt.Errorf("insert source holding: %w", err)
		}
	}
	return tx.Commit()
}

// isContentHash reports whether s is a well-formed content hash (64 lowercase
// hex) — the same shape federation.isBlobHash enforces, kept local so the
// database package carries no build-tagged dependency.
func isContentHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// ── Cached rows and the node they came from ──────────────────────────────────

// sourceJoin attaches a cached catalog or holdings row (alias) to the node it
// was pulled from — its SOURCE — and to the local peer row for that node when
// there is one. The peer join is LEFT because since F7 item 5 most sources are
// members no admin ever touched; it carries the two things a source row
// deliberately does not hold: the admin's label, and whether the admin blocked
// this node.
func sourceJoin(alias string) string {
	return `
	JOIN federation_catalog_sources s ON s.id = ` + alias + `.source_id
	LEFT JOIN federation_peers p ON p.public_key = s.public_key`
}

// notBlocked hides a blocked node's cached rows without deleting them, so an
// unblock restores the view with no resync. Before migration 036 this was the
// `p.state = 'friend'` join every browse query carried; it is the only trust
// condition left, because pulling a catalog no longer implies friendship.
const notBlocked = `COALESCE(p.state, '') <> 'blocked'`

// srcLastSeen is a source's freshness: the newest of what a catalog pull
// observed and what a friendship ping observed. A friend is pinged every minute
// and pulled every fifteen, a member is only ever pulled — so neither clock
// alone is the answer, and the later one always is.
const srcLastSeen = `MAX(s.last_seen, COALESCE(p.last_seen, 0))`

// sourceLabelExpr names a cached row's origin: the admin's label if this node is
// a peer they named, else what the node calls itself, else its short key. The
// SQL twin of federation.BlobProvider.Display.
//
// Two heard names, and both are consulted: a *friend's* claim is refreshed by the
// friendship ping and lands on the peer row, a *member's* by the discovery ping
// and lands on the source row. Reading only one of them made friends — the nodes
// an admin cares most about — show as bare key prefixes while strangers showed
// their names, which is exactly backwards.
const sourceHeardExpr = `COALESCE(NULLIF(p.heard_name, ''), s.heard_name)`
const sourceLabelExpr = `COALESCE(NULLIF(p.name, ''), NULLIF(` + sourceHeardExpr + `, ''), substr(s.public_key, 1, 12))`

// ── Blob lookup (federation F3/F4) ───────────────────────────────────────────

// madnetworkRowsForHash scans cached rows whose renditions advertise hash,
// most-recently-seen source first. The LIKE is a cheap pre-filter (a content
// hash is plain hex — no LIKE metacharacters); the JSON parse confirms.
func (db *DB) madnetworkRowsForHash(ctx context.Context, hash string,
	visit func(provider *federation.BlobProvider, entry *federation.CatalogEntry, rendition *federation.CatalogRendition) bool) error {
	rows, err := db.QueryContext(ctx, `
		SELECT s.id, COALESCE(p.id, 0), s.public_key, COALESCE(p.name, ''),
		       `+sourceHeardExpr+`, `+srcLastSeen+`,
		       c.entry_key, c.recording_key, c.title, c.artist, c.album_artist,
		       c.album, COALESCE(c.genre, ''), c.year, c.track_number, c.disc_number,
		       COALESCE(c.duration, 0), COALESCE(c.license, ''), c.guest_playable, c.renditions
		FROM federation_catalog c`+sourceJoin("c")+`
		WHERE c.renditions LIKE ? AND `+notBlocked+`
		ORDER BY `+srcLastSeen+` DESC, s.id, c.entry_key`, "%"+hash+"%")
	if err != nil {
		return fmt.Errorf("madnetwork rows for hash: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p federation.BlobProvider
		var e federation.CatalogEntry
		var year, track, disc sql.NullInt64
		var renditions string
		if err := rows.Scan(&p.SourceID, &p.PeerID, &p.PublicKey, &p.Name, &p.HeardName, &p.LastSeen,
			&e.Key, &e.RecordingKey, &e.Title, &e.Artist, &e.AlbumArtist, &e.Album,
			&e.Genre, &year, &track, &disc, &e.Duration, &e.License, &e.GuestPlayable,
			&renditions); err != nil {
			return fmt.Errorf("scan madnetwork hash row: %w", err)
		}
		e.Year, e.TrackNumber, e.DiscNumber = nullInt(year), nullInt(track), nullInt(disc)
		if err := json.Unmarshal([]byte(renditions), &e.Renditions); err != nil {
			continue // tolerate a damaged cache row
		}
		for i := range e.Renditions {
			if e.Renditions[i].Hash == hash {
				if !visit(&p, &e, &e.Renditions[i]) {
					return nil
				}
				break
			}
		}
	}
	return rows.Err()
}

// MadnetworkBlobProviders returns the nodes that hold hash — the swarm's
// tracker (federation F4). It unions two sources: nodes whose published catalog
// advertises the hash as a rendition (their library) and nodes advertising it in
// their download cache (federation_holdings). Ordered most-recently-seen first
// (the fetch order); the advertised byte size comes from the catalog (a hint; a
// cache-only holder contributes none and the fetch learns the size from the
// manifest). Satisfies the F4 half of federation.PeerStore.
//
// Since F7 item 5 a holder is any node we cache a catalog from — every member of
// our community the frontier has reached, not only a friend. That widening is
// the whole point of the phase: authorization was never what kept other people's
// libraries out of reach, knowing who holds a hash was.
func (db *DB) MadnetworkBlobProviders(ctx context.Context, hash string) (int64, []*federation.BlobProvider, error) {
	var size int64
	holders := map[int64]*federation.BlobProvider{}
	err := db.madnetworkRowsForHash(ctx, hash, func(p *federation.BlobProvider, _ *federation.CatalogEntry, rd *federation.CatalogRendition) bool {
		if size == 0 {
			size = rd.Size
		}
		if _, ok := holders[p.SourceID]; !ok {
			cp := *p
			holders[cp.SourceID] = &cp
		}
		return true
	})
	if err != nil {
		return 0, nil, err
	}

	// Cache holders (federation_holdings) — nodes seeding the blob from their
	// download cache without it being in their library catalog.
	rows, err := db.QueryContext(ctx, `
		SELECT s.id, COALESCE(p.id, 0), s.public_key, COALESCE(p.name, ''),
		       `+sourceHeardExpr+`, `+srcLastSeen+`
		FROM federation_holdings h`+sourceJoin("h")+`
		WHERE h.hash = ? AND `+notBlocked, hash)
	if err != nil {
		return 0, nil, fmt.Errorf("holdings providers: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p federation.BlobProvider
		if err := rows.Scan(&p.SourceID, &p.PeerID, &p.PublicKey, &p.Name, &p.HeardName, &p.LastSeen); err != nil {
			return 0, nil, fmt.Errorf("scan holdings provider: %w", err)
		}
		if _, ok := holders[p.SourceID]; !ok {
			cp := p
			holders[cp.SourceID] = &cp
		}
	}
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}

	out := make([]*federation.BlobProvider, 0, len(holders))
	for _, p := range holders {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastSeen != out[j].LastSeen {
			return out[i].LastSeen > out[j].LastSeen // most recently seen first
		}
		return out[i].SourceID < out[j].SourceID
	})
	return size, out, nil
}

// MadnetworkEntryForHash returns the catalog entry (tagset text) behind a
// rendition hash — from the most recently seen node advertising it — for the
// download-to-library staging metadata. Nil when nobody advertises it.
func (db *DB) MadnetworkEntryForHash(ctx context.Context, hash string) (*federation.CatalogEntry, error) {
	var entry *federation.CatalogEntry
	err := db.madnetworkRowsForHash(ctx, hash, func(_ *federation.BlobProvider, e *federation.CatalogEntry, _ *federation.CatalogRendition) bool {
		entry = e
		return false
	})
	return entry, err
}

// ── Merged browse (the /madnetwork drill-down) ───────────────────────────────

// MadnetworkView carries the merged-browse policy: whether to fold in this
// node's own published set, and the reachability Cutoff — a friend's rendition is
// shown only when its holder's last_seen >= Cutoff (docs/architecture/federation.md
// §Availability & node health). Cutoff <= 0 disables availability filtering (fail
// open, or the admin turned hiding off), which reproduces the pre-availability
// behaviour of showing every friend's cached catalog.
type MadnetworkView struct {
	IncludeSelf bool
	Cutoff      int64
	// PullCutoff is the same idea for a source whose only liveness clock is the
	// catalog pull — a member no friend of ours vouches for (F7 item 10,
	// §Availability, "Two clocks, two windows"). Three catalog cycles rather than
	// three ping rounds, because the window measures how recently we would have
	// NOTICED, and for such a node that is the rotation and not the ping. Zero
	// falls back to Cutoff, which reproduces the single-window behaviour.
	PullCutoff int64
	// PingedSince decides WHICH of the two a row is judged by: a source is on the
	// ping window if it is a direct friend, or if a freshness hint about it
	// arrived since this moment. It is *now − the ping window*, and it is set
	// even when the two cutoffs above are not — the class of a node is a fact
	// about who watches it, not about whether this request is filtering, and the
	// ⓘ panel greys holders by it while the browse shows everything (fail open).
	//
	// Asking "is a fast observer reporting NOW" rather than "did one ever" is
	// what keeps a healthy member visible when the friend that vouched for it
	// dies: the hints stop, the row falls back to the pull clock our own rotation
	// still refreshes, instead of being held to a window nothing feeds. Zero
	// classes every source as pinged, which is the pre-F7-item-10 behaviour.
	PingedSince int64
	// DefaultShareDepth is the node-level sharing scope the self-merged rows
	// inherit when a recording carries none (F5). A recording this node does not
	// publish is not on the network, so it must not appear on the network page
	// either — it stays in the local library at /, which is exactly the
	// distinction the admin made. Only the private/not-private boundary matters
	// here, so the zero value (DepthFriends) is the safe default for an unset
	// view: recordings without an explicit depth stay visible, explicitly
	// private ones do not.
	DefaultShareDepth int

	// SourceID restricts the browse to ONE cached catalog — the "By node" lane's
	// shelf (docs/ui/madnetwork-page.md §Browsing a single node). Zero is the
	// merged view. A single node's shelf never folds the own set in: browsing a
	// node means seeing what that node offers, and we are a different node.
	SourceID int64
	// SelfOnly is the same restriction pointed at ourselves — our own published
	// library as the network sees it, which is the one shelf on the list whose
	// contents an admin can actually change.
	SelfOnly bool
}

// includeRemote / includeOwn split a view into the two row sources the merged
// queries union. Keeping the rule in one place is what stops a source filter
// from being applied to one half of a UNION and forgotten on the other.
//
// Both can be false, and that case is load-bearing rather than an oversight:
// asking for OUR shelf on a node that publishes nothing to the network must
// answer with nothing. Answering with the merged catalog instead — the shape
// this had first — is the one answer that is certainly wrong.
func (v MadnetworkView) includeRemote() bool { return !v.SelfOnly }
func (v MadnetworkView) includeOwn() bool {
	return v.IncludeSelf && (v.SelfOnly || v.SourceID == 0)
}

// reachClause gates a source join by reachability. Both cutoffs are
// server-computed unix times (now − window), never user input, so inlining the
// integers is safe and avoids threading bound parameters through every shared
// fragment. An empty clause (Cutoff <= 0) leaves the join unfiltered.
//
// There is one window per CLASS OF OBSERVER, not one per query, because a single
// browse mixes them (F7 item 10, docs/architecture/federation.md §Availability,
// "Two clocks, two windows"):
//
//	friend            pinged every minute        → the tight ping window
//	hinted member     a friend pings it for us   → the tight ping window
//	pull-only member  reached by the rotation    → three catalog cycles
//
// A hint counts only while it is still ARRIVING (PingedSince, one ping window
// back), never for as long as one once did. Both failure modes turn on that:
//
//	the member died      hints keep coming, carrying a frozen observation, so the
//	                     row stays on the tight window and is hidden in 3 minutes
//	the VOUCHER died     hints stop, the row drops back to the pull clock our own
//	                     rotation still refreshes, and a healthy member stays up
//
// Reading the class off a hint that arrived 40 minutes ago would get the second
// case backwards — hiding a node we can reach, because somebody else stopped
// talking about it.
func reachClause(view MadnetworkView) string {
	if view.Cutoff <= 0 {
		return ""
	}
	pull := view.PullCutoff
	if pull <= 0 || pull >= view.Cutoff || view.PingedSince <= 0 {
		return fmt.Sprintf(" AND "+srcLastSeen+" >= %d", view.Cutoff)
	}
	return fmt.Sprintf(" AND "+srcLastSeen+" >= (CASE WHEN "+sourcePingedExpr+
		" THEN %d ELSE %d END)", view.PingedSince, view.Cutoff, pull)
}

// sourcePingedExpr is true for a source something watches on the one-minute ping
// cadence: our own friendship ping, or a friend's, relayed as a freshness hint.
// It takes the hint horizon as its one argument (MadnetworkView.PingedSince) and
// is also selected as a column, so the ⓘ panel greys a holder by the same window
// the browse filtered it by.
const sourcePingedExpr = `(COALESCE(p.state,'') = 'friend' OR s.hinted_at >= %d)`

// sourcePinged renders sourcePingedExpr as a selectable column. Without a
// horizon there is nothing to divide the sources by, so the column reads
// constant-true and every row is judged on the one window.
func sourcePinged(view MadnetworkView) string {
	if view.PingedSince <= 0 {
		return "1"
	}
	return fmt.Sprintf(sourcePingedExpr, view.PingedSince)
}

// reachable applies the view's own windows to a row judged after the query — the
// summary strip, which lists every source and greys the stale ones rather than
// filtering them out.
func (v MadnetworkView) reachable(lastSeen int64, pinged bool) bool {
	return ReachableAt(lastSeen, pinged, v.Cutoff, v.PullCutoff)
}

// ReachableAt is the Go twin of reachClause, for the callers that judge a row
// after it has been selected. The ⓘ panel's holder greying passes its own
// cutoffs rather than the view's, deliberately: when the browse fails open and
// shows everything, a stale holder must still read as stale instead of every
// holder suddenly looking reachable.
func ReachableAt(lastSeen int64, pinged bool, cutoff, pullCutoff int64) bool {
	if cutoff <= 0 {
		return true
	}
	if !pinged && pullCutoff > 0 && pullCutoff < cutoff {
		return lastSeen >= pullCutoff
	}
	return lastSeen >= cutoff
}

// sourceClause narrows a cached-row query to one source (the "By node" shelf).
// The id is an int64 the handler parsed out of the query string — a value that
// cannot carry SQL whatever it holds — so it is inlined like reachClause's
// cutoff rather than threaded as a bind through every shared fragment.
func sourceClause(view MadnetworkView) string {
	if view.SourceID <= 0 {
		return ""
	}
	return fmt.Sprintf(" AND s.id = %d", view.SourceID)
}

// The browse queries group by DISPLAY identity — the grouping artist is the
// album artist, falling back to the performer, falling back to the unknown
// bucket, mirroring the local library's album-artist-only artist list; albums
// fall back to the shared "Other" bucket. Only reachable, unblocked sources'
// catalogs are visible (reachClause; cutoff <= 0 = every source).
func fedcatBase(view MadnetworkView) string {
	return `
	FROM (SELECT COALESCE(NULLIF(c.album_artist, ''), NULLIF(c.artist, ''), '` + DefaultArtistName + `') AS akey,
	             COALESCE(NULLIF(c.album, ''), '` + DefaultAlbumTitle + `') AS alb,
	             ` + sourceLabelExpr + ` AS source_label,
	             ` + srcLastSeen + ` AS source_last_seen,
	             ` + sourcePinged(view) + ` AS source_pinged,
	             s.public_key AS source_key,
	             c.*
	      FROM federation_catalog c` + sourceJoin("c") + `
	      WHERE ` + notBlocked + reachClause(view) + sourceClause(view) + `)`
}

// fedcatRemoteRows / fedcatSelfRows are the two sources of the merged counting
// queries (artists / albums / summary / search), reduced to the columns those
// queries group and count by. The self source is this node's own published set
// — the same visibleTagset predicate and display-name resolution that
// PublishedCatalog advertises to friends, with fedcatBase's bucket fallbacks
// applied on top, so a track we publish folds with the same track cached from
// a friend (docs/ui/madnetwork-page.md §Own tracks).
func fedcatRemoteRows(view MadnetworkView) string {
	return `
	SELECT COALESCE(NULLIF(c.album_artist, ''), NULLIF(c.artist, ''), '` + DefaultArtistName + `') AS akey,
	       COALESCE(NULLIF(c.album, ''), '` + DefaultAlbumTitle + `') AS alb,
	       c.title AS title, c.track_number AS track_number,
	       c.disc_number AS disc_number, c.year AS year
	FROM federation_catalog c` + sourceJoin("c") + `
	WHERE ` + notBlocked + reachClause(view) + sourceClause(view)
}

// selfPublishedClause keeps the self-merged rows to what this node actually
// publishes: a recording is on the network iff its effective depth reaches at
// least a direct friend (F5). defaultDepth is a server-resolved integer, never
// user input, so it is inlined like reachClause's cutoff rather than threaded as
// a bind parameter through every shared fragment.
func selfPublishedClause(defaultDepth int) string {
	return fmt.Sprintf(" AND COALESCE(r.share_depth, %d) >= %d", defaultDepth, federation.DepthFriends)
}

func fedcatSelfRows(defaultDepth int) string {
	return `
	SELECT COALESCE(NULLIF(COALESCE(aar.name, m.album_artist, ''), ''),
	                NULLIF(COALESCE(par.name, m.artist, ''), ''), '` + DefaultArtistName + `') AS akey,
	       COALESCE(NULLIF(COALESCE(al.title, m.album, ''), ''), '` + DefaultAlbumTitle + `') AS alb,
	       m.title AS title, m.track_number AS track_number,
	       m.disc_number AS disc_number, m.year AS year
	FROM tagsets m` + recordingJoin + `
	LEFT JOIN artists par ON par.id = m.artist_id
	LEFT JOIN artists aar ON aar.id = m.album_artist_id
	LEFT JOIN albums al   ON al.id  = m.album_id
	WHERE ` + visibleTagset + selfPublishedClause(defaultDepth)
}

// fedcatCountBase is the FROM clause of the counting queries: reachable friends'
// catalogs (cutoff), optionally unioned with the own published set (always
// available — self is never gated). includeSelf is off when federation is
// disabled — the page then stays what the friends provide (nothing), matching
// the "list fully clears" rule. A source-filtered view (the "By node" shelf)
// keeps exactly one of the two halves.
func fedcatCountBase(view MadnetworkView) string {
	switch {
	case view.includeRemote() && view.includeOwn():
		return ` FROM (` + fedcatRemoteRows(view) + ` UNION ALL ` + fedcatSelfRows(view.DefaultShareDepth) + `)`
	case view.includeRemote():
		return ` FROM (` + fedcatRemoteRows(view) + `)`
	case view.includeOwn():
		return ` FROM (` + fedcatSelfRows(view.DefaultShareDepth) + `)`
	default:
		return ` FROM (` + fedcatNoRows + `)`
	}
}

// fedcatNoRows is a row source shaped like the other two and guaranteed empty —
// the view that includes neither half (§includeOwn). A well-typed nothing keeps
// every query above it unchanged; a special case at each call site would not.
const fedcatNoRows = `
	SELECT '' AS akey, '' AS alb, '' AS title, NULL AS track_number,
	       NULL AS disc_number, NULL AS year
	WHERE 0`

// Leading ORDER BY keys forcing the unknown buckets to the bottom of the
// alphabetical lists (the library's norm_name trick, matched on the canonical
// default strings — remote catalogs carry no normalized ids).
const artistBucketLast = `(lower(akey) = lower('` + DefaultArtistName + `')) ASC`
const albumBucketLast = `(lower(alb) = lower('` + DefaultAlbumTitle + `')) ASC`

// trackIdent is the merged logical-track identity inside one artist bucket:
// album + disc + track + title (case-insensitive) — the same text offered by
// several friends is ONE row.
const trackIdent = `lower(alb) || char(31) || COALESCE(disc_number, -1) || char(31) ||
	COALESCE(track_number, -1) || char(31) || lower(title)`

// trackFullIdent is that identity across the whole merged view rather than
// inside one artist bucket — the key the discovery lanes rank and the counting
// queries count distinct.
const trackFullIdent = `lower(akey) || char(31) || ` + trackIdent

// MadnetworkArtist is one row of the merged artist list.
type MadnetworkArtist struct {
	Name   string `json:"name"`
	Albums int64  `json:"albums"`
	Tracks int64  `json:"tracks"`
}

// MadnetworkArtists lists the merged catalog's artists (display-identity
// grouping, case-insensitive), optionally filtered by a substring. The unknown
// bucket sorts last; includeSelf merges the own published set in.
//
// The list is keyset-paged: limit <= 0 returns everything (the search path,
// which caps its own results), otherwise one page plus the cursor for the next.
// Browse all is now the community's whole output rather than a few friends'
// libraries, which is what took this off the "adopt when catalogs grow" list.
func (db *DB) MadnetworkArtists(ctx context.Context, q string, view MadnetworkView, limit int, cursor string) ([]*MadnetworkArtist, string, error) {
	conds, args := []string{}, []any{}
	if s := strings.TrimSpace(q); s != "" {
		escaped := strings.NewReplacer(`%`, `\%`, `_`, `\_`).Replace(s)
		conds = append(conds, `lower(akey) LIKE lower(?) ESCAPE '\'`)
		args = append(args, "%"+escaped+"%")
	}
	if c, ok := decodeArtistCursor(cursor); ok {
		// Row-value comparison over the ORDER BY key: the bucket flag first (the
		// unknown bucket sorts last), then the folded name. Applied row-level
		// because the grouping key is lower(akey), which is constant per group.
		// The cursor type is the library's — same sort key, minus the id a merged
		// row has no equivalent of (the folded name IS the group).
		conds = append(conds, `((`+artistBucketExpr+`), lower(akey)) > (?, ?)`)
		args = append(args, c.Unknown, c.Name)
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	page := ""
	if limit > 0 {
		page = " LIMIT ?"
		args = append(args, limit+1) // one extra row = "there is a next page"
	}
	rows, err := db.QueryContext(ctx, `
		SELECT MIN(akey), COUNT(DISTINCT lower(alb)), COUNT(DISTINCT `+trackIdent+`)
		`+fedcatCountBase(view)+where+`
		GROUP BY lower(akey)
		ORDER BY `+artistBucketLast+`, lower(akey)`+page, args...)
	if err != nil {
		return nil, "", fmt.Errorf("madnetwork artists: %w", err)
	}
	defer rows.Close()
	var out []*MadnetworkArtist
	for rows.Next() {
		var a MadnetworkArtist
		if err := rows.Scan(&a.Name, &a.Albums, &a.Tracks); err != nil {
			return nil, "", fmt.Errorf("scan madnetwork artist: %w", err)
		}
		out = append(out, &a)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if limit > 0 && len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1].Name
		c := artistCursor{Name: strings.ToLower(last)}
		if strings.EqualFold(last, DefaultArtistName) {
			c.Unknown = 1
		}
		next = encodeArtistCursor(c)
	}
	return out, next, nil
}

// artistBucketExpr is artistBucketLast without its sort direction — the same
// flag as a value, so the keyset cursor compares exactly what the ORDER BY
// orders by.
const artistBucketExpr = `lower(akey) = lower('` + DefaultArtistName + `')`

// MadnetworkAlbum is one row of an artist's merged album list.
type MadnetworkAlbum struct {
	Title  string `json:"title"`
	Tracks int64  `json:"tracks"`
	Year   *int64 `json:"year,omitempty"`
}

// MadnetworkAlbums lists one artist's albums in the merged catalog; the
// "Other" bucket sorts last.
func (db *DB) MadnetworkAlbums(ctx context.Context, artist string, view MadnetworkView) ([]*MadnetworkAlbum, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT MIN(alb), COUNT(DISTINCT `+trackIdent+`), MAX(year)
		`+fedcatCountBase(view)+`
		WHERE lower(akey) = lower(?)
		GROUP BY lower(alb)
		ORDER BY `+albumBucketLast+`, year IS NULL, year, lower(alb)`, artist)
	if err != nil {
		return nil, fmt.Errorf("madnetwork albums: %w", err)
	}
	defer rows.Close()
	var out []*MadnetworkAlbum
	for rows.Next() {
		var a MadnetworkAlbum
		var year sql.NullInt64
		if err := rows.Scan(&a.Title, &a.Tracks, &year); err != nil {
			return nil, fmt.Errorf("scan madnetwork album: %w", err)
		}
		a.Year = nullInt(year)
		out = append(out, &a)
	}
	return out, rows.Err()
}

// MadnetworkTrackRow is one RAW row of the merged track view — one per
// (source, appearance), a source being another node's cached catalog or, for
// Self rows, this node's own published set. The handler merges rows into
// logical tracks and their "N versions" (distinct claimed recordings) — set
// logic that reads better in Go than SQL at album scale.
type MadnetworkTrackRow struct {
	// SourceID / SourceName / SourceLastSeen describe the node the row came
	// from. Since F7 item 5 that node is not necessarily a friend — it is any
	// member of our community whose catalog the frontier has cached — so the
	// display name falls back through the admin's label, the node's own claim
	// and its short key.
	SourceID       int64
	SourceName     string
	SourceLastSeen int64
	// SourcePinged reports which freshness window this node is judged by — true
	// for one something pings every minute (our friend, or a friend's friend
	// vouching for it), false for one reached only by the catalog rotation (F7
	// item 10). The ⓘ panel needs it to grey a holder by the same window the
	// browse filtered it by; without it a perfectly fresh member reads as stale.
	SourcePinged bool
	// SourceKey is the node's public key — the map address of a holder, so the
	// library's ⓘ list can link one (F7 item 7). Empty on Self rows.
	SourceKey string
	Entry     federation.CatalogEntry

	// GroupArtist/GroupAlbum are the display-identity buckets (akey/alb) the
	// row belongs to — the search handler groups cross-album results by them.
	GroupArtist string
	GroupAlbum  string

	// Self rows come from the local library: SourceID is 0, Entry.Key is the
	// local tagset id, and ObjectKeys maps each rendition hash to its local
	// files object key (for direct /files/ play URLs).
	Self       bool
	ObjectKeys map[string]string
}

// remoteTrackRows runs the raw cached-row query with a caller-supplied match
// clause over the bucketed columns (akey/alb/title available). cutoff gates the
// rows to reachable sources (cutoff <= 0 = all).
func (db *DB) remoteTrackRows(ctx context.Context, view MadnetworkView, match string, args ...any) ([]*MadnetworkTrackRow, error) {
	if !view.includeRemote() {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT source_id, source_label, source_last_seen, source_pinged, source_key, akey, alb,
		       entry_key, recording_key, title, artist, album_artist,
		       COALESCE(genre, ''), year, track_number, disc_number,
		       COALESCE(duration, 0), COALESCE(license, ''), guest_playable, renditions
		`+fedcatBase(view)+`
		WHERE `+match+`
		ORDER BY (disc_number IS NULL) ASC, disc_number ASC, track_number ASC, lower(title) ASC, source_id ASC`,
		args...)
	if err != nil {
		return nil, fmt.Errorf("madnetwork tracks: %w", err)
	}
	defer rows.Close()
	var out []*MadnetworkTrackRow
	for rows.Next() {
		var r MadnetworkTrackRow
		var year, track, disc sql.NullInt64
		var renditions string
		if err := rows.Scan(&r.SourceID, &r.SourceName, &r.SourceLastSeen, &r.SourcePinged, &r.SourceKey, &r.GroupArtist, &r.GroupAlbum,
			&r.Entry.Key, &r.Entry.RecordingKey, &r.Entry.Title, &r.Entry.Artist,
			&r.Entry.AlbumArtist, &r.Entry.Genre, &year, &track, &disc,
			&r.Entry.Duration, &r.Entry.License, &r.Entry.GuestPlayable, &renditions); err != nil {
			return nil, fmt.Errorf("scan madnetwork track: %w", err)
		}
		r.Entry.Album = r.GroupAlbum
		r.Entry.Year, r.Entry.TrackNumber, r.Entry.DiscNumber = nullInt(year), nullInt(track), nullInt(disc)
		if err := json.Unmarshal([]byte(renditions), &r.Entry.Renditions); err != nil {
			r.Entry.Renditions = nil // tolerate a damaged cache row rather than failing the album
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// MadnetworkTracks returns reachable sources' cached rows for one artist+album,
// in display order (the view gates reachability and, for a single node's shelf,
// which source may answer at all).
func (db *DB) MadnetworkTracks(ctx context.Context, artist, album string, view MadnetworkView) ([]*MadnetworkTrackRow, error) {
	return db.remoteTrackRows(ctx, view, `lower(akey) = lower(?) AND lower(alb) = lower(?)`, artist, album)
}

// Self-row display-identity expressions over the tagsets join (aliases par /
// aar / al as in fedcatSelfRows) — the WHERE side of the bucket fallbacks.
const selfAkeyExpr = `COALESCE(NULLIF(COALESCE(aar.name, m.album_artist, ''), ''),
	NULLIF(COALESCE(par.name, m.artist, ''), ''), '` + DefaultArtistName + `')`
const selfAlbExpr = `COALESCE(NULLIF(COALESCE(al.title, m.album, ''), ''), '` + DefaultAlbumTitle + `')`

// ownTrackRows returns this node's own published appearances matching a
// caller-supplied clause (akey/alb/title-level), shaped like cached catalog
// rows: Self = true, SourceID 0, Entry.Key = tagset id, renditions attached from
// the recording's live files with their local object keys. defaultDepth applies
// the same self-published filter as the counting queries, so a recording kept
// off the network cannot be listed by a view whose counts already exclude it.
func (db *DB) ownTrackRows(ctx context.Context, view MadnetworkView, match string, args ...any) ([]*MadnetworkTrackRow, error) {
	if !view.includeOwn() {
		return nil, nil
	}
	defaultDepth := view.DefaultShareDepth
	rows, err := db.QueryContext(ctx, `
		SELECT m.id, m.recording_id, `+selfAkeyExpr+`, `+selfAlbExpr+`, m.title,
		       COALESCE(par.name, m.artist, ''), COALESCE(aar.name, m.album_artist, ''),
		       COALESCE(m.genre, ''), m.year, m.track_number, m.disc_number,
		       COALESCE(r.license, ''), r.guest_playable
		FROM tagsets m`+recordingJoin+`
		LEFT JOIN artists par ON par.id = m.artist_id
		LEFT JOIN artists aar ON aar.id = m.album_artist_id
		LEFT JOIN albums al   ON al.id  = m.album_id
		WHERE `+visibleTagset+selfPublishedClause(defaultDepth)+` AND `+match+`
		ORDER BY (m.disc_number IS NULL) ASC, m.disc_number ASC, m.track_number ASC, lower(m.title) ASC, m.id ASC`,
		args...)
	if err != nil {
		return nil, fmt.Errorf("madnetwork own tracks: %w", err)
	}
	defer rows.Close()

	var out []*MadnetworkTrackRow
	recordings := map[string][]int{} // recording key -> row indexes awaiting renditions
	for rows.Next() {
		r := MadnetworkTrackRow{Self: true, ObjectKeys: map[string]string{}}
		var tagsetID, recordingID int64
		var year, track, disc sql.NullInt64
		if err := rows.Scan(&tagsetID, &recordingID, &r.GroupArtist, &r.GroupAlbum, &r.Entry.Title,
			&r.Entry.Artist, &r.Entry.AlbumArtist, &r.Entry.Genre, &year, &track, &disc,
			&r.Entry.License, &r.Entry.GuestPlayable); err != nil {
			return nil, fmt.Errorf("scan madnetwork own track: %w", err)
		}
		r.Entry.Key = strconv.FormatInt(tagsetID, 10)
		r.Entry.RecordingKey = strconv.FormatInt(recordingID, 10)
		r.Entry.Album = r.GroupAlbum
		r.Entry.Year, r.Entry.TrackNumber, r.Entry.DiscNumber = nullInt(year), nullInt(track), nullInt(disc)
		r.Entry.Renditions = []federation.CatalogRendition{}
		recordings[r.Entry.RecordingKey] = append(recordings[r.Entry.RecordingKey], len(out))
		out = append(out, &r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}

	// Attach each recording's live renditions plus their object keys (the
	// local /files/ addresses the browse serves for self-held tracks).
	ids := make([]any, 0, len(recordings))
	ph := make([]string, 0, len(recordings))
	for key := range recordings {
		ids = append(ids, key)
		ph = append(ph, "?")
	}
	rrows, err := db.QueryContext(ctx, `
		SELECT f.recording_id, f.hash, f.byte_size, f.object_key,
		       COALESCE(mm.codec, ''), COALESCE(mm.bitrate, 0),
		       COALESCE(mm.sample_rate, 0), COALESCE(mm.bit_depth, 0),
		       COALESCE(mm.duration_seconds, 0)
		FROM files f
		LEFT JOIN media_metadata mm ON mm.file_id = f.id
		WHERE f.deleted_at IS NULL AND f.recording_id IN (`+strings.Join(ph, ",")+`)
		ORDER BY f.recording_id, f.id`, ids...)
	if err != nil {
		return nil, fmt.Errorf("madnetwork own renditions: %w", err)
	}
	defer rrows.Close()
	for rrows.Next() {
		var recordingID int64
		var objectKey string
		var rd federation.CatalogRendition
		if err := rrows.Scan(&recordingID, &rd.Hash, &rd.Size, &objectKey, &rd.Codec,
			&rd.Bitrate, &rd.SampleRate, &rd.BitDepth, &rd.Duration); err != nil {
			return nil, fmt.Errorf("scan madnetwork own rendition: %w", err)
		}
		for _, i := range recordings[strconv.FormatInt(recordingID, 10)] {
			out[i].Entry.Renditions = append(out[i].Entry.Renditions, rd)
			out[i].ObjectKeys[rd.Hash] = objectKey
			if out[i].Entry.Duration == 0 && rd.Duration > 0 {
				out[i].Entry.Duration = rd.Duration
			}
		}
	}
	return out, rrows.Err()
}

// MadnetworkOwnTracks returns the own published rows for one artist+album —
// the Self side of the merged track view.
func (db *DB) MadnetworkOwnTracks(ctx context.Context, artist, album string, view MadnetworkView) ([]*MadnetworkTrackRow, error) {
	return db.ownTrackRows(ctx, view,
		`lower(`+selfAkeyExpr+`) = lower(?) AND lower(`+selfAlbExpr+`) = lower(?)`, artist, album)
}

// ── Merged search (docs/ui/madnetwork-page.md §Search) ───────────────────────

// MadnetworkSearchAlbum is one album hit of the merged search.
type MadnetworkSearchAlbum struct {
	Artist string `json:"artist_name"`
	Title  string `json:"title"`
	Tracks int64  `json:"track_count"`
	Year   *int64 `json:"year,omitempty"`
}

// MadnetworkSearchAlbums lists merged albums whose title matches a substring.
func (db *DB) MadnetworkSearchAlbums(ctx context.Context, q string, limit int, view MadnetworkView) ([]*MadnetworkSearchAlbum, error) {
	s := strings.TrimSpace(q)
	if s == "" || limit <= 0 {
		return []*MadnetworkSearchAlbum{}, nil
	}
	escaped := strings.NewReplacer(`%`, `\%`, `_`, `\_`).Replace(s)
	rows, err := db.QueryContext(ctx, `
		SELECT MIN(akey), MIN(alb), COUNT(DISTINCT `+trackIdent+`), MAX(year)
		`+fedcatCountBase(view)+`
		WHERE lower(alb) LIKE lower(?) ESCAPE '\'
		GROUP BY lower(akey), lower(alb)
		ORDER BY `+albumBucketLast+`, lower(alb), lower(akey)
		LIMIT ?`, "%"+escaped+"%", limit)
	if err != nil {
		return nil, fmt.Errorf("madnetwork search albums: %w", err)
	}
	defer rows.Close()
	out := []*MadnetworkSearchAlbum{}
	for rows.Next() {
		var a MadnetworkSearchAlbum
		var year sql.NullInt64
		if err := rows.Scan(&a.Artist, &a.Title, &a.Tracks, &year); err != nil {
			return nil, fmt.Errorf("scan madnetwork search album: %w", err)
		}
		a.Year = nullInt(year)
		out = append(out, &a)
	}
	return out, rows.Err()
}

// searchRowCap bounds the raw rows fed into the search handler's track merge —
// a LIKE over titles can fan out; the handler caps the merged result anyway.
const searchRowCap = 400

// MadnetworkSearchTrackRows returns the raw rows (remote and, when includeSelf,
// own) whose title matches a substring. Rows arrive grouped by source; the
// handler groups by (GroupArtist, GroupAlbum) and merges per group, so having
// every source's rows for a matching title keeps version folding correct.
func (db *DB) MadnetworkSearchTrackRows(ctx context.Context, q string, view MadnetworkView) ([]*MadnetworkTrackRow, error) {
	s := strings.TrimSpace(q)
	if s == "" {
		return []*MadnetworkTrackRow{}, nil
	}
	escaped := "%" + strings.NewReplacer(`%`, `\%`, `_`, `\_`).Replace(s) + "%"
	rows, err := db.remoteTrackRows(ctx, view, `lower(title) LIKE lower(?) ESCAPE '\'`, escaped)
	if err != nil {
		return nil, err
	}
	own, err := db.ownTrackRows(ctx, view, `lower(m.title) LIKE lower(?) ESCAPE '\'`, escaped)
	if err != nil {
		return nil, err
	}
	rows = append(rows, own...)
	if len(rows) > searchRowCap {
		rows = rows[:searchRowCap]
	}
	return rows, nil
}

// MadnetworkFriend is one node's sync status on the /madnetwork page strip.
// The strip lists every node whose catalog this node caches (reachable or not);
// Reachable drives the greying of one seen longer ago than the view's freshness
// window, and Friend distinguishes the nodes an admin hand-picked from the
// members the frontier reached on its own.
type MadnetworkFriend struct {
	// ID is the catalog-source row, the address of this node's shelf in the
	// "By node" lane. Stable for as long as we cache the node.
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	LastSeen  int64  `json:"last_seen"`
	SyncedAt  int64  `json:"synced_at"`
	Entries   int64  `json:"entries"`
	Reachable bool   `json:"reachable"`
	Friend    bool   `json:"friend"`
	// Pinged reports which freshness window judged this node — see
	// MadnetworkTrackRow.SourcePinged. Not serialized: the client is told the
	// verdict (Reachable), not the arithmetic behind it.
	Pinged bool `json:"-"`
}

// MadnetworkSummary reports the merged catalog's shape: every source with sync
// state and reachability, plus the merged distinct track count over the visible
// (reachable + own) set. The source list is not filtered by reachability — the
// strip shows them all and greys the unreachable — but the track count uses the
// view's cutoff. Direct friends sort first: they are the nodes an admin chose,
// and the strip is where an admin looks for them.
func (db *DB) MadnetworkSummary(ctx context.Context, view MadnetworkView) ([]*MadnetworkFriend, int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT s.id, `+sourceLabelExpr+`, `+srcLastSeen+`, s.catalog_synced_at,
		       (SELECT COUNT(*) FROM federation_catalog c WHERE c.source_id = s.id),
		       COALESCE(p.state, '') = 'friend', `+sourcePinged(view)+`
		FROM federation_catalog_sources s
		LEFT JOIN federation_peers p ON p.public_key = s.public_key
		WHERE `+notBlocked+`
		  AND (COALESCE(p.state, '') = 'friend'
		       OR EXISTS (SELECT 1 FROM federation_catalog c2 WHERE c2.source_id = s.id))
		ORDER BY (COALESCE(p.state, '') = 'friend') DESC, lower(`+sourceLabelExpr+`), s.id`)
	if err != nil {
		return nil, 0, fmt.Errorf("madnetwork summary: %w", err)
	}
	defer rows.Close()
	var friends []*MadnetworkFriend
	for rows.Next() {
		var f MadnetworkFriend
		if err := rows.Scan(&f.ID, &f.Name, &f.LastSeen, &f.SyncedAt, &f.Entries, &f.Friend, &f.Pinged); err != nil {
			return nil, 0, fmt.Errorf("scan madnetwork source: %w", err)
		}
		f.Reachable = view.reachable(f.LastSeen, f.Pinged)
		friends = append(friends, &f)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	var tracks int64
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT `+trackFullIdent+`)
		`+fedcatCountBase(view)).Scan(&tracks); err != nil {
		return nil, 0, fmt.Errorf("madnetwork track count: %w", err)
	}
	return friends, tracks, nil
}

func nullInt(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	return &v.Int64
}

func nullFloat(v float64) any {
	if v == 0 {
		return nil
	}
	return v
}
