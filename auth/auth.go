package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// Permission strings. These are the Layer-A capabilities (see
// docs/architecture/auth.md §4.1); roles are bundles of them.
const (
	PermUserManage       = "user.manage"
	PermRoleManage       = "role.manage"
	PermFileUpload       = "file.upload"
	PermFileDelete       = "file.delete"
	PermMetadataEdit     = "metadata.edit"
	PermLibraryShare     = "library.share"
	PermFederationManage = "federation.manage"
	PermContentPlay      = "content.play"
	PermContentDownload  = "content.download"
	PermContentAll       = "content.all"
)

// SessionCookieName is the cookie carrying the opaque session token.
const SessionCookieName = "madshare_session"

// Identity is the authenticated principal resolved from a session or token.
// A nil *Identity in the request context means the request is anonymous.
type Identity struct {
	UserID                 int64
	Username               string
	Permissions            map[string]bool
	PasswordChangeRequired bool
}

// Has reports whether the identity holds the given permission.
func (i *Identity) Has(perm string) bool {
	if i == nil {
		return false
	}
	return i.Permissions[perm]
}

type ctxKey struct{}

// WithIdentity returns a context carrying id (which may be nil for anonymous).
func WithIdentity(ctx context.Context, id *Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromContext returns the identity stored in ctx, or nil if anonymous/unset.
func FromContext(ctx context.Context) *Identity {
	id, _ := ctx.Value(ctxKey{}).(*Identity)
	return id
}

// GenerateSecret returns a URL-safe random secret (raw, to hand to the client)
// and is used for both session and API-token values.
func GenerateSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: generate secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashSecret returns the hex SHA-256 of a session or token value. Only the hash
// is stored, so a database leak does not expose live credentials.
func HashSecret(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
