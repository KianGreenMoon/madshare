package database

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"

	"daemonlord.ygg/madshare/federation"
)

// Settings keys (see migration 006_access_mgmt.sql; settings is a generic
// key/value table, so new keys need no migration).
const (
	settingAutoDeriveEnabled         = "access.autoderive.enabled"
	settingAutoDeriveLicenses        = "access.autoderive.licenses"
	settingTrashRestorePolicy        = "upload.trash_restore_policy"
	settingMusicBrainzEnabled        = "tagsource.musicbrainz.enabled"
	settingAcoustIDKey               = "tagsource.acoustid.api_key"
	settingMadnetworkAutoapprove     = "madnetwork.autoapprove_downloads"
	settingMadnetworkSeedEnabled     = "madnetwork.seed_enabled"
	settingMadnetworkSeedCache       = "madnetwork.seed_cache"
	settingMadnetworkHideUnavailable = "madnetwork.hide_unavailable"
	settingMadnetworkDefaultDepth    = "madnetwork.default_share_depth"
	settingMadnetworkPublishFriends  = "madnetwork.publish_friend_list"
	settingMadnetworkServeGuests     = "madnetwork.serve_guests"
)

// Trash-restore policy modes — what may happen to a trashed file whose content
// is uploaded again. See docs/api/upload.md (Trash-restore policy).
const (
	TrashReuploadRestores = "reupload_restores" // default: re-uploading the bytes restores it (historical behavior)
	TrashInform           = "inform"            // don't restore; tell the uploader to ask an admin
	TrashUploaderRestore  = "uploader_restore"  // an uploader may restore via POST /api/files/{hash}/restore
)

// ValidTrashRestorePolicy reports whether m is a known policy mode.
func ValidTrashRestorePolicy(m string) bool {
	switch m {
	case TrashReuploadRestores, TrashInform, TrashUploaderRestore:
		return true
	}
	return false
}

// GetTrashRestorePolicy reads the trash-restore policy. A missing or unrecognised
// value reads as the default (reupload_restores — the historical behavior).
func (db *DB) GetTrashRestorePolicy(ctx context.Context) (string, error) {
	v, ok, err := db.GetSetting(ctx, settingTrashRestorePolicy)
	if err != nil {
		return TrashReuploadRestores, err
	}
	if !ok || !ValidTrashRestorePolicy(v) {
		return TrashReuploadRestores, nil
	}
	return v, nil
}

// SetTrashRestorePolicy persists the trash-restore policy.
func (db *DB) SetTrashRestorePolicy(ctx context.Context, mode string) error {
	if !ValidTrashRestorePolicy(mode) {
		return errors.New("invalid trash restore policy")
	}
	return db.SetSetting(ctx, settingTrashRestorePolicy, mode)
}

// MadnetworkPolicy holds the madnetwork download + seeding behavior (federation
// F3/F4). AutoapproveDownloads skips the review bucket: a downloaded file lands
// approved, exactly as fetched (default OFF on servers). SeedEnabled is the
// master switch for serving blobs to friends over the swarm; SeedCache controls
// whether the download cache is served and advertised in holdings — both
// default ON ("everything a node holds seeds by default"). The upload rate cap
// is a static config knob ([federation] seed_rate_kib), not stored here.
type MadnetworkPolicy struct {
	AutoapproveDownloads bool
	SeedEnabled          bool
	SeedCache            bool
	// HideUnavailable hides tracks held only by an unreachable friend from the
	// merged browse (the availability rule). Default on; off shows every friend's
	// cached catalog regardless of reachability (docs/plans/availability.md).
	HideUnavailable bool
	// DefaultShareDepth is the node-level sharing scope (F5): the depth every
	// recording that carries no explicit share_depth inherits. Default
	// federation.DepthUnlimited (∞ — the whole reachable madnetwork);
	// federation.DepthFriends restricts the node to direct friends,
	// federation.DepthPrivate publishes nothing at all.
	DefaultShareDepth int
	// PublishFriendList controls whether this node publishes its own friend-list
	// record to the gossip (F6). Default on, matching the network's transparent
	// default.
	//
	// Off means "I publish no record" and nothing more: a friendship has two
	// ends, and friends' own records still name this node, so it stays on the
	// map with visible edges — only its own list goes missing. Any UI for this
	// must say exactly that rather than imply invisibility
	// (docs/architecture/federation.md §Friend-list gossip).
	PublishFriendList bool
	// ServeGuests answers mesh nodes outside our community with guest-playable
	// content (F7). Default OFF — the node's posture is everything to our
	// community, nothing outside it, and this is the one switch that crosses that
	// line. It is byte endpoints only: catalog and holdings never leave the
	// community, whatever this says.
	ServeGuests bool
}

