package auth

import (
	"sync"
	"time"
)

// Limiter slows repeated failures of one key (an address, an account), with
// a growing delay: a few attempts are free, then each failure doubles the wait
// before the next attempt, up to a maximum. A success clears the key.
type Limiter struct {
	mu      sync.Mutex
	entries map[string]*attempts
	free    int
	max     time.Duration
	now     func() time.Time
}

type attempts struct {
	failures int
	until    time.Time
	last     time.Time
}

// A key whose last failure is older than this is forgotten.
const limiterMemory = 2 * time.Hour

// NewLimiter returns a limiter: five free failures, at most 15 minutes' wait.
func NewLimiter() *Limiter {
	return &Limiter{entries: map[string]*attempts{}, free: 5, max: 15 * time.Minute, now: time.Now}
}

// Wait returns how long key must wait before its next attempt; 0 if it may
// try now.
func (l *Limiter) Wait(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[key]
	if e == nil {
		return 0
	}
	now := l.now()
	if now.Sub(e.last) > limiterMemory {
		delete(l.entries, key)
		return 0
	}
	if wait := e.until.Sub(now); wait > 0 {
		return wait
	}
	return 0
}

// Fail records a failed attempt of key.
func (l *Limiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.entries) > 100_000 { // a flood of addresses cannot grow it for ever
		for k, e := range l.entries {
			if now.Sub(e.last) > limiterMemory {
				delete(l.entries, k)
			}
		}
	}
	e := l.entries[key]
	if e == nil {
		e = &attempts{}
		l.entries[key] = e
	}
	e.failures++
	e.last = now
	if over := e.failures - l.free; over > 0 {
		wait := l.max
		if over < 12 {
			if d := time.Second << (over - 1); d < l.max {
				wait = d
			}
		}
		e.until = now.Add(wait)
	}
}

// Clear forgets key's failures.
func (l *Limiter) Clear(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}
