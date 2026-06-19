package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/metrics"
)

type contextKey string

const KeyNameContextKey contextKey = "key_name"

// AllowedModelsContextKey carries the calling key's allowed-models list so the
// proxy can enforce per-key model restrictions after extracting the model name.
const AllowedModelsContextKey contextKey = "allowed_models"

type Middleware struct {
	mu      sync.RWMutex
	enabled bool
	keys    map[string]*keyState
	byName  map[string]*keyState
}

type keyState struct {
	name         string
	key          string
	rateLimit    int
	limiter      *tokenBucket
	counter      *keyCounter
	models       []string
	expiresAt    string
	createdAt    time.Time
	dailyLimit   int
	monthlyLimit int
}

type keyCounter struct {
	mu          sync.Mutex
	today       int
	month       int
	tokensToday int64
	tokensMonth int64
	lastReset   time.Time
}

// resetLocked zeroes whichever counters have gone stale relative to now.
// Caller must hold c.mu. Month rollover clears both day and month buckets;
// a day change within the same month clears only the day buckets.
func (c *keyCounter) resetLocked(now time.Time) {
	if now.Month() != c.lastReset.Month() || now.Year() != c.lastReset.Year() {
		c.today, c.month = 0, 0
		c.tokensToday, c.tokensMonth = 0, 0
		return
	}
	if now.Day() != c.lastReset.Day() {
		c.today = 0
		c.tokensToday = 0
	}
}

func (c *keyCounter) increment() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	c.resetLocked(now)
	c.lastReset = now
	c.today++
	c.month++
}

// incrementAndStats bumps the request counters and returns the post-increment
// day/month counts atomically, so the quota check is a single critical section
// (increment() followed by a separate stats() was a TOCTOU that could miscount
// near the limit under concurrent requests).
func (c *keyCounter) incrementAndStats() (today, month int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	c.resetLocked(now)
	c.lastReset = now
	c.today++
	c.month++
	return c.today, c.month
}

// decrement reverses one increment(), used to refund a request that was
// rejected by policy before it reached a node. Clamps at zero.
func (c *keyCounter) decrement() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.today > 0 {
		c.today--
	}
	if c.month > 0 {
		c.month--
	}
}

// addTokens accumulates token usage for the current day and month.
func (c *keyCounter) addTokens(n int64) {
	if n <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	c.resetLocked(now)
	c.lastReset = now
	c.tokensToday += n
	c.tokensMonth += n
}

