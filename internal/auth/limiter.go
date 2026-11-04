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

// NewLimiterFor returns a limiter with its own number of free failures, and longest wait.
func NewLimiterFor(free int, longest time.Duration) *Limiter {
	return &Limiter{entries: map[string]*attempts{}, free: free, max: longest, now: time.Now}
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

// Take says how long to wait, if at all, and otherwise counts an attempt, in one step: done apart, many at once
// would all find that they need not wait, and get far more tries than the limit is there to give. Return gives
// back an attempt that turned out not to have failed.
func (l *Limiter) Take(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if e := l.entries[key]; e != nil && now.Sub(e.last) <= limiterMemory {
		if wait := e.until.Sub(now); wait > 0 {
			return wait
		}
	}
	l.fail(key, now)
	return 0
}

// Return gives back an attempt that Take counted.
func (l *Limiter) Return(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[key]
	if e == nil {
		return
	}
	if e.failures--; e.failures <= l.free {
		e.until = time.Time{}
	}
}

// Fail records a failed attempt of key.
func (l *Limiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fail(key, l.now())
}

func (l *Limiter) fail(key string, now time.Time) {
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
