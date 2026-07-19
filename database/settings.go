package database

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// Settings keys (see migration 006_access_mgmt.sql; settings is a generic
// key/value table, so new keys need no migration).
const (
	settingAutoDeriveEnabled     = "access.autoderive.enabled"
	settingAutoDeriveLicenses    = "access.autoderive.licenses"
	settingTrashRestorePolicy    = "upload.trash_restore_policy"
	settingMusicBrainzEnabled    = "tagsource.musicbrainz.enabled"
	settingAcoustIDKey           = "tagsource.acoustid.api_key"
	settingMadnetworkAutoapprove = "madnetwork.autoapprove_downloads"
	settingMadnetworkSeedEnabled = "madnetwork.seed_enabled"
	settingMadnetworkSeedCache   = "madnetwork.seed_cache"
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
	return MadnetworkPolicy{
		AutoapproveDownloads: auto == "1",
		SeedEnabled:          seed != "0",  // default on
		SeedCache:            cache != "0", // default on
	}, nil
}

// SetMadnetworkPolicy persists the madnetwork settings atomically.
func (db *DB) SetMadnetworkPolicy(ctx context.Context, p MadnetworkPolicy) error {
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
	} {
		if _, err := tx.ExecContext(ctx, upsert, kv.key, kv.val); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SeedingPolicy reports whether this node serves blobs to friends and whether
// it seeds its download cache — the F4 swarm-serving gate the embedded node
// consults on every blob/manifest/holdings request. Both default on. Satisfies
// the F4 half of federation.PeerStore.
func (db *DB) SeedingPolicy(ctx context.Context) (enabled, cache bool, err error) {
	p, err := db.GetMadnetworkPolicy(ctx)
	if err != nil {
		return false, false, err
	}
	return p.SeedEnabled, p.SeedCache, nil
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
