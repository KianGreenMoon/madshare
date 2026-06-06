package api

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"daemonlord.ygg/madshare/auth"
)

// usernamePattern constrains new usernames to a sane, URL/log-safe set: 3–32
// chars of letters, digits, and . _ - (must start with a letter or digit).
var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{2,31}$`)

// listRoles returns the assignable roles for the admin UI's role picker.
func (h *manageHandler) listRoles(w http.ResponseWriter, r *http.Request) {
	roles, err := h.store.ListRoles(r.Context())
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(roles))
	for _, role := range roles {
		out = append(out, map[string]any{"name": role.Name, "built_in": role.BuiltIn})
	}
	writeJSON(w, http.StatusOK, out)
}

// knownRoleSet returns the set of valid role names, or writes a 500 and returns
// ok=false on a storage error.
func (h *manageHandler) knownRoleSet(w http.ResponseWriter, r *http.Request) (map[string]bool, bool) {
	roles, err := h.store.ListRoles(r.Context())
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return nil, false
	}
	set := make(map[string]bool, len(roles))
	for _, role := range roles {
		set[role.Name] = true
	}
	return set, true
}

// createUser handles POST /api/admin/users. The default role is "listener"
// (play + download) — the common case of adding a regular listening account.
func (h *manageHandler) createUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username              string   `json:"username"`
		Password              string   `json:"password"`
		Roles                 []string `json:"roles"`
		RequirePasswordChange bool     `json:"require_password_change"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if !usernamePattern.MatchString(req.Username) {
		http.Error(w, "invalid username (3–32 chars: letters, digits, . _ -)", http.StatusBadRequest)
		return
	}
	if len(req.Password) < minPasswordLen {
		http.Error(w, "password too short", http.StatusBadRequest)
		return
	}
	roles := req.Roles
	if len(roles) == 0 {
		roles = []string{auth.RoleListener}
	}
	known, ok := h.knownRoleSet(w, r)
	if !ok {
		return
	}
	for _, role := range roles {
		if !known[role] {
			http.Error(w, "unknown role: "+role, http.StatusBadRequest)
			return
		}
	}

	if existing, err := h.store.GetUserByUsername(r.Context(), req.Username); err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	} else if existing != nil {
		http.Error(w, "username already exists", http.StatusConflict)
		return
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		http.Error(w, "could not create user", http.StatusInternalServerError)
		return
	}
	id, err := h.store.CreateUser(r.Context(), req.Username, hash, req.RequirePasswordChange)
	if err != nil {
		http.Error(w, "username already exists", http.StatusConflict)
		return
	}
	if err := h.store.SetUserRoles(r.Context(), id, roles); err != nil {
		http.Error(w, "user created but role assignment failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.mAudit(r.Context(), "user.create", "user:"+strconv.FormatInt(id, 10),
		req.Username+" ["+strings.Join(roles, ",")+"]")
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "username": req.Username, "roles": roles})
}

