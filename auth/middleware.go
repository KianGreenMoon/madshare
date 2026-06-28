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
// Bearer token) to an *Identity and stores it in the context. A request with no
// (or an unknown/expired) credential is simply anonymous; authorization is
// enforced downstream by RequirePermission. But when a credential *is* presented
// and the store lookup fails with a transient error (e.g. a SQLITE_BUSY hiccup),
// it fails closed with 503 rather than silently downgrading to anonymous — the
// latter renders an authenticated user logged-out (and would, mid-session, throw
// them off the admin page on any DB blip).
func Identify(store Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, err := resolve(r, store)
			if err != nil {
				http.Error(w, "authentication temporarily unavailable", http.StatusServiceUnavailable)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), id)))
		})
	}
}

// resolve maps a presented credential to an identity. It returns (nil, nil) for
// no/unknown/expired credential (anonymous), (id, nil) on a hit, and (nil, err)
// when a presented credential's store lookup errors — a transient DB failure the
// caller must surface, not swallow as anonymous. A store error on the cookie
// short-circuits (the token lookup would hit the same ailing DB).
func resolve(r *http.Request, store Store) (*Identity, error) {
	ctx := r.Context()
	if c, err := r.Cookie(SessionCookieName); err == nil && c.Value != "" {
		id, err := store.SessionIdentity(ctx, HashSecret(c.Value))
		if err != nil {
			return nil, err
		}
		if id != nil {
			return id, nil
		}
	}
	if raw, ok := bearerToken(r); ok {
		id, err := store.TokenIdentity(ctx, HashSecret(raw))
		if err != nil {
			return nil, err
		}
		if id != nil {
			return id, nil
		}
	}
	return nil, nil
}

func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):]), true
	}
	return "", false
}

// DenyPasswordChange writes the standard 403 used when an authenticated
// identity must change its password before it may act. The
// X-Password-Change-Required header lets a client distinguish this from an
// ordinary permission denial and route the user to the change-password flow.
func DenyPasswordChange(w http.ResponseWriter) {
	w.Header().Set("X-Password-Change-Required", "1")
	http.Error(w, "password change required", http.StatusForbidden)
}

// RequirePermission returns middleware that allows the request only if the
// context identity holds perm: 401 when anonymous, 403 when authenticated but
// lacking the permission. An identity flagged PasswordChangeRequired is refused
// outright (it must change its password first) — enforced here so every
// capability-gated route inherits it.
func RequirePermission(perm string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := FromContext(r.Context())
			if id == nil {
				http.Error(w, "authentication required", http.StatusUnauthorized)
				return
			}
			if id.PasswordChangeRequired {
				DenyPasswordChange(w)
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
			if id.PasswordChangeRequired {
				DenyPasswordChange(w)
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