func (c *keyCounter) stats() (today, month int, tokensMonth int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	// Check month/year change FIRST — if the month rolled over, both counters are stale.
	if now.Month() != c.lastReset.Month() || now.Year() != c.lastReset.Year() {
		return 0, 0, 0
	}
	// Day changed within the same month — only today is stale; month survives.
	if now.Day() != c.lastReset.Day() {
		return 0, c.month, c.tokensMonth
	}
	return c.today, c.month, c.tokensMonth
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

// refund returns one token to the bucket (capped at capacity), reversing an
// allow() for a request that was rejected by policy before reaching a node.
func (tb *tokenBucket) refund() {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	tb.tokens++
	if tb.tokens > tb.capacity {
		tb.tokens = tb.capacity
	}
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
	m := &Middleware{
		enabled: cfg.Enabled,
		keys:    make(map[string]*keyState),
		byName:  make(map[string]*keyState),
	}
	for _, k := range cfg.Keys {
		ks := &keyState{
			name:         k.Name,
			key:          k.Key,
			rateLimit:    k.RateLimit,
			limiter:      newTokenBucket(k.RateLimit),
			counter:      &keyCounter{lastReset: time.Now()},
			models:       k.Models,
			expiresAt:    k.ExpiresAt,
			createdAt:    time.Now(),
			dailyLimit:   k.DailyLimit,
			monthlyLimit: k.MonthlyLimit,
		}
		m.keys[k.Key] = ks
		m.byName[k.Name] = ks
	}
	return m
}

func (m *Middleware) AddKey(k config.KeyConfig) {
	ks := &keyState{
		name:         k.Name,
		key:          k.Key,
		rateLimit:    k.RateLimit,
		limiter:      newTokenBucket(k.RateLimit),
		counter:      &keyCounter{lastReset: time.Now()},
		models:       k.Models,
		expiresAt:    k.ExpiresAt,
		createdAt:    time.Now(),
		dailyLimit:   k.DailyLimit,
		monthlyLimit: k.MonthlyLimit,
	}
	m.mu.Lock()
	m.keys[k.Key] = ks
	m.byName[k.Name] = ks
	m.mu.Unlock()
}

// Reload atomically replaces the key set from a new config. Keys whose name
// AND token value are unchanged have their counter and rate-limiter state
// preserved (request counts survive reload). Keys with a rotated token value
// start fresh (old token stops working immediately). Removed keys stop
// accepting requests after the swap.
func (m *Middleware) Reload(cfg config.AuthConfig) {
	newKeys := make(map[string]*keyState, len(cfg.Keys))
	newByName := make(map[string]*keyState, len(cfg.Keys))

	m.mu.RLock()
	oldByName := m.byName
	m.mu.RUnlock()

	for _, k := range cfg.Keys {
		existing, sameName := oldByName[k.Name]
		if sameName && existing.key == k.Key {
			// Same key value — preserve counter + limiter, update policy fields.
			existing.models = k.Models
			existing.dailyLimit = k.DailyLimit
			existing.monthlyLimit = k.MonthlyLimit
			existing.rateLimit = k.RateLimit
			existing.expiresAt = k.ExpiresAt
			newKeys[k.Key] = existing
			newByName[k.Name] = existing
		} else {
			// New key or rotated token — fresh state.
			ks := &keyState{
				name:         k.Name,
				key:          k.Key,
				rateLimit:    k.RateLimit,
				limiter:      newTokenBucket(k.RateLimit),
				counter:      &keyCounter{lastReset: time.Now()},
				models:       k.Models,
				expiresAt:    k.ExpiresAt,
				createdAt:    time.Now(),
				dailyLimit:   k.DailyLimit,
				monthlyLimit: k.MonthlyLimit,
			}
			newKeys[k.Key] = ks
			newByName[k.Name] = ks
		}
	}

	m.mu.Lock()
	m.enabled = cfg.Enabled
	m.keys = newKeys
	m.byName = newByName
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

// Refund restores one request's rate-limit token and quota count for a key,
// used when the proxy rejects the request by policy (model allow-list) before
// it reaches a node, so a disallowed request never burns the key's budget.
// No-op for an unknown key name (e.g. auth disabled).
func (m *Middleware) Refund(name string) {
	if name == "" {
		return
	}
	m.mu.RLock()
	ks, ok := m.byName[name]
	m.mu.RUnlock()
	if !ok {
		return
	}
	ks.limiter.refund()
	ks.counter.decrement()
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
		// Enforce key expiry before consuming any rate-limit/quota budget. The
		// expires_at field was loaded and surfaced in the UI but never checked, so
		// expired keys authenticated forever.
		if keyExpired(ks.expiresAt, time.Now()) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"api key expired"}`))
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
		// Enforce hard daily/monthly request quotas (0 = unlimited). The Nth
		// allowed request is the limit; the next one is rejected with 429.
		// Increment and read counts atomically to avoid a near-limit TOCTOU.
		today, month := ks.counter.incrementAndStats()
		if ks.dailyLimit > 0 || ks.monthlyLimit > 0 {
			if ks.dailyLimit > 0 && today > ks.dailyLimit {
				ks.counter.decrement()
				ks.limiter.refund()
				metrics.QuotaRejection(ks.name, "daily")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				w.Write([]byte(`{"error":"daily quota exceeded"}`))
				return
			}
			if ks.monthlyLimit > 0 && month > ks.monthlyLimit {
				ks.counter.decrement()
				ks.limiter.refund()
				metrics.QuotaRejection(ks.name, "monthly")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				w.Write([]byte(`{"error":"monthly quota exceeded"}`))
				return
			}
		}
		ctx := context.WithValue(r.Context(), KeyNameContextKey, ks.name)
		ctx = context.WithValue(ctx, AllowedModelsContextKey, ks.models)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// keyExpired reports whether an API key's expires_at has passed. Empty means no
// expiry. Accepts a date ("2006-01-02", valid through the end of that day) or a
// full RFC3339 timestamp. An unparseable value is treated as non-expiring so a
// config typo never silently locks out a working key (it is validated at load).
func keyExpired(expiresAt string, now time.Time) bool {
	if expiresAt == "" {
		return false
	}
	if t, err := time.Parse("2006-01-02", expiresAt); err == nil {
		return now.After(t.Add(24 * time.Hour))
	}
	if t, err := time.Parse(time.RFC3339, expiresAt); err == nil {
		return now.After(t)
	}
	return false
}

func KeyNameFromContext(ctx context.Context) string {
	v, _ := ctx.Value(KeyNameContextKey).(string)
	return v
}

// AllowedModelsFromContext returns the calling key's allowed-models list.
// An empty/nil slice means no restriction (all models allowed).
func AllowedModelsFromContext(ctx context.Context) []string {
	v, _ := ctx.Value(AllowedModelsContextKey).([]string)
	return v
}

func (m *Middleware) KeyStats(name string) (today, month int, tokensMonth int64, models []string, expiresAt string, rateLimit int, createdAt time.Time, ok bool) {
	m.mu.RLock()
	ks, ok := m.byName[name]
	m.mu.RUnlock()
	if !ok {
		return 0, 0, 0, nil, "", 0, time.Time{}, false
	}
	today, month, tokensMonth = ks.counter.stats()
	return today, month, tokensMonth, ks.models, ks.expiresAt, ks.rateLimit, ks.createdAt, true
}

// AddKeyTokens accumulates token usage against a key (by name). No-op for
// unknown keys or non-positive counts.
func (m *Middleware) AddKeyTokens(name string, n int64) {
	if n <= 0 {
		return
	}
	m.mu.RLock()
	ks, ok := m.byName[name]
	m.mu.RUnlock()
	if ok {
		ks.counter.addTokens(n)
	}
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
