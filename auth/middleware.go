package auth

import (
	"context"
	"net/http"
	"strings"
)

// Store is the persistence the middleware needs to resolve credentials to an
// identity. Implementations return (nil, nil) when the credential is unknown,
// expired, revoked, or belongs to a disabled user.
type Store interface {
	// SessionIdentity resolves a session token hash to an identity and updates
	// the session's last-seen timestamp.
	SessionIdentity(ctx context.Context, tokenHash string) (*Identity, error)
	// TokenIdentity resolves an API token hash to an identity and updates the
	// token's last-used timestamp.
	TokenIdentity(ctx context.Context, tokenHash string) (*Identity, error)
}

// Identify resolves the request's credential (session cookie first, then a
// Bearer token) to an *Identity and stores it in the context. It never rejects:
// an unresolved request is simply anonymous. Authorization is enforced
// downstream by RequirePermission.
func Identify(store Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := resolve(r, store)
			next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), id)))
		})
	}
}

func resolve(r *http.Request, store Store) *Identity {
	ctx := r.Context()
	if c, err := r.Cookie(SessionCookieName); err == nil && c.Value != "" {
		if id, err := store.SessionIdentity(ctx, HashSecret(c.Value)); err == nil && id != nil {
			return id
		}
	}
	if raw, ok := bearerToken(r); ok {
		if id, err := store.TokenIdentity(ctx, HashSecret(raw)); err == nil && id != nil {
			return id
		}
	}
	return nil
}

func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):]), true
	}
	return "", false
}

// RequirePermission returns middleware that allows the request only if the
// context identity holds perm: 401 when anonymous, 403 when authenticated but
// lacking the permission.
func RequirePermission(perm string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := FromContext(r.Context())
			if id == nil {
				http.Error(w, "authentication required", http.StatusUnauthorized)
				return
			}
			if !id.Has(perm) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireAnyPermission allows the request if the identity holds ANY of perms:
// 401 when anonymous, 403 when authenticated but holding none of them. (Used by
// the cover-upload routes, which accept either metadata.edit or file.upload; the
// handler then enforces the finer add-only rule for file.upload-only callers.)
func RequireAnyPermission(perms ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := FromContext(r.Context())
			if id == nil {
				http.Error(w, "authentication required", http.StatusUnauthorized)
				return
			}
			for _, p := range perms {
				if id.Has(p) {
					next.ServeHTTP(w, r)
					return
				}
			}
			http.Error(w, "forbidden", http.StatusForbidden)
		})
	}
}
