package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"daemonlord.ygg/madshare/auth"
	"daemonlord.ygg/madshare/database"
	"github.com/go-chi/chi/v5"
)

// minPasswordLen is the minimum accepted password length.
const minPasswordLen = 8

// AuthStore is the persistence the auth endpoints depend on. *database.DB
// satisfies it.
type AuthStore interface {
	auth.Store
	GetUserByUsername(ctx context.Context, username string) (*database.User, error)
	CreateSession(ctx context.Context, tokenHash string, userID, expiresAt int64) error
	DeleteSession(ctx context.Context, tokenHash string) error
	DeleteUserSessions(ctx context.Context, userID int64) error
	SetPassword(ctx context.Context, userID int64, passwordHash string, changeRequired bool) error
	CreateToken(ctx context.Context, userID int64, name, tokenHash string, expiresAt int64) (int64, error)
	ListTokens(ctx context.Context, userID int64) ([]*database.APIToken, error)
	RevokeToken(ctx context.Context, userID, tokenID int64) (bool, error)
}

type authHandler struct {
	store    AuthStore
	throttle *loginThrottle
}

// registerAuth mounts the /api/auth/* endpoints. These self-check the identity
// (set by the auth.Identify middleware) rather than relying on
// RequirePermission, since any authenticated user may manage their own session,
// password, and tokens.
func registerAuth(r chi.Router, store AuthStore) {
	h := &authHandler{store: store, throttle: newLoginThrottle()}
	r.Post("/api/auth/login", h.login)
	r.Post("/api/auth/logout", h.logout)
	r.Get("/api/auth/me", h.me)
	r.Post("/api/auth/password", h.changePassword)
	r.Get("/api/auth/tokens", h.listTokens)
	r.Post("/api/auth/tokens", h.createToken)
	r.Delete("/api/auth/tokens/{id}", h.revokeToken)
}

// identityJSON is the shape returned for the current principal.
type identityJSON struct {
	Username               string   `json:"username"`
	Permissions            []string `json:"permissions"`
	PasswordChangeRequired bool     `json:"password_change_required"`
}

func toIdentityJSON(id *auth.Identity) identityJSON {
	perms := make([]string, 0, len(id.Permissions))
	for p := range id.Permissions {
		perms = append(perms, p)
	}
	return identityJSON{
		Username:               id.Username,
		Permissions:            perms,
		PasswordChangeRequired: id.PasswordChangeRequired,
	}
}

