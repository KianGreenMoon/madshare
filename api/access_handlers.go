package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"daemonlord.ygg/madshare/auth"
	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/federation"
	"github.com/go-chi/chi/v5"
)

// ManageStore is the persistence the content-access management endpoints depend
// on (access groups, grants, per-file guest/license flags, and the auto-derive
// policy). *database.DB satisfies it. Kept separate from Repository so the
// upload/listing fakes in tests need not implement it.
type ManageStore interface {
	ListUsers(ctx context.Context) ([]*database.User, error)

	// User administration (user.manage). See user_handlers.go.
	GetUserByUsername(ctx context.Context, username string) (*database.User, error)
	GetUserByID(ctx context.Context, id int64) (*database.User, error)
	CreateUser(ctx context.Context, username, passwordHash string, changeRequired bool) (int64, error)
	SetPassword(ctx context.Context, userID int64, passwordHash string, changeRequired bool) error
	SetUserDisabled(ctx context.Context, userID int64, disabled bool) (bool, error)
	DeleteUser(ctx context.Context, userID int64) (bool, error)
	DeleteUserSessions(ctx context.Context, userID int64) error
	ListRoles(ctx context.Context) ([]database.Role, error)
	AllUserRoles(ctx context.Context) (map[int64][]string, error)
	SetUserRoles(ctx context.Context, userID int64, roleNames []string) error
	CountEnabledUsersWithRole(ctx context.Context, roleName string) (int, error)

	SetGuestPlayable(ctx context.Context, hash string, guest bool) (bool, error)
	SetLicense(ctx context.Context, hash, license string) (bool, error)

	GetAutoDerivePolicy(ctx context.Context) (database.AutoDerivePolicy, error)
	SetAutoDerivePolicy(ctx context.Context, p database.AutoDerivePolicy) error

	GetTrashRestorePolicy(ctx context.Context) (string, error)
	SetTrashRestorePolicy(ctx context.Context, mode string) error

	GetTagsourcePolicy(ctx context.Context) (database.TagsourcePolicy, error)
	SetTagsourcePolicy(ctx context.Context, enabled bool, apiKey *string) error

	GetMadnetworkPolicy(ctx context.Context) (database.MadnetworkPolicy, error)
	SetMadnetworkPolicy(ctx context.Context, p database.MadnetworkPolicy) error
	// The download cache's ceiling is separate from the policy above because it
	// is three-valued: nil = inherit [federation].cache_max_mb, a value = pin it
	// (0 meaning "no limit", a real override).
	GetCacheCeiling(ctx context.Context) (*int64, error)
	SetCacheCeiling(ctx context.Context, maxBytes *int64) error
	// The cache's age rule is a plain number beside it (0 = off): with no
	// config layer there is no "inherit" state to tell apart from zero. See
	// database.GetCacheMaxAgeDays for why it has none.
	GetCacheMaxAgeDays(ctx context.Context) (int64, error)
	SetCacheMaxAgeDays(ctx context.Context, days int64) error

	RecordAudit(ctx context.Context, actorUserID sql.NullInt64, action, target, detail string) error
}

// knownLicenses is the controlled vocabulary accepted for files.license (§5.1).
// The empty string clears the license.
var knownLicenses = map[string]bool{
	"":                    true,
	"CC0-1.0":             true,
	"CC-BY-4.0":           true,
	"CC-BY-SA-4.0":        true,
	"public-domain":       true,
	"all-rights-reserved": true,
	"unknown":             true,
}

type manageHandler struct {
	store ManageStore
	// sweep enforces the download cache's ceiling right after it is changed. It
	// is injected rather than built here because it needs the cache directory and
	// the running transfers, neither of which is this package's to know; nil
	// simply means the hourly sweep is the only one.
	sweep CacheSweeper
	// cacheDefault is [federation].cache_max_mb in bytes — what the settings
	// card's "Default" resolves to, and what an unset override inherits.
	cacheDefault int64
}