// GetMadnetworkPolicy reads the madnetwork settings. Missing keys read as the
// defaults: autoapprove off (downloads go through review), seeding on, cache
// seeding on.
func (db *DB) GetMadnetworkPolicy(ctx context.Context) (MadnetworkPolicy, error) {
	auto, _, err := db.GetSetting(ctx, settingMadnetworkAutoapprove)
	if err != nil {
		return MadnetworkPolicy{}, err
	}
	seed, _, err := db.GetSetting(ctx, settingMadnetworkSeedEnabled)
	if err != nil {
		return MadnetworkPolicy{}, err
	}
	cache, _, err := db.GetSetting(ctx, settingMadnetworkSeedCache)
	if err != nil {
		return MadnetworkPolicy{}, err
	}
	hide, _, err := db.GetSetting(ctx, settingMadnetworkHideUnavailable)
	if err != nil {
		return MadnetworkPolicy{}, err
	}
	depth, _, err := db.GetSetting(ctx, settingMadnetworkDefaultDepth)
	if err != nil {
		return MadnetworkPolicy{}, err
	}
	publish, _, err := db.GetSetting(ctx, settingMadnetworkPublishFriends)
	if err != nil {
		return MadnetworkPolicy{}, err
	}
	guests, _, err := db.GetSetting(ctx, settingMadnetworkServeGuests)
	if err != nil {
		return MadnetworkPolicy{}, err
	}
	return MadnetworkPolicy{
		AutoapproveDownloads: auto == "1",
		SeedEnabled:          seed != "0",  // default on
		SeedCache:            cache != "0", // default on
		HideUnavailable:      hide != "0",  // default on
		DefaultShareDepth:    parseShareDepth(depth),
		PublishFriendList:    publish != "0", // default on
		ServeGuests:          guests == "1",  // default OFF
	}, nil
}

// parseShareDepth reads the stored node-default depth, falling back to ∞ for an
// unset or unparseable value — the historical behavior (publish the whole
// approved library) and the documented default.
func parseShareDepth(raw string) int {
	d, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || !federation.ValidDepth(d) {
		return federation.DepthUnlimited
	}
	return d
}

