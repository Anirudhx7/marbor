package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
)

type contextKey string

const KeyNameContextKey contextKey = "key_name"

type Middleware struct {
	mu      sync.RWMutex
	enabled bool
	keys    map[string]*keyState
	byName  map[string]*keyState
}

type keyState struct {
	name      string
	key       string
	rateLimit int
	limiter   *tokenBucket
	counter   *keyCounter
	models    []string
	expiresAt string
	createdAt time.Time
}

type keyCounter struct {
	mu        sync.Mutex
	today     int
	month     int
	lastReset time.Time
}

func (c *keyCounter) increment() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	// Reset daily counter if day changed
	if now.Day() != c.lastReset.Day() || now.Month() != c.lastReset.Month() || now.Year() != c.lastReset.Year() {
		c.today = 0
	}
	// Reset monthly counter if month changed
	if now.Month() != c.lastReset.Month() || now.Year() != c.lastReset.Year() {
		c.month = 0
	}
	c.lastReset = now
	c.today++
	c.month++
}

func (c *keyCounter) stats() (today, month int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if now.Day() != c.lastReset.Day() || now.Month() != c.lastReset.Month() || now.Year() != c.lastReset.Year() {
		return 0, c.month
	}
	if now.Month() != c.lastReset.Month() || now.Year() != c.lastReset.Year() {
		return 0, 0
	}
	return c.today, c.month
}

type tokenBucket struct {
	mu         sync.Mutex
	tokens     float64
	capacity   float64
	lastRefill time.Time
	rate       float64
}

func newTokenBucket(ratePerHour int) *tokenBucket {
	rate := float64(ratePerHour) / 3600.0
	return &tokenBucket{
		tokens:     float64(ratePerHour),
		capacity:   float64(ratePerHour),
		lastRefill: time.Now(),
		rate:       rate,
	}
}

// snapshot returns the current token count, capacity, and approximate Unix
// timestamp when the bucket will be fully refilled. Callers must not hold tb.mu.
func (tb *tokenBucket) snapshot() (remaining float64, capacity float64, resetAt int64) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	now := time.Now()
	elapsed := now.Sub(tb.lastRefill).Seconds()
	current := tb.tokens + elapsed*tb.rate
	if current > tb.capacity {
		current = tb.capacity
	}
	// Seconds until full = (capacity - current) / rate
	var secsUntilFull float64
	if tb.rate > 0 {
		secsUntilFull = (tb.capacity - current) / tb.rate
	}
	if secsUntilFull < 0 {
		secsUntilFull = 0
	}
	reset := now.Add(time.Duration(secsUntilFull * float64(time.Second))).Unix()
	return current, tb.capacity, reset
}

func (tb *tokenBucket) allow() bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	now := time.Now()
	elapsed := now.Sub(tb.lastRefill).Seconds()
	tb.tokens += elapsed * tb.rate
	if tb.tokens > tb.capacity {
		tb.tokens = tb.capacity
	}
	tb.lastRefill = now
	if tb.tokens >= 1 {
		tb.tokens--
		return true
	}
	return false
}

func NewMiddleware(cfg config.AuthConfig) *Middleware {
	m := &Middleware{enabled: cfg.Enabled}
	if cfg.Enabled {
		m.keys = make(map[string]*keyState)
		m.byName = make(map[string]*keyState)
		for _, k := range cfg.Keys {
			ks := &keyState{
				name:      k.Name,
				key:       k.Key,
				rateLimit: k.RateLimit,
				limiter:   newTokenBucket(k.RateLimit),
				counter:   &keyCounter{lastReset: time.Now()},
				models:    k.Models,
				expiresAt: k.ExpiresAt,
				createdAt: time.Now(),
			}
			m.keys[k.Key] = ks
			m.byName[k.Name] = ks
		}
	}
	return m
}

func (m *Middleware) AddKey(k config.KeyConfig) {
	ks := &keyState{
		name:      k.Name,
		key:       k.Key,
		rateLimit: k.RateLimit,
		limiter:   newTokenBucket(k.RateLimit),
		counter:   &keyCounter{lastReset: time.Now()},
		models:    k.Models,
		expiresAt: k.ExpiresAt,
		createdAt: time.Now(),
	}
	m.mu.Lock()
	m.keys[k.Key] = ks
	m.byName[k.Name] = ks
	m.mu.Unlock()
}

func (m *Middleware) RevokeKey(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ks, ok := m.byName[name]
	if !ok {
		return
	}
	delete(m.keys, ks.key)
	delete(m.byName, name)
}

func (m *Middleware) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !m.enabled {
			next.ServeHTTP(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		if auth == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"missing authorization header"}`))
			return
		}
		parts := strings.SplitN(auth, " ", 2)
		if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"invalid authorization format"}`))
			return
		}
		token := parts[1]
		m.mu.RLock()
		ks, ok := m.keys[token]
		m.mu.RUnlock()
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"invalid api key"}`))
			return
		}
		if !ks.limiter.allow() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":"rate limit exceeded"}`))
			return
		}
		// Expose rate-limit state to callers.
		remaining, capacity, resetAt := ks.limiter.snapshot()
		w.Header().Set("X-RateLimit-Limit", fmt.Sprintf("%d", int64(capacity)))
		w.Header().Set("X-RateLimit-Remaining", fmt.Sprintf("%d", int64(remaining)))
		w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", resetAt))
		ks.counter.increment()
		ctx := context.WithValue(r.Context(), KeyNameContextKey, ks.name)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func KeyNameFromContext(ctx context.Context) string {
	v, _ := ctx.Value(KeyNameContextKey).(string)
	return v
}

func (m *Middleware) KeyStats(name string) (today, month int, models []string, expiresAt string, rateLimit int, createdAt time.Time, ok bool) {
	m.mu.RLock()
	ks, ok := m.byName[name]
	m.mu.RUnlock()
	if !ok {
		return 0, 0, nil, "", 0, time.Time{}, false
	}
	today, month = ks.counter.stats()
	return today, month, ks.models, ks.expiresAt, ks.rateLimit, ks.createdAt, true
}

func (m *Middleware) AllKeyNames() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.byName))
	for name := range m.byName {
		out = append(out, name)
	}
	return out
}
