package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/metrics"
)

// authAPIError is the OpenAI-compatible error envelope used by auth middleware.
type authAPIError struct {
	Error authAPIErrorBody `json:"error"`
}

type authAPIErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// writeAuthError writes an OpenAI-schema error response from the auth layer.
func writeAuthError(w http.ResponseWriter, status int, message, errType, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(authAPIError{Error: authAPIErrorBody{
		Message: message,
		Type:    errType,
		Code:    code,
	}})
}

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
	mu            sync.RWMutex
	name          string
	key           string
	rateLimit     int
	limiter       *tokenBucket
	counter       *keyCounter
	models        []string
	expiresAt     string
	createdAt     time.Time
	dailyLimit    int
	monthlyLimit  int
	dailyUsdCap   float64
	monthlyUsdCap float64
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
	// Check month/year change FIRST - if the month rolled over, both counters are stale.
	if now.Month() != c.lastReset.Month() || now.Year() != c.lastReset.Year() {
		return 0, 0, 0
	}
	// Day changed within the same month - only today is stale; month survives.
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
	// unlimited disables rate limiting entirely. A configured rate_limit of 0
	// (or negative) means "no per-key request-rate cap", matching the daily/
	// monthly-quota convention where 0 == unlimited. Without this flag a zero
	// rate produced a bucket with capacity 0 and refill rate 0, so allow() could
	// never reach 1 token and every request was rejected with 429 - the exact
	// opposite of the intended "unlimited" semantics.
	unlimited bool
}

func newTokenBucket(ratePerHour int) *tokenBucket {
	if ratePerHour <= 0 {
		return &tokenBucket{unlimited: true, lastRefill: time.Now()}
	}
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
	if tb.unlimited {
		// -1 signals "no limit" to clients rather than a misleading 0/0.
		return -1, -1, 0
	}
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

// retryAfterSeconds returns how many seconds until at least one token is
// available. Minimum 1. Used to populate Retry-After on 429 responses.
func (tb *tokenBucket) retryAfterSeconds() int {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	now := time.Now()
	elapsed := now.Sub(tb.lastRefill).Seconds()
	current := tb.tokens + elapsed*tb.rate
	if current > tb.capacity {
		current = tb.capacity
	}
	if current >= 1 || tb.rate <= 0 {
		return 1
	}
	secs := (1 - current) / tb.rate
	return int(math.Ceil(secs))
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
	if tb.unlimited {
		return true
	}
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
		enabled: cfg.IsEnabled(),
		keys:    make(map[string]*keyState),
		byName:  make(map[string]*keyState),
	}
	for _, k := range cfg.Keys {
		ks := &keyState{
			name:          k.Name,
			key:           k.Key,
			rateLimit:     k.RateLimit,
			limiter:       newTokenBucket(k.RateLimit),
			counter:       &keyCounter{lastReset: time.Now()},
			models:        k.Models,
			expiresAt:     k.ExpiresAt,
			createdAt:     time.Now(),
			dailyLimit:    k.DailyLimit,
			monthlyLimit:  k.MonthlyLimit,
			dailyUsdCap:   k.DailyUsdCap,
			monthlyUsdCap: k.MonthlyUsdCap,
		}
		m.keys[k.Key] = ks
		m.byName[k.Name] = ks
	}
	return m
}

// KeyPatch holds optional runtime-mutable key settings.
// Only non-nil fields are applied; counters are preserved.
type KeyPatch struct {
	RateLimit     *int     `json:"rate_limit"`
	DailyLimit    *int     `json:"daily_limit"`
	MonthlyLimit  *int     `json:"monthly_limit"`
	DailyUsdCap   *float64 `json:"daily_usd_cap"`
	MonthlyUsdCap *float64 `json:"monthly_usd_cap"`
	Models        []string `json:"models"`
	ExpiresAt     *string  `json:"expires_at"`
}