// registerManage mounts the content-access management endpoints. Group/grant/
// membership administration and the auto-derive policy are user.manage; per-file
// guest_playable/license edits are metadata.edit. The caller applies those
// gates per-route (see RegisterAdmin).
func registerManage(r chi.Router, d Deps) {
	h := &manageHandler{store: d.Manage, sweep: d.CacheSweep, cacheDefault: d.CacheDefaultBytes}
	userManage := d.protect(auth.PermUserManage)
	metaEdit := d.protect(auth.PermMetadataEdit)

	r.With(userManage).Get("/users", h.listUsers)
	r.With(userManage).Post("/users", h.createUser)
	r.With(userManage).Patch("/users/{id}", h.updateUser)
	r.With(userManage).Post("/users/{id}/password", h.resetUserPassword)
	r.With(userManage).Delete("/users/{id}", h.deleteUser)
	r.With(userManage).Get("/roles", h.listRoles)

	r.With(metaEdit).Post("/files/{hash}/guest", h.setGuest)
	r.With(metaEdit).Post("/files/{hash}/license", h.setLicense)

	r.With(userManage).Get("/settings/autoderive", h.getAutoDerive)
	r.With(userManage).Post("/settings/autoderive", h.setAutoDerive)
	r.With(userManage).Get("/settings/trash-policy", h.getTrashPolicy)
	r.With(userManage).Post("/settings/trash-policy", h.setTrashPolicy)
	r.With(userManage).Get("/settings/tagsource", h.getTagsource)
	r.With(userManage).Post("/settings/tagsource", h.setTagsource)
	r.With(userManage).Get("/settings/madnetwork", h.getMadnetworkSettings)
	r.With(userManage).Post("/settings/madnetwork", h.setMadnetworkSettings)
}

// mAudit records a privileged management action, logging (never failing) on
// error — mirrors handler.audit.
func (h *manageHandler) mAudit(ctx context.Context, action, target, detail string) {
	if err := h.store.RecordAudit(ctx, actorID(ctx), action, target, detail); err != nil {
		log.Printf("audit %s %s: %v", action, target, err)
	}
}

func (h *manageHandler) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := h.store.ListUsers(r.Context())
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	roles, err := h.store.AllUserRoles(r.Context())
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(users))
	for _, u := range users {
		rs := roles[u.ID]
		if rs == nil {
			rs = []string{}
		}
		out = append(out, map[string]any{
			"id":                       u.ID,
			"username":                 u.Username,
			"disabled":                 u.Disabled,
			"roles":                    rs,
			"password_change_required": u.PasswordChangeRequired,
			"created_at":               u.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *manageHandler) setGuest(w http.ResponseWriter, r *http.Request) {
	hash := chi.URLParam(r, "hash")
	if !adminHashPattern.MatchString(hash) {
		http.Error(w, "invalid hash", http.StatusBadRequest)
		return
	}
	var req struct {
		GuestPlayable bool `json:"guest_playable"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	found, err := h.store.SetGuestPlayable(r.Context(), hash, req.GuestPlayable)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	h.mAudit(r.Context(), "access.guest", hash, strconv.FormatBool(req.GuestPlayable))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "guest_playable": req.GuestPlayable})
}

func (h *manageHandler) setLicense(w http.ResponseWriter, r *http.Request) {
	hash := chi.URLParam(r, "hash")
	if !adminHashPattern.MatchString(hash) {
		http.Error(w, "invalid hash", http.StatusBadRequest)
		return
	}
	var req struct {
		License string `json:"license"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if !knownLicenses[req.License] {
		http.Error(w, "unknown license", http.StatusBadRequest)
		return
	}
	found, err := h.store.SetLicense(r.Context(), hash, req.License)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	h.mAudit(r.Context(), "access.license", hash, req.License)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "license": req.License})
}

func (h *manageHandler) getAutoDerive(w http.ResponseWriter, r *http.Request) {
	p, err := h.store.GetAutoDerivePolicy(r.Context())
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	licenses := p.Licenses
	if licenses == nil {
		licenses = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": p.Enabled, "licenses": licenses})
}

func (h *manageHandler) setAutoDerive(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled  bool     `json:"enabled"`
		Licenses []string `json:"licenses"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	for _, lic := range req.Licenses {
		if !knownLicenses[lic] || lic == "" {
			http.Error(w, "unknown license in allow-list", http.StatusBadRequest)
			return
		}
	}
	if err := h.store.SetAutoDerivePolicy(r.Context(), database.AutoDerivePolicy{Enabled: req.Enabled, Licenses: req.Licenses}); err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	h.mAudit(r.Context(), "access.autoderive", "", strconv.FormatBool(req.Enabled)+": "+strings.Join(req.Licenses, ","))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": req.Enabled})
}

func (h *manageHandler) getTrashPolicy(w http.ResponseWriter, r *http.Request) {
	p, err := h.store.GetTrashRestorePolicy(r.Context())
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"policy": p})
}

func (h *manageHandler) setTrashPolicy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Policy string `json:"policy"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if !database.ValidTrashRestorePolicy(req.Policy) {
		http.Error(w, "unknown trash-restore policy", http.StatusBadRequest)
		return
	}
	if err := h.store.SetTrashRestorePolicy(r.Context(), req.Policy); err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	h.mAudit(r.Context(), "upload.trash_policy", "", req.Policy)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "policy": req.Policy})
}

