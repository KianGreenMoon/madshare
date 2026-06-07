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
}

// registerManage mounts the content-access management endpoints. Group/grant/
// membership administration and the auto-derive policy are user.manage; per-file
// guest_playable/license edits are metadata.edit. The caller applies those
// gates per-route (see RegisterAdmin).
func registerManage(r chi.Router, d Deps) {
	h := &manageHandler{store: d.Manage}
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

// decodeJSON decodes the request body into v, writing a 400 and returning false
// on a malformed body. An empty body is treated as an empty object.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
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
