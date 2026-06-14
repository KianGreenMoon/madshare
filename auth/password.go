// Package auth provides password hashing, session/token secrets, request
// identity, and the authentication/authorization middleware. See
// docs/architecture/auth.md for the overall design.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters. memory is in KiB. These can be raised later without
// breaking existing hashes: the parameters are encoded into each hash string
// and read back at verification time.
const (
	argonTime    = 1
	argonMemory  = 64 * 1024 // 64 MiB
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

// ErrInvalidHash is returned when a stored password hash cannot be parsed.
var ErrInvalidHash = errors.New("auth: invalid password hash format")

// HashPassword returns a self-describing argon2id hash string of the form
// $argon2id$v=19$m=<mem>,t=<time>,p=<threads>$<b64 salt>$<b64 hash>.
func HashPassword(plain string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: read salt: %w", err)
	}
	hash := argon2.IDKey([]byte(plain), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		b64.EncodeToString(salt), b64.EncodeToString(hash)), nil
}

// VerifyPassword reports whether plain matches the encoded argon2id hash. It
// recomputes with the parameters stored in the hash, so hashes made with older
// parameters still verify. The comparison is constant-time.
func VerifyPassword(encoded, plain string) (bool, error) {
	parts := strings.Split(encoded, "$")
	// ["", "argon2id", "v=19", "m=...,t=...,p=...", salt, hash]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, ErrInvalidHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false, ErrInvalidHash
	}
	var mem uint32
	var time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &time, &threads); err != nil {
		return false, ErrInvalidHash
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return false, ErrInvalidHash
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil {
		return false, ErrInvalidHash
	}
	got := argon2.IDKey([]byte(plain), salt, time, mem, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// dummyHash is a fixed argon2id hash, computed once on first use, that no real
// password will ever match. It exists so DummyVerifyPassword can spend the same
// work as a genuine verification.
var dummyHash = sync.OnceValue(func() string {
	h, err := HashPassword("madshare-login-timing-equalizer")
	if err != nil {
		return "" // rand failure: DummyVerifyPassword becomes a no-op
	}
	return h
})

// DummyVerifyPassword runs a throwaway argon2id verification against a fixed
// hash and discards the result. Callers use it on the user-not-found login path
// so a missing username costs the same time as a wrong password — closing the
// timing side channel that would otherwise reveal which usernames exist.
func DummyVerifyPassword(plain string) {
	h := dummyHash()
	if h == "" {
		return
	}
	_, _ = VerifyPassword(h, plain)
}
