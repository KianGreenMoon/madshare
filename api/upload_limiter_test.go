package api

import (
	"errors"
	"sync"
	"testing"
)

func TestUploadLimiter_ServerLimit(t *testing.T) {
	l := NewUploadLimiter(1, 0) // global cap 1, per-user unlimited
	if err := l.Acquire("a"); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	// A different user still hits the global cap.
	if err := l.Acquire("b"); !errors.Is(err, ErrServerLimit) {
		t.Errorf("second acquire err = %v, want ErrServerLimit", err)
	}
}

func TestUploadLimiter_UserLimit(t *testing.T) {
	l := NewUploadLimiter(0, 1) // global unlimited, per-user cap 1
	if err := l.Acquire("a"); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if err := l.Acquire("a"); !errors.Is(err, ErrUserLimit) {
		t.Errorf("second acquire (same user) err = %v, want ErrUserLimit", err)
	}
}

func TestUploadLimiter_DifferentUsers(t *testing.T) {
	l := NewUploadLimiter(0, 1)
	if err := l.Acquire("a"); err != nil {
		t.Fatalf("acquire a: %v", err)
	}
	if err := l.Acquire("b"); err != nil {
		t.Errorf("acquire b err = %v, want nil (per-user cap is independent)", err)
	}
}

func TestUploadLimiter_Release(t *testing.T) {
	l := NewUploadLimiter(1, 0)
	if err := l.Acquire("a"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	l.Release("a")
	if err := l.Acquire("a"); err != nil {
		t.Errorf("acquire after release err = %v, want nil", err)
	}
}

// TestUploadLimiter_ReleaseUnderflow verifies a stray Release cannot drive
// counters negative and thereby grant extra slots above the cap.
func TestUploadLimiter_ReleaseUnderflow(t *testing.T) {
	l := NewUploadLimiter(0, 1) // isolate the per-user dimension
	l.Release("a")              // stray release, nothing acquired
	if err := l.Acquire("a"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := l.Acquire("a"); !errors.Is(err, ErrUserLimit) {
		t.Errorf("err = %v, want ErrUserLimit (underflow must not grant extra slots)", err)
	}
}

func TestUploadLimiter_Unlimited(t *testing.T) {
	l := NewUploadLimiter(0, 0)
	for i := 0; i < 1000; i++ {
		if err := l.Acquire("a"); err != nil {
			t.Fatalf("acquire %d err = %v, want nil (unlimited)", i, err)
		}
	}
}

// TestUploadLimiter_Concurrent exercises Acquire/Release under -race and asserts
// the global cap is never exceeded by tracking peak concurrent successes.
func TestUploadLimiter_Concurrent(t *testing.T) {
	const cap = 4
	l := NewUploadLimiter(cap, 0)

	var (
		mu       sync.Mutex
		inFlight int
		peak     int
	)
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			user := string(rune('a' + i%5))
			if err := l.Acquire(user); err != nil {
				return // cap reached — fine
			}
			mu.Lock()
			inFlight++
			if inFlight > peak {
				peak = inFlight
			}
			mu.Unlock()

			mu.Lock()
			inFlight--
			mu.Unlock()
			l.Release(user)
		}(i)
	}
	wg.Wait()

	if peak > cap {
		t.Errorf("peak concurrent acquisitions = %d, want <= %d", peak, cap)
	}
}
