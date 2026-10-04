// Package ratelimit is a token bucket per key, in memory: how many requests
// one client address, or one invite token, may make. It bounds what a public
// endpoint costs — guessing is hopeless against 32 random bytes, the
// database and Keycloak behind it are not — per replica; a deployment with
// several replicas allows that many times as much.
package ratelimit

import (
	"sync"
	"time"
)

// maxKeys bounds the memory a flood of distinct keys can take: past it,
// buckets that are full again are dropped, and a key the limiter has not
// seen is refused until there is room.
const maxKeys = 100_000

// Limiter allows Burst requests at once per key, and one more every Every.
type Limiter struct {
	Burst int
	Every time.Duration
	// now is the clock; time.Now unless a test says otherwise.
	now func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
	swept   time.Time
}

type bucket struct {
	tokens float64
	at     time.Time
}

// New is a limiter of burst requests at once, refilled one every every.
func New(burst int, every time.Duration) *Limiter {
	return &Limiter{Burst: burst, Every: every, now: time.Now, buckets: map[string]*bucket{}}
}

// WithClock is the limiter on a clock of a test's.
func (l *Limiter) WithClock(now func() time.Time) *Limiter {
	l.now = now
	return l
}

// Allow takes one request from key's bucket; false, and how long until the
// next one is allowed, when it is empty.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.buckets) >= maxKeys || now.Sub(l.swept) > time.Minute {
		l.sweep(now)
	}
	b := l.buckets[key]
	if b == nil {
		if len(l.buckets) >= maxKeys {
			return false, l.Every
		}
		b = &bucket{tokens: float64(l.Burst), at: now}
		l.buckets[key] = b
	}
	b.tokens += now.Sub(b.at).Seconds() / l.Every.Seconds()
	if b.tokens > float64(l.Burst) {
		b.tokens = float64(l.Burst)
	}
	b.at = now
	if b.tokens < 1 {
		wait := time.Duration((1 - b.tokens) * float64(l.Every))
		return false, wait
	}
	b.tokens--
	return true, 0
}

// sweep drops the buckets that are full again: a key seen next is as new.
func (l *Limiter) sweep(now time.Time) {
	full := time.Duration(l.Burst) * l.Every
	for k, b := range l.buckets {
		if now.Sub(b.at) >= full {
			delete(l.buckets, k)
		}
	}
	l.swept = now
}
