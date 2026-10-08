package web

import (
	"sync"
	"time"
)

// limiter counts failed authentication attempts per key in a fixed window.
type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	m      map[string]*bucket
}

type bucket struct {
	n     int
	start time.Time
}

func newLimiter(max int, window time.Duration) *limiter {
	l := &limiter{max: max, window: window, m: make(map[string]*bucket)}
	go func() {
		for range time.Tick(window) {
			l.gc()
		}
	}()
	return l
}

func (l *limiter) blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.m[key]
	if b == nil {
		return false
	}
	if time.Since(b.start) > l.window {
		delete(l.m, key)
		return false
	}
	return b.n >= l.max
}

// take counts an attempt before it is made, so that a burst of parallel
// requests cannot all pass blocked() before the first failure is recorded.
// It reports false if key is already blocked. Undo it with refund.
func (l *limiter) take(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.m[key]
	if b == nil || time.Since(b.start) > l.window {
		b = &bucket{start: time.Now()}
		l.m[key] = b
	}
	if b.n >= l.max {
		return false
	}
	b.n++
	return true
}

// refund takes back an attempt counted by take.
func (l *limiter) refund(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b := l.m[key]; b != nil && b.n > 0 {
		b.n--
	}
}

func (l *limiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.m[key]
	if b == nil || time.Since(b.start) > l.window {
		b = &bucket{start: time.Now()}
		l.m[key] = b
	}
	b.n++
}

func (l *limiter) reset(key string) {
	l.mu.Lock()
	delete(l.m, key)
	l.mu.Unlock()
}

func (l *limiter) gc() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, b := range l.m {
		if time.Since(b.start) > l.window {
			delete(l.m, k)
		}
	}
}

// codeAlerts counts wrong 2FA codes entered after a correct password. Such
// a failure means someone may know the password, so the user is warned at
// their next login instead of being locked out (which would let anyone who
// knows the password lock the real user out). Kept in memory only.
type codeAlerts struct {
	mu sync.Mutex
	m  map[string]codeAlert
}

type codeAlert struct {
	n     int
	since time.Time
}

func newCodeAlerts() *codeAlerts { return &codeAlerts{m: make(map[string]codeAlert)} }

func (c *codeAlerts) add(user string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.m[user]
	if a.n == 0 {
		a.since = time.Now()
	}
	a.n++
	c.m[user] = a
}

// take returns and clears the failures recorded for user.
func (c *codeAlerts) take(user string) codeAlert {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.m[user]
	delete(c.m, user)
	return a
}

// counter tracks how many operations are running per key.
type counter struct {
	mu sync.Mutex
	m  map[string]int
}

// acquire counts one more operation for key unless max are running.
func (c *counter) acquire(key string, max int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m[key] >= max {
		return false
	}
	c.m[key]++
	return true
}

func (c *counter) release(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m[key]--; c.m[key] <= 0 {
		delete(c.m, key)
	}
}