func (h *authHandler) login(w http.ResponseWriter, r *http.Request) {
	// Per-IP throttle: login is unauthenticated and every attempt runs an
	// expensive argon2id verify, making it both a brute-force target and a
	// resource-exhaustion vector. allowIP slows guessing from a single source.
	if h.throttle != nil && !h.throttle.allowIP(clientIP(r)) {
		http.Error(w, "too many login attempts, slow down", http.StatusTooManyRequests)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Cap concurrent password verifications (each ~64 MiB). Acquiring before the
	// user lookup gates the missing-user and real-user paths alike, so the
	// timing equalization below is not undone under load.
	if h.throttle != nil {
		release, ok := h.throttle.acquire()
		if !ok {
			http.Error(w, "server busy, try again shortly", http.StatusServiceUnavailable)
			return
		}
		defer release()
	}

	user, err := h.store.GetUserByUsername(r.Context(), req.Username)
	if err != nil {
		http.Error(w, "login failed", http.StatusInternalServerError)
		return
	}
	// Same generic 401 whether the user is missing, disabled, or the password is
	// wrong — and the same argon2 work either way (DummyVerifyPassword on the
	// miss path), so the response time does not leak which.
	if user == nil || user.Disabled {
		auth.DummyVerifyPassword(req.Password)
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	ok, err := auth.VerifyPassword(user.PasswordHash, req.Password)
	if err != nil || !ok {
		http.Error(w, "invalid credentials", http.StatusUnauthorized)
		return
	}

	raw, err := auth.GenerateSecret()
	if err != nil {
		http.Error(w, "login failed", http.StatusInternalServerError)
		return
	}
	expires := time.Now().Add(auth.SessionTTL)
	if err := h.store.CreateSession(r.Context(), auth.HashSecret(raw), user.ID, expires.Unix()); err != nil {
		http.Error(w, "login failed", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookieName,
		Value:    raw,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, identityJSON{
		Username:               user.Username,
		Permissions:            []string{}, // populated on /me; login just confirms
		PasswordChangeRequired: user.PasswordChangeRequired,
	})
}

func (h *authHandler) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(auth.SessionCookieName); err == nil && c.Value != "" {
		_ = h.store.DeleteSession(r.Context(), auth.HashSecret(c.Value))
	}
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
	w.WriteHeader(http.StatusNoContent)
}

func (h *authHandler) me(w http.ResponseWriter, r *http.Request) {
	id, ok := requireIdentity(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, toIdentityJSON(id))
}

func (h *authHandler) changePassword(w http.ResponseWriter, r *http.Request) {
	id, ok := requireIdentity(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if len(req.NewPassword) < minPasswordLen {
		http.Error(w, "new password too short", http.StatusBadRequest)
		return
	}
	user, err := h.store.GetUserByUsername(r.Context(), id.Username)
	if err != nil || user == nil {
		http.Error(w, "password change failed", http.StatusInternalServerError)
		return
	}
	ok2, err := auth.VerifyPassword(user.PasswordHash, req.OldPassword)
	if err != nil || !ok2 {
		http.Error(w, "current password incorrect", http.StatusUnauthorized)
		return
	}
	hash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		http.Error(w, "password change failed", http.StatusInternalServerError)
		return
	}
	if err := h.store.SetPassword(r.Context(), user.ID, hash, false); err != nil {
		http.Error(w, "password change failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *authHandler) createToken(w http.ResponseWriter, r *http.Request) {
	id, ok := requireIdentity(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var req struct {
		Name string `json:"name"`
		// ExpiresAt is an absolute unix timestamp (seconds); the web UI's date
		// picker posts this. ExpiresInDays is the duration form kept for
		// non-browser API clients. 0/absent in both means the token never expires;
		// ExpiresAt wins when both are sent.
		ExpiresAt     int64 `json:"expires_at"`
		ExpiresInDays int   `json:"expires_in_days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		http.Error(w, "token name required", http.StatusBadRequest)
		return
	}
	var expires int64
	switch {
	case req.ExpiresAt > 0:
		if req.ExpiresAt <= time.Now().Unix() {
			http.Error(w, "expiry must be in the future", http.StatusBadRequest)
			return
		}
		expires = req.ExpiresAt
	case req.ExpiresInDays > 0:
		expires = time.Now().Add(time.Duration(req.ExpiresInDays) * 24 * time.Hour).Unix()
	}
	raw, err := auth.GenerateSecret()
	if err != nil {
		http.Error(w, "token creation failed", http.StatusInternalServerError)
		return
	}
	tokenID, err := h.store.CreateToken(r.Context(), id.UserID, req.Name, auth.HashSecret(raw), expires)
	if err != nil {
		http.Error(w, "token creation failed", http.StatusInternalServerError)
		return
	}
	// The raw token is shown exactly once.
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":    tokenID,
		"name":  req.Name,
		"token": raw,
	})
}

func (h *authHandler) listTokens(w http.ResponseWriter, r *http.Request) {
	id, ok := requireIdentity(w, r)
	if !ok {
		return
	}
	tokens, err := h.store.ListTokens(r.Context(), id.UserID)
	if err != nil {
		http.Error(w, "failed to list tokens", http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(tokens))
	for _, t := range tokens {
		out = append(out, map[string]any{
			"id":         t.ID,
			"name":       t.Name,
			"created_at": t.CreatedAt,
			"last_used":  nullIntJSON(t.LastUsedAt),
			"expires_at": nullIntJSON(t.ExpiresAt),
			"revoked":    t.RevokedAt.Valid,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *authHandler) revokeToken(w http.ResponseWriter, r *http.Request) {
	id, ok := requireIdentity(w, r)
	if !ok {
		return
	}
	tokenID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid token id", http.StatusBadRequest)
		return
	}
	found, err := h.store.RevokeToken(r.Context(), id.UserID, tokenID)
	if err != nil {
		http.Error(w, "revoke failed", http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(w, "token not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// requireIdentity returns the request's identity, or writes 401 and returns
// ok=false when the request is anonymous.
func requireIdentity(w http.ResponseWriter, r *http.Request) (*auth.Identity, bool) {
	id := auth.FromContext(r.Context())
	if id == nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return nil, false
	}
	return id, true
}

// nullIntJSON renders a sql.NullInt64 as its value or JSON null.
func nullIntJSON(n sql.NullInt64) any {
	if n.Valid {
		return n.Int64
	}
	return nil
}
