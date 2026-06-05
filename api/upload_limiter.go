package api

import (
	"errors"
	"sync"
)

// UploadLimiter is a non-blocking concurrency gate for upload requests. It
// tracks in-flight uploads globally and per user, rejecting an Acquire once a
// cap is reached rather than queuing. A zero serverMax/userMax means that
// dimension is unlimited.
//
// There is deliberately no admin bypass: both caps apply to every user (the
// Identity model has no role signal to key one on — see the plan's locked
// decisions). Safe for concurrent use.
type UploadLimiter struct {
	mu          sync.Mutex
	serverMax   int
	userMax     int
	globalCount int
	perUser     map[string]int
}

// NewUploadLimiter returns a limiter with the given global and per-user caps.
// Either cap <= 0 disables that dimension (treated as unlimited).
func NewUploadLimiter(serverMax, userMax int) *UploadLimiter {
	return &UploadLimiter{
		serverMax: serverMax,
		userMax:   userMax,
		perUser:   make(map[string]int),
	}
}

var (
	// ErrServerLimit is returned when the global in-flight cap is reached.
	ErrServerLimit = errors.New("server upload limit reached")
	// ErrUserLimit is returned when the per-user in-flight cap is reached.
	ErrUserLimit = errors.New("user upload limit reached")
)

// Acquire claims an upload slot for userID without blocking. It returns
// ErrServerLimit or ErrUserLimit when the respective cap is already reached, and
// nil on success. A successful Acquire must be paired with exactly one Release.
func (l *UploadLimiter) Acquire(userID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.serverMax > 0 && l.globalCount >= l.serverMax {
		return ErrServerLimit
	}
	if l.userMax > 0 && l.perUser[userID] >= l.userMax {
		return ErrUserLimit
	}
	l.globalCount++
	l.perUser[userID]++
	return nil
}

// Release returns a slot previously claimed via a successful Acquire. It guards
// against underflow so a stray call cannot drive a counter negative, and prunes
// the per-user entry when it reaches zero to keep the map bounded.
func (l *UploadLimiter) Release(userID string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.globalCount > 0 {
		l.globalCount--
	}
	if n := l.perUser[userID]; n > 1 {
		l.perUser[userID] = n - 1
	} else if n == 1 {
		delete(l.perUser, userID)
	}
}