// keyLast4 returns the trailing 4 characters of a stored secret for display
// ("which key is this") without revealing it.
func keyLast4(k string) string {
	if len(k) <= 4 {
		return k
	}
	return k[len(k)-4:]
}

// getTagsource reports the tag-services settings. The stored AcoustID key is
// never echoed — only whether one is set, plus its last 4 characters.
func (h *manageHandler) getTagsource(w http.ResponseWriter, r *http.Request) {
	p, err := h.store.GetTagsourcePolicy(r.Context())
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"musicbrainz_enabled": p.MusicBrainzEnabled,
		"api_key_set":         p.AcoustIDKey != "",
		"api_key_last4":       keyLast4(p.AcoustIDKey),
	})
}

// setTagsource updates the tag-services settings. api_key semantics: absent =
// keep the stored key, "" = clear it, anything else = replace it. Enabling
// MusicBrainz requires an effective key — the lookup cannot work without one.
func (h *manageHandler) setTagsource(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MusicBrainzEnabled bool    `json:"musicbrainz_enabled"`
		APIKey             *string `json:"api_key"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	stored, err := h.store.GetTagsourcePolicy(r.Context())
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	effectiveKey := stored.AcoustIDKey
	if req.APIKey != nil {
		effectiveKey = strings.TrimSpace(*req.APIKey)
	}
	if req.MusicBrainzEnabled && effectiveKey == "" {
		http.Error(w, "an AcoustID API key is required to enable MusicBrainz lookups", http.StatusBadRequest)
		return
	}
	if err := h.store.SetTagsourcePolicy(r.Context(), req.MusicBrainzEnabled, req.APIKey); err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	// Audit the change without the key material.
	keyChange := "key kept"
	if req.APIKey != nil {
		if effectiveKey == "" {
			keyChange = "key cleared"
		} else {
			keyChange = "key set"
		}
	}
	h.mAudit(r.Context(), "tagsource.settings", "", strconv.FormatBool(req.MusicBrainzEnabled)+"; "+keyChange)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                  true,
		"musicbrainz_enabled": req.MusicBrainzEnabled,
		"api_key_set":         effectiveKey != "",
		"api_key_last4":       keyLast4(effectiveKey),
	})
}

// getMadnetworkSettings reports the madnetwork download + seeding settings
// (federation F3/F4): whether downloads skip the review bucket, and whether the
// node seeds blobs / its download cache to friends.
func (h *manageHandler) getMadnetworkSettings(w http.ResponseWriter, r *http.Request) {
	p, err := h.store.GetMadnetworkPolicy(r.Context())
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	out := map[string]any{
		"autoapprove_downloads": p.AutoapproveDownloads,
		"seed_enabled":          p.SeedEnabled,
		"seed_cache":            p.SeedCache,
		"hide_unavailable":      p.HideUnavailable,
		"default_share_depth":   p.DefaultShareDepth,
		"serve_guests":          p.ServeGuests,
		"publish_friend_list":   p.PublishFriendList,
	}
	h.describeCeiling(r, out)
	writeJSON(w, http.StatusOK, out)
}

// describeCeiling reports the download cache's ceiling in the three parts a
// client needs to render it: the override (null when there is none), what the
// config file says, and which of the two is therefore in force.
//
// Naming the DEFAULT matters as much as the value. "Default" as a UI choice is
// meaningless unless it says what it resolves to — and on a server that is
// usually 0, meaning the cache is not capped at all.
func (h *manageHandler) describeCeiling(r *http.Request, out map[string]any) {
	override, err := h.store.GetCacheCeiling(r.Context())
	if err != nil {
		log.Printf("read cache ceiling: %v", err)
		return
	}
	out["cache_max_bytes"] = nil
	out["cache_source"] = "config"
	if override != nil {
		out["cache_max_bytes"] = *override
		out["cache_source"] = "override"
	}
	out["cache_default_bytes"] = h.cacheDefault
	out["cache_effective_bytes"] = database.ResolveCacheCeiling(override, h.cacheDefault)
	if days, err := h.store.GetCacheMaxAgeDays(r.Context()); err != nil {
		log.Printf("read cache age policy: %v", err)
	} else {
		out["cache_max_age_days"] = days
	}
}

// setMadnetworkSettings updates the madnetwork download + seeding settings. The
// seed fields default true (missing = keep the "seed by default" stance) so an
// older client that only sends autoapprove_downloads does not silently disable
// seeding.
//
// Everything a client may omit is a *pointer* and absent means **unchanged**,
// which is the only shape that is safe here: this endpoint writes the whole
// policy row, so a field the request does not mention would otherwise be reset
// to its zero value. That is not hypothetical — publish_friend_list has no
// control on the settings card at all, and every save of the seed checkboxes
// used to silently switch it off, which under F7 walls a node's own friends off
// from each other (they stop being vouched for, so they stop being members).
// serve_guests is a pointer for the same reason turned outward, and
// default_share_depth because 0 is a meaningful scope (Direct friends), so an
// absent field must not narrow the node to it.
func (h *manageHandler) setMadnetworkSettings(w http.ResponseWriter, r *http.Request) {
	req := struct {
		AutoapproveDownloads bool  `json:"autoapprove_downloads"`
		SeedEnabled          bool  `json:"seed_enabled"`
		SeedCache            bool  `json:"seed_cache"`
		HideUnavailable      bool  `json:"hide_unavailable"`
		DefaultShareDepth    *int  `json:"default_share_depth"`
		ServeGuests          *bool `json:"serve_guests"`
		PublishFriendList    *bool `json:"publish_friend_list"`
		// Three-valued, the same shape share_depth and the swarm rates use and
		// for the same reason: absent ≠ null ≠ a number. Absent leaves the
		// ceiling alone, null clears the override back to the config file, and a
		// number pins it — including 0, which means "no limit" and is a real
		// override rather than a synonym for "unset".
		CacheMaxBytes json.RawMessage `json:"cache_max_bytes"`
		// The age half is two-valued (0 = off), so a pointer is enough: absent
		// leaves it alone, a number sets it. There is no third state to spell,
		// because there is no config layer under it to inherit from.
		CacheMaxAgeDays *int64 `json:"cache_max_age_days"`
	}{SeedEnabled: true, SeedCache: true, HideUnavailable: true}
	if !decodeJSON(w, r, &req) {
		return
	}
	current, err := h.store.GetMadnetworkPolicy(r.Context())
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	depth := current.DefaultShareDepth
	if req.DefaultShareDepth != nil {
		depth = *req.DefaultShareDepth
		if !federation.ValidDepth(depth) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid default share depth"})
			return
		}
	}
	keep := func(v *bool, cur bool) bool {
		if v == nil {
			return cur
		}
		return *v
	}
	p := database.MadnetworkPolicy{
		AutoapproveDownloads: req.AutoapproveDownloads,
		SeedEnabled:          req.SeedEnabled,
		SeedCache:            req.SeedCache,
		HideUnavailable:      req.HideUnavailable,
		DefaultShareDepth:    depth,
		ServeGuests:          keep(req.ServeGuests, current.ServeGuests),
		PublishFriendList:    keep(req.PublishFriendList, current.PublishFriendList),
	}
	// Decided before anything is written, so a malformed ceiling refuses the
	// whole save rather than applying half of it.
	ceiling, changed, ok := h.ceilingUpdate(w, r, req.CacheMaxBytes)
	if !ok {
		return
	}
	if req.CacheMaxAgeDays != nil && *req.CacheMaxAgeDays < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "error": "cache_max_age_days must be a count of days ≥ 0 (0 = keep everything)"})
		return
	}
	if err := h.store.SetMadnetworkPolicy(r.Context(), p); err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if changed {
		if err := h.store.SetCacheCeiling(r.Context(), ceiling); err != nil {
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
	}
	if req.CacheMaxAgeDays != nil {
		if err := h.store.SetCacheMaxAgeDays(r.Context(), *req.CacheMaxAgeDays); err != nil {
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
	}
	h.mAudit(r.Context(), "madnetwork.settings", "",
		"autoapprove_downloads="+strconv.FormatBool(p.AutoapproveDownloads)+
			" seed_enabled="+strconv.FormatBool(p.SeedEnabled)+
			" seed_cache="+strconv.FormatBool(p.SeedCache)+
			" hide_unavailable="+strconv.FormatBool(p.HideUnavailable)+
			" default_share_depth="+strconv.Itoa(p.DefaultShareDepth)+
			" serve_guests="+strconv.FormatBool(p.ServeGuests)+
			" publish_friend_list="+strconv.FormatBool(p.PublishFriendList)+
			" cache_ceiling="+ceilingLabel(ceiling, changed)+
			" cache_max_age_days="+ageLabel(req.CacheMaxAgeDays))

	resp := map[string]any{
		"ok":                    true,
		"autoapprove_downloads": p.AutoapproveDownloads,
		"seed_enabled":          p.SeedEnabled,
		"seed_cache":            p.SeedCache,
		"hide_unavailable":      p.HideUnavailable,
		"default_share_depth":   p.DefaultShareDepth,
		"serve_guests":          p.ServeGuests,
		"publish_friend_list":   p.PublishFriendList,
	}
	h.describeCeiling(r, resp)
	// Enforce it now rather than at the next hourly sweep. Somebody who has just
	// lowered a ceiling is watching the disk, and a number that takes an hour to
	// mean anything reads as a control that does not work.
	effective, _ := resp["cache_effective_bytes"].(int64)
	ageDays, _ := resp["cache_max_age_days"].(int64)
	if removed, freed := h.sweepCache(r.Context(), effective, ageDays); removed > 0 {
		resp["evicted"], resp["freed_bytes"] = removed, freed
	}
	writeJSON(w, http.StatusOK, resp)
}

// ceilingUpdate decodes the three-valued cache ceiling. changed is false when
// the field was absent; ok is false when the caller has already been answered
// with a 400.
func (h *manageHandler) ceilingUpdate(w http.ResponseWriter, r *http.Request, raw json.RawMessage) (*int64, bool, bool) {
	if len(raw) == 0 {
		return nil, false, true // absent: unchanged
	}
	if string(raw) == "null" {
		return nil, true, true // explicit null: back to the config file
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil || n < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "error": "cache_max_bytes must be null (use the configured default) or a byte count ≥ 0"})
		return nil, false, false
	}
	return &n, true, true
}

// ageLabel renders an age-policy update for the audit log. An absent field is
// "unchanged", which is a different thing from 0 ("keep everything") and the
// log must not blur them.
func ageLabel(days *int64) string {
	if days == nil {
		return "unchanged"
	}
	if *days == 0 {
		return "off"
	}
	return strconv.FormatInt(*days, 10)
}

// ceilingLabel renders a ceiling update for the audit log.
func ceilingLabel(v *int64, changed bool) string {
	switch {
	case !changed:
		return "unchanged"
	case v == nil:
		return "default"
	case *v == 0:
		return "unlimited"
	}
	return strconv.FormatInt(*v, 10)
}

// sweepCache applies the ceiling immediately, logging rather than failing: the
// setting HAS been saved by the time this runs, and reporting a 500 for a
// housekeeping pass would tell the caller their change did not stick.
func (h *manageHandler) sweepCache(ctx context.Context, maxBytes, maxAgeDays int64) (int, int64) {
	if (maxBytes <= 0 && maxAgeDays <= 0) || h.sweep == nil {
		return 0, 0
	}
	removed, freed, err := h.sweep(ctx, maxBytes, maxAgeDays)
	if err != nil {
		log.Printf("cache retention sweep: %v", err)
	} else if removed > 0 {
		log.Printf("cache retention: evicted %d blob(s), freed %d bytes", removed, freed)
	}
	return removed, freed
}

// decodeJSON decodes the request body into v, writing a 400 and returning false
// on a malformed body. An empty body is treated as an empty object.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return false
	}
	return true
}

// pathInt parses an int64 URL parameter, writing a 400 and returning false when
// it is missing or non-numeric.
func pathInt(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	v, err := strconv.ParseInt(chi.URLParam(r, name), 10, 64)
	if err != nil {
		http.Error(w, "invalid "+name, http.StatusBadRequest)
		return 0, false
	}
	return v, true
}
