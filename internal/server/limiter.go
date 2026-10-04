package server

import (
	"sync"
	"time"
)

// limiter is a fixed-window counter per key (an IP or an email address). It is
// in memory, which is enough for the single console replica of v1.
type limiter struct {
	mu      sync.Mutex
	max     int
	window  time.Duration
	buckets map[string]*bucket
	now     func() time.Time
}

type bucket struct {
	start time.Time
	count int
}

func newLimiter(max int, window time.Duration, now func() time.Time) *limiter {
	return &limiter{max: max, window: window, buckets: map[string]*bucket{}, now: now}
}

// allow counts one attempt for key and reports whether it is within the limit.
func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.buckets) > 10_000 { // drop stale buckets so memory stays bounded
		for k, b := range l.buckets {
			if now.Sub(b.start) >= l.window {
				delete(l.buckets, k)
			}
		}
	}
	b, ok := l.buckets[key]
	if !ok || now.Sub(b.start) >= l.window {
		b = &bucket{start: now}
		l.buckets[key] = b
	}
	b.count++
	return b.count <= l.max
}

// exceeded reports, without counting, whether key used up its window. With
// allow called only on failures, it blocks a key after too many of them.
func (l *limiter) exceeded(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	return ok && l.now().Sub(b.start) < l.window && b.count >= l.max
}
