// Package ratelimit implements the non-blocking send limiter that keeps the
// account below WhatsApp's anti-spam thresholds.
package ratelimit

import (
	"sync"
	"time"
)

const window = time.Hour

// Clock abstracts time for tests.
type Clock interface{ Now() time.Time }

// SystemClock is the wall clock.
type SystemClock struct{}

// Now returns the current time.
func (SystemClock) Now() time.Time { return time.Now() }

// Limiter enforces a minimum interval between sends and a cap on sends within
// a trailing one-hour window.
type Limiter struct {
	mu          sync.Mutex
	minInterval time.Duration
	hourlyCap   int
	clock       Clock
	last        time.Time
	sends       []send
	nextID      uint64
}

type send struct {
	at time.Time
	id uint64
}

// Slot identifies one consumed send so that exactly that send can be refunded.
// The zero Slot refunds nothing.
type Slot struct{ id uint64 }

// New builds a Limiter. A nil clock means the system clock.
func New(minInterval time.Duration, hourlyCap int, clock Clock) *Limiter {
	if clock == nil {
		clock = SystemClock{}
	}
	return &Limiter{minInterval: minInterval, hourlyCap: hourlyCap, clock: clock}
}

// Allow consumes a slot when both limits permit it. Otherwise it returns false
// and how long to wait before retrying; it never blocks.
func (l *Limiter) Allow() (ok bool, retryAfter time.Duration) {
	ok, retryAfter, _ = l.Reserve()
	return ok, retryAfter
}

// Reserve is Allow that also returns the consumed Slot, for Refund.
func (l *Limiter) Reserve() (ok bool, retryAfter time.Duration, slot Slot) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.clock.Now()
	l.prune(now)

	if !l.last.IsZero() {
		if wait := l.minInterval - now.Sub(l.last); wait > 0 {
			retryAfter = wait
		}
	}
	if len(l.sends) >= l.hourlyCap {
		wait := window
		if len(l.sends) > 0 {
			wait = l.sends[0].at.Add(window).Sub(now)
		}
		if wait > retryAfter {
			retryAfter = wait
		}
	}
	if retryAfter > 0 {
		return false, retryAfter, Slot{}
	}

	l.nextID++
	l.last = now
	l.sends = append(l.sends, send{at: now, id: l.nextID})
	return true, 0, Slot{id: l.nextID}
}

func (l *Limiter) prune(now time.Time) {
	cutoff := now.Add(-window)
	i := 0
	for i < len(l.sends) && !l.sends[i].at.After(cutoff) {
		i++
	}
	l.sends = l.sends[i:]
}

// Refund gives back exactly the slot a failed send consumed, leaving slots of
// concurrent sends intact. It is a no-op for the zero Slot and for a slot that
// was already refunded or has left the window.
func (l *Limiter) Refund(slot Slot) {
	if slot.id == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	idx := -1
	for i, s := range l.sends {
		if s.id == slot.id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return
	}
	l.sends = append(l.sends[:idx:idx], l.sends[idx+1:]...)
	l.last = time.Time{}
	for _, s := range l.sends {
		if s.at.After(l.last) {
			l.last = s.at
		}
	}
}