// PatchKey updates mutable fields of an existing key without rotating it.
// Counters and the key token itself are preserved. Returns false if not found.
func (m *Middleware) PatchKey(name string, patch KeyPatch) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	ks, ok := m.byName[name]
	if !ok {
		return false
	}
	ks.mu.Lock()
	if patch.RateLimit != nil {
		ks.rateLimit = *patch.RateLimit
		ks.limiter = newTokenBucket(*patch.RateLimit)
	}
	if patch.DailyLimit != nil {
		ks.dailyLimit = *patch.DailyLimit
	}
	if patch.MonthlyLimit != nil {
		ks.monthlyLimit = *patch.MonthlyLimit
	}
	if patch.DailyUsdCap != nil {
		ks.dailyUsdCap = *patch.DailyUsdCap
	}
	if patch.MonthlyUsdCap != nil {
		ks.monthlyUsdCap = *patch.MonthlyUsdCap
	}
	if patch.Models != nil {
		ks.models = patch.Models
	}
	if patch.ExpiresAt != nil {
		ks.expiresAt = *patch.ExpiresAt
	}
	ks.mu.Unlock()
	return true
}

func (m *Middleware) AddKey(k config.KeyConfig) {
	ks := &keyState{
		name:          k.Name,
		key:           k.Key,
		rateLimit:     k.RateLimit,
		limiter:       newTokenBucket(k.RateLimit),
		counter:       &keyCounter{lastReset: time.Now()},
		models:        k.Models,
		expiresAt:     k.ExpiresAt,
		createdAt:     time.Now(),
		dailyLimit:    k.DailyLimit,
		monthlyLimit:  k.MonthlyLimit,
		dailyUsdCap:   k.DailyUsdCap,
		monthlyUsdCap: k.MonthlyUsdCap,
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

	for _, k := range cfg.Keys {
		existing, sameName := oldByName[k.Name]
		if sameName && existing.key == k.Key {
			// Same key value - preserve counter, update policy fields.
			existing.mu.Lock()
			existing.models = k.Models
			existing.dailyLimit = k.DailyLimit
			existing.monthlyLimit = k.MonthlyLimit
			existing.dailyUsdCap = k.DailyUsdCap
			existing.monthlyUsdCap = k.MonthlyUsdCap
			if k.RateLimit != existing.rateLimit {
				existing.rateLimit = k.RateLimit
				existing.limiter = newTokenBucket(k.RateLimit)
			}
			existing.expiresAt = k.ExpiresAt
			existing.mu.Unlock()
			newKeys[k.Key] = existing
			newByName[k.Name] = existing
		} else {
			// New key or rotated token - fresh state.
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
	m.mu.RUnlock()

	m.mu.Lock()
	m.enabled = cfg.IsEnabled()
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

// KeyUsdCaps returns the live (possibly patched) per-key cloud-spend caps for
// name. This reads from the in-memory key state rather than config.Config,
// because PatchKey mutates only this state - config.Config goes stale after
// a patch. ok is false if no key with this name exists.
func (m *Middleware) KeyUsdCaps(name string) (daily, monthly float64, ok bool) {
	m.mu.RLock()
	ks, found := m.byName[name]
	m.mu.RUnlock()
	if !found {
		return 0, 0, false
	}
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	return ks.dailyUsdCap, ks.monthlyUsdCap, true
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
	ks.mu.RLock()
	limiter := ks.limiter
	ks.mu.RUnlock()
	limiter.refund()
	ks.counter.decrement()
}

func (m *Middleware) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// enabled and keys are read under one RLock/RUnlock pair so the closure
		// never sees enabled from one reload generation alongside keys/byName
		// from another (a torn read across two Reload() swaps).
		m.mu.RLock()
		enabled := m.enabled
		if !enabled {
			// Auth enforcement is off, but still extract the key name from the
			// Authorization header so the request log shows which key was used.
			var ks *keyState
			var ok bool
			if hdr := r.Header.Get("Authorization"); hdr != "" {
				parts := strings.SplitN(hdr, " ", 2)
				if len(parts) == 2 && strings.ToLower(parts[0]) == "bearer" {
					ks, ok = m.keys[parts[1]]
				}
			}
			m.mu.RUnlock()
			if ok {
				r = r.WithContext(context.WithValue(r.Context(), KeyNameContextKey, ks.name))
			}
			next.ServeHTTP(w, r)
			return
		}
		m.mu.RUnlock()
		auth := r.Header.Get("Authorization")
		if auth == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="ollama-mesh"`)
			writeAuthError(w, http.StatusUnauthorized, "missing authorization header", "authentication_error", "missing_auth_header")
			return
		}
		parts := strings.SplitN(auth, " ", 2)
		if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="ollama-mesh"`)
			writeAuthError(w, http.StatusUnauthorized, "invalid authorization format, expected 'Bearer <token>'", "authentication_error", "invalid_auth_format")
			return
		}
		token := parts[1]
		m.mu.RLock()
		ks, ok := m.keys[token]
		m.mu.RUnlock()
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="ollama-mesh"`)
			writeAuthError(w, http.StatusUnauthorized, "invalid api key", "authentication_error", "invalid_api_key")
			return
		}
		ks.mu.RLock()
		expiresAt := ks.expiresAt
		limiter := ks.limiter
		dailyLimit := ks.dailyLimit
		monthlyLimit := ks.monthlyLimit
		modelsList := ks.models
		ks.mu.RUnlock()

		if keyExpired(expiresAt, time.Now()) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="ollama-mesh"`)
			writeAuthError(w, http.StatusUnauthorized, "api key has expired", "authentication_error", "api_key_expired")
			return
		}
		if !limiter.allow() {
			w.Header().Set("Retry-After", fmt.Sprintf("%d", limiter.retryAfterSeconds()))
			writeAuthError(w, http.StatusTooManyRequests, "rate limit exceeded", "insufficient_quota", "rate_limit_exceeded")
			return
		}
		// Expose rate-limit state to callers.
		remaining, capacity, resetAt := limiter.snapshot()
		w.Header().Set("X-RateLimit-Limit", fmt.Sprintf("%d", int64(capacity)))
		w.Header().Set("X-RateLimit-Remaining", fmt.Sprintf("%d", int64(remaining)))
		w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", resetAt))
		// Enforce hard daily/monthly request quotas (0 = unlimited). The Nth
		// allowed request is the limit; the next one is rejected with 429.
		// Increment and read counts atomically to avoid a near-limit TOCTOU.
		today, month := ks.counter.incrementAndStats()
		if dailyLimit > 0 || monthlyLimit > 0 {
			if dailyLimit > 0 && today > dailyLimit {
				ks.counter.decrement()
				limiter.refund()
				metrics.QuotaRejection(ks.name, "daily")
				now := time.Now()
				midnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
				w.Header().Set("X-Quota-Limit", fmt.Sprintf("%d", dailyLimit))
				w.Header().Set("X-Quota-Reset", fmt.Sprintf("%d", midnight.Unix()))
				w.Header().Set("Retry-After", fmt.Sprintf("%d", int(math.Ceil(time.Until(midnight).Seconds()))))
				writeAuthError(w, http.StatusTooManyRequests, "daily request quota exceeded", "insufficient_quota", "daily_quota_exceeded")
				return
			}
			if monthlyLimit > 0 && month > monthlyLimit {
				ks.counter.decrement()
				limiter.refund()
				metrics.QuotaRejection(ks.name, "monthly")
				now := time.Now()
				nextMonth := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, now.Location())
				w.Header().Set("X-Quota-Limit", fmt.Sprintf("%d", monthlyLimit))
				w.Header().Set("X-Quota-Reset", fmt.Sprintf("%d", nextMonth.Unix()))
				w.Header().Set("Retry-After", fmt.Sprintf("%d", int(math.Ceil(time.Until(nextMonth).Seconds()))))
				writeAuthError(w, http.StatusTooManyRequests, "monthly request quota exceeded", "insufficient_quota", "monthly_quota_exceeded")
				return
			}
		}
		ctx := context.WithValue(r.Context(), KeyNameContextKey, ks.name)
		ctx = context.WithValue(ctx, AllowedModelsContextKey, modelsList)
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
	ks.mu.RLock()
	defer ks.mu.RUnlock()
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