// SetMadnetworkPolicy persists the madnetwork settings atomically. An
// out-of-range default depth is rejected rather than silently clamped: it is the
// node's whole sharing scope, so a typo must not quietly widen or close it.
func (db *DB) SetMadnetworkPolicy(ctx context.Context, p MadnetworkPolicy) error {
	if !federation.ValidDepth(p.DefaultShareDepth) {
		return errors.New("invalid default share depth")
	}
	bit := func(b bool) string {
		if b {
			return "1"
		}
		return "0"
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	upsert := `INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`
	for _, kv := range []struct {
		key, val string
	}{
		{settingMadnetworkAutoapprove, bit(p.AutoapproveDownloads)},
		{settingMadnetworkSeedEnabled, bit(p.SeedEnabled)},
		{settingMadnetworkSeedCache, bit(p.SeedCache)},
		{settingMadnetworkHideUnavailable, bit(p.HideUnavailable)},
		{settingMadnetworkDefaultDepth, strconv.Itoa(p.DefaultShareDepth)},
		{settingMadnetworkPublishFriends, bit(p.PublishFriendList)},
		{settingMadnetworkServeGuests, bit(p.ServeGuests)},
	} {
		if _, err := tx.ExecContext(ctx, upsert, kv.key, kv.val); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SeedingPolicy reports what this node serves over the swarm — the gate the
// embedded node consults on every blob/manifest/holdings request. Satisfies the
// F4 half of federation.PeerStore. A read error yields the zero policy, which
// serves nothing.
func (db *DB) SeedingPolicy(ctx context.Context) (federation.SeedPolicy, error) {
	p, err := db.GetMadnetworkPolicy(ctx)
	if err != nil {
		return federation.SeedPolicy{}, err
	}
	return federation.SeedPolicy{
		Enabled: p.SeedEnabled,
		Cache:   p.SeedCache,
		Guests:  p.ServeGuests,
	}, nil
}

// PublishFriendList reports whether this node publishes its own friend-list
// record to the gossip — the F6 half of federation.PeerStore. Default on.
func (db *DB) PublishFriendList(ctx context.Context) (bool, error) {
	p, err := db.GetMadnetworkPolicy(ctx)
	if err != nil {
		return false, err
	}
	return p.PublishFriendList, nil
}

// GetSetting returns the value for key. ok is false (no error) when unset.
func (db *DB) GetSetting(ctx context.Context, key string) (value string, ok bool, err error) {
	err = db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

// SetSetting upserts a key/value setting.
func (db *DB) SetSetting(ctx context.Context, key, value string) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// TagsourcePolicy is the external tag-suggestion service configuration
// (docs/architecture/tag-suggestions.md P1). The AcoustID key is stored
// plaintext — it must be replayable to the service, unlike our own hashed
// tokens — and the API layer never echoes it back in full.
type TagsourcePolicy struct {
	MusicBrainzEnabled bool
	AcoustIDKey        string
}

// GetTagsourcePolicy reads the tag-service settings. Missing keys read as
// disabled / no key.
func (db *DB) GetTagsourcePolicy(ctx context.Context) (TagsourcePolicy, error) {
	var p TagsourcePolicy
	enabled, _, err := db.GetSetting(ctx, settingMusicBrainzEnabled)
	if err != nil {
		return p, err
	}
	p.MusicBrainzEnabled = enabled == "1"
	key, _, err := db.GetSetting(ctx, settingAcoustIDKey)
	if err != nil {
		return p, err
	}
	p.AcoustIDKey = key
	return p, nil
}

// SetTagsourcePolicy persists the tag-service settings atomically. A nil
// apiKey keeps the stored key (the UI never round-trips it); a pointer to ""
// clears it.
func (db *DB) SetTagsourcePolicy(ctx context.Context, enabled bool, apiKey *string) error {
	val := "0"
	if enabled {
		val = "1"
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	upsert := `INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`
	if _, err := tx.ExecContext(ctx, upsert, settingMusicBrainzEnabled, val); err != nil {
		return err
	}
	if apiKey != nil {
		if _, err := tx.ExecContext(ctx, upsert, settingAcoustIDKey, strings.TrimSpace(*apiKey)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AutoDerivePolicy is the opt-in license-based guest-access setting (auth.md
// §5.1). When Enabled, files whose license is in Licenses are guest-accessible
// via a live query-time check in accessClause (no writes to guest_playable).
type AutoDerivePolicy struct {
	Enabled  bool
	Licenses []string
}

// GetAutoDerivePolicy reads the current auto-derivation policy. A missing
// setting reads as disabled with an empty allow-list.
func (db *DB) GetAutoDerivePolicy(ctx context.Context) (AutoDerivePolicy, error) {
	var p AutoDerivePolicy
	enabled, _, err := db.GetSetting(ctx, settingAutoDeriveEnabled)
	if err != nil {
		return p, err
	}
	p.Enabled = enabled == "1"
	licenses, _, err := db.GetSetting(ctx, settingAutoDeriveLicenses)
	if err != nil {
		return p, err
	}
	p.Licenses = splitLicenses(licenses)
	return p, nil
}

// SetAutoDerivePolicy persists the auto-derivation policy atomically. Access
// derived from the policy takes effect immediately at query time via accessClause.
func (db *DB) SetAutoDerivePolicy(ctx context.Context, p AutoDerivePolicy) error {
	val := "0"
	if p.Enabled {
		val = "1"
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	upsert := `INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`
	if _, err := tx.ExecContext(ctx, upsert, settingAutoDeriveEnabled, val); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, upsert, settingAutoDeriveLicenses, strings.Join(normalizeLicenses(p.Licenses), ",")); err != nil {
		return err
	}
	return tx.Commit()
}

// splitLicenses parses a stored comma-separated allow-list into a slice.
func splitLicenses(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return normalizeLicenses(strings.Split(s, ","))
}

// normalizeLicenses trims whitespace and drops empty entries.
func normalizeLicenses(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