// updateUser handles PATCH /api/admin/users/{id}: change role set and/or the
// disabled flag. Both fields are optional (pointers ⇒ "field present"). Guards
// prevent an admin from locking themselves or the last admin out.
func (h *manageHandler) updateUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(w, r, "id")
	if !ok {
		return
	}
	var req struct {
		Roles    *[]string `json:"roles"`
		Disabled *bool     `json:"disabled"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}

	target, err := h.store.GetUserByID(r.Context(), id)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if target == nil {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}

	self := actorID(r.Context())
	isSelf := self.Valid && self.Int64 == id

	allRoles, err := h.store.AllUserRoles(r.Context())
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	wasAdmin := hasRole(allRoles[id], auth.RoleAdmin)

	// Determine the resulting admin status to enforce the last-admin guard.
	willBeAdmin := wasAdmin
	if req.Roles != nil {
		willBeAdmin = hasRole(*req.Roles, auth.RoleAdmin)
	}
	willBeDisabled := target.Disabled
	if req.Disabled != nil {
		willBeDisabled = *req.Disabled
	}

	// Block changes that would remove the final enabled administrator.
	losingAdmin := wasAdmin && (!willBeAdmin || willBeDisabled)
	if losingAdmin {
		admins, err := h.store.CountEnabledUsersWithRole(r.Context(), auth.RoleAdmin)
		if err != nil {
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
		if admins <= 1 {
			http.Error(w, "cannot remove the last administrator", http.StatusBadRequest)
			return
		}
	}
	if isSelf && req.Disabled != nil && *req.Disabled {
		http.Error(w, "you cannot disable your own account", http.StatusBadRequest)
		return
	}

	if req.Roles != nil {
		known, ok := h.knownRoleSet(w, r)
		if !ok {
			return
		}
		for _, role := range *req.Roles {
			if !known[role] {
				http.Error(w, "unknown role: "+role, http.StatusBadRequest)
				return
			}
		}
		if err := h.store.SetUserRoles(r.Context(), id, *req.Roles); err != nil {
			http.Error(w, "could not set roles: "+err.Error(), http.StatusBadRequest)
			return
		}
		h.mAudit(r.Context(), "user.roles", "user:"+strconv.FormatInt(id, 10), strings.Join(*req.Roles, ","))
	}

	if req.Disabled != nil && *req.Disabled != target.Disabled {
		if _, err := h.store.SetUserDisabled(r.Context(), id, *req.Disabled); err != nil {
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
		if *req.Disabled {
			// Revoke active sessions so a disabled user is logged out immediately.
			_ = h.store.DeleteUserSessions(r.Context(), id)
		}
		h.mAudit(r.Context(), "user.disabled", "user:"+strconv.FormatInt(id, 10), strconv.FormatBool(*req.Disabled))
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

// resetUserPassword handles POST /api/admin/users/{id}/password. It sets a new
// password and logs the user out everywhere, optionally forcing a change on next
// login.
func (h *manageHandler) resetUserPassword(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(w, r, "id")
	if !ok {
		return
	}
	var req struct {
		NewPassword           string `json:"new_password"`
		RequirePasswordChange bool   `json:"require_password_change"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.NewPassword) < minPasswordLen {
		http.Error(w, "password too short", http.StatusBadRequest)
		return
	}
	target, err := h.store.GetUserByID(r.Context(), id)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if target == nil {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}
	hash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		http.Error(w, "could not set password", http.StatusInternalServerError)
		return
	}
	if err := h.store.SetPassword(r.Context(), id, hash, req.RequirePasswordChange); err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	_ = h.store.DeleteUserSessions(r.Context(), id)
	h.mAudit(r.Context(), "user.password", "user:"+strconv.FormatInt(id, 10), "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id})
}

// deleteUser handles DELETE /api/admin/users/{id}. It refuses to delete the
// caller's own account or the last enabled administrator.
func (h *manageHandler) deleteUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt(w, r, "id")
	if !ok {
		return
	}
	self := actorID(r.Context())
	if self.Valid && self.Int64 == id {
		http.Error(w, "you cannot delete your own account", http.StatusBadRequest)
		return
	}

	allRoles, err := h.store.AllUserRoles(r.Context())
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if hasRole(allRoles[id], auth.RoleAdmin) {
		admins, err := h.store.CountEnabledUsersWithRole(r.Context(), auth.RoleAdmin)
		if err != nil {
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
		if admins <= 1 {
			http.Error(w, "cannot delete the last administrator", http.StatusBadRequest)
			return
		}
	}

	found, err := h.store.DeleteUser(r.Context(), id)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(w, "user not found", http.StatusNotFound)
		return
	}
	h.mAudit(r.Context(), "user.delete", "user:"+strconv.FormatInt(id, 10), "")
	w.WriteHeader(http.StatusNoContent)
}

func hasRole(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
