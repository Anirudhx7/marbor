package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/metrics"
)

// hashToken maps a raw API key token to its lookup key in m.keys. Indexing by
// digest instead of the raw token means lookup timing depends only on the
// digest, never on how many leading bytes of the presented token matched a
// real key - closing the H6 timing side channel without an O(n) scan over
// every configured key.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

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

	// expiryWarned remembers when each key last logged a malformed-expiry
	// warning (key name -> time.Time), so a bad value warns once a minute
	// instead of once per request.
	expiryWarned sync.Map

	// expiryMu guards expiryHook and expiryAudited. It is a leaf lock: it is
	// never held while calling the hook or while taking m.mu or a keyState lock.
	expiryMu sync.Mutex
	// expiryHook, when set, is told once per key name and malformed value that a
	// stored expires_at cannot be parsed. See SetExpiryAuditHook.
	expiryHook func(keyName string)
	// expiryAudited remembers which (key name, malformed value) pairs were already
	// reported, for the life of the process.
	expiryAudited map[malformedExpiry]struct{}
}

// malformedExpiry identifies one malformed stored expiry for de-duplication.
// Only a SHA-256 digest of the value is kept, so the raw string is never
// retained, logged or passed to the hook.
type malformedExpiry struct {
	name   string
	digest string
}

type keyState struct {
	mu                    sync.RWMutex
	name                  string
	key                   string
	rateLimit             int
	limiter               *tokenBucket
	counter               *keyCounter
	models                []string
	expiresAt             string
	createdAt             time.Time
	dailyLimit            int
	monthlyLimit          int
	dailyUsdCap           float64
	monthlyUsdCap         float64
	localOnly             bool
	allowLocalDegradation bool
}

// refundLimiter refunds one token to whichever *tokenBucket ks.limiter names
// at the moment the write lock is held - unlike reading ks.limiter under
// RLock and calling refund() after releasing it, this can never race
// PatchKey's swap of ks.limiter to a fresh bucket: PatchKey.RateLimit also
// takes ks.mu.Lock() to install the new bucket, so the read here and that
// swap can never interleave.
func (ks *keyState) refundLimiter() {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	ks.limiter.refund()
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
			name:                  k.Name,
			key:                   k.Key,
			rateLimit:             k.RateLimit,
			limiter:               newTokenBucket(k.RateLimit),
			counter:               &keyCounter{lastReset: time.Now()},
			models:                k.Models,
			expiresAt:             k.ExpiresAt,
			createdAt:             time.Now(),
			dailyLimit:            k.DailyLimit,
			monthlyLimit:          k.MonthlyLimit,
			dailyUsdCap:           k.DailyUsdCap,
			monthlyUsdCap:         k.MonthlyUsdCap,
			localOnly:             k.LocalOnly,
			allowLocalDegradation: k.AllowLocalDegradation,
		}
		m.keys[hashToken(k.Key)] = ks
		m.byName[k.Name] = ks
	}
	return m
}

// KeyPatch holds optional runtime-mutable key settings.
// Only non-nil fields are applied; counters are preserved.
type KeyPatch struct {
	RateLimit             *int     `json:"rate_limit"`
	DailyLimit            *int     `json:"daily_limit"`
	MonthlyLimit          *int     `json:"monthly_limit"`
	DailyUsdCap           *float64 `json:"daily_usd_cap"`
	MonthlyUsdCap         *float64 `json:"monthly_usd_cap"`
	Models                []string `json:"models"`
	ExpiresAt             *string  `json:"expires_at"`
	LocalOnly             *bool    `json:"local_only"`
	AllowLocalDegradation *bool    `json:"allow_local_degradation"`
}

// PatchKey updates mutable fields of an existing key without rotating it.
// Counters and the key token itself are preserved. Returns false if not found.
func (m *Middleware) PatchKey(name string, patch KeyPatch) bool {
	ok, found := m.patchKey(name, patch)
	// The audit hook runs after every lock is released.
	m.reportMalformedExpiries(found)
	return ok
}

func (m *Middleware) patchKey(name string, patch KeyPatch) (bool, []malformedExpiry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ks, ok := m.byName[name]
	if !ok {
		return false, nil
	}
	var found []malformedExpiry
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
		found = collectMalformed(found, name, ks.expiresAt)
	}
	if patch.LocalOnly != nil {
		ks.localOnly = *patch.LocalOnly
	}
	if patch.AllowLocalDegradation != nil {
		ks.allowLocalDegradation = *patch.AllowLocalDegradation
	}
	ks.mu.Unlock()
	return true, found
}

func (m *Middleware) AddKey(k config.KeyConfig) {
	ks := &keyState{
		name:                  k.Name,
		key:                   k.Key,
		rateLimit:             k.RateLimit,
		limiter:               newTokenBucket(k.RateLimit),
		counter:               &keyCounter{lastReset: time.Now()},
		models:                k.Models,
		expiresAt:             k.ExpiresAt,
		createdAt:             time.Now(),
		dailyLimit:            k.DailyLimit,
		monthlyLimit:          k.MonthlyLimit,
		dailyUsdCap:           k.DailyUsdCap,
		monthlyUsdCap:         k.MonthlyUsdCap,
		localOnly:             k.LocalOnly,
		allowLocalDegradation: k.AllowLocalDegradation,
	}
	m.mu.Lock()
	// A name collision with a different token value is a rotation - drop the
	// old token from m.keys so it stops authenticating immediately, rather
	// than staying valid alongside the new one until the next Reload/restart
	// (B1 ADMIN-02: a compromised key "rotated" this way must not survive).
	if existing, ok := m.byName[k.Name]; ok && existing.key != k.Key {
		delete(m.keys, hashToken(existing.key))
	}
	m.keys[hashToken(k.Key)] = ks
	m.byName[k.Name] = ks
	m.mu.Unlock()
	m.reportMalformedExpiries(collectMalformed(nil, k.Name, k.ExpiresAt))
}

// Reload atomically replaces the key set from a new config. Keys whose name
// AND token value are unchanged have their counter and rate-limiter state
// preserved (request counts survive reload). Keys with a rotated token value
// start fresh (old token stops working immediately). Removed keys stop
// accepting requests after the swap.
func (m *Middleware) Reload(cfg config.AuthConfig) {
	// The audit hook runs after every lock is released.
	m.reportMalformedExpiries(m.swapKeys(cfg))
}

// swapKeys installs the new key set under m.mu (released by defer, so a panic
// cannot leave it held) and returns the malformed expiries it saw.
func (m *Middleware) swapKeys(cfg config.AuthConfig) []malformedExpiry {
	newKeys := make(map[string]*keyState, len(cfg.Keys))
	newByName := make(map[string]*keyState, len(cfg.Keys))

	var found []malformedExpiry
	m.mu.Lock()
	defer m.mu.Unlock()
	oldByName := m.byName

	for _, k := range cfg.Keys {
		found = collectMalformed(found, k.Name, k.ExpiresAt)
		existing, sameName := oldByName[k.Name]
		if sameName && existing.key == k.Key {
			// Same key value - preserve counter, update policy fields.
			existing.mu.Lock()
			existing.models = k.Models
			existing.dailyLimit = k.DailyLimit
			existing.monthlyLimit = k.MonthlyLimit
			existing.dailyUsdCap = k.DailyUsdCap
			existing.monthlyUsdCap = k.MonthlyUsdCap
			existing.localOnly = k.LocalOnly
			existing.allowLocalDegradation = k.AllowLocalDegradation
			if k.RateLimit != existing.rateLimit {
				existing.rateLimit = k.RateLimit
				existing.limiter = newTokenBucket(k.RateLimit)
			}
			existing.expiresAt = k.ExpiresAt
			existing.mu.Unlock()
			newKeys[hashToken(k.Key)] = existing
			newByName[k.Name] = existing
		} else {
			// New key or rotated token - fresh state.
			ks := &keyState{
				name:                  k.Name,
				key:                   k.Key,
				rateLimit:             k.RateLimit,
				limiter:               newTokenBucket(k.RateLimit),
				counter:               &keyCounter{lastReset: time.Now()},
				models:                k.Models,
				expiresAt:             k.ExpiresAt,
				createdAt:             time.Now(),
				dailyLimit:            k.DailyLimit,
				monthlyLimit:          k.MonthlyLimit,
				dailyUsdCap:           k.DailyUsdCap,
				monthlyUsdCap:         k.MonthlyUsdCap,
				localOnly:             k.LocalOnly,
				allowLocalDegradation: k.AllowLocalDegradation,
			}
			newKeys[hashToken(k.Key)] = ks
			newByName[k.Name] = ks
		}
	}
	m.enabled = cfg.IsEnabled()
	m.keys = newKeys
	m.byName = newByName
	return found
}

func (m *Middleware) RevokeKey(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ks, ok := m.byName[name]
	if !ok {
		return
	}
	delete(m.keys, hashToken(ks.key))
	delete(m.byName, name)
	m.expiryWarned.Delete(name)
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

// IsLocalOnly reports whether name's live (possibly patched) policy forbids
// cloud fallback - a request that would otherwise spill to a cloud provider
// must instead fail closed. Returns false for an unknown key name: an
// unrecognized/anonymous request was never cloud-gated by this check, so it
// fails open to today's existing behavior rather than blocking it.
//
// Locking follows the same discipline as KeyUsdCaps above: Middleware.mu is
// held only briefly to look up the key, then released before keyState.mu is
// taken - never the reverse, and never held across a call into
// internal/store. This is a pure in-memory read with no store lock at all,
// so it is a strict subset of KeyUsdCaps'/CloudBudgetExceeded's existing
// lock ordering, not a new hierarchy.
func (m *Middleware) IsLocalOnly(name string) bool {
	m.mu.RLock()
	ks, found := m.byName[name]
	m.mu.RUnlock()
	if !found {
		return false
	}
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	return ks.localOnly
}

// IsAllowLocalDegradation reports whether name's live (possibly patched)
// policy permits this key's requests to be substituted with an
// operator-declared local alternate model (routing.local_degradation_chains)
// before cloud fallback, when the requested model has no available node.
// Returns false for an unknown key name: an unrecognized/anonymous request
// was never allowed to degrade, so this preserves that fail-safe behavior.
//
// Locking follows the same discipline as IsLocalOnly above: Middleware.mu is
// held only briefly to look up the key, then released before keyState.mu is
// taken - never the reverse, and never held across a call into
// internal/store.
func (m *Middleware) IsAllowLocalDegradation(name string) bool {
	m.mu.RLock()
	ks, found := m.byName[name]
	m.mu.RUnlock()
	if !found {
		return false
	}
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	return ks.allowLocalDegradation
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
	ks.refundLimiter()
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
					ks, ok = m.keys[hashToken(parts[1])]
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
			w.Header().Set("WWW-Authenticate", `Bearer realm="marbor"`)
			writeAuthError(w, http.StatusUnauthorized, "missing authorization header", "authentication_error", "missing_auth_header")
			return
		}
		parts := strings.SplitN(auth, " ", 2)
		if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="marbor"`)
			writeAuthError(w, http.StatusUnauthorized, "invalid authorization format, expected 'Bearer <token>'", "authentication_error", "invalid_auth_format")
			return
		}
		token := parts[1]
		m.mu.RLock()
		ks, ok := m.keys[hashToken(token)]
		m.mu.RUnlock()
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="marbor"`)
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

		expired, malformed := ExpiryStatus(expiresAt, time.Now())
		if malformed {
			m.warnMalformedExpiry(ks.name)
		}
		if expired || malformed {
			w.Header().Set("WWW-Authenticate", `Bearer realm="marbor"`)
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

// ExpiryAuditAction is the system-audit action recorded when a key's stored
// expiry is found to be malformed.
const ExpiryAuditAction = "api_key_expiry_malformed"

// ExpiryAuditDetails is the plain-language detail for that audit event. It names
// no key value and no raw expiry string.
const ExpiryAuditDetails = "The stored expiry for this API key is malformed, so the key is rejected until the expiry is corrected."

// collectMalformed appends (name, expiresAt) to found when expiresAt is a
// non-empty value ExpiryStatus cannot parse.
func collectMalformed(found []malformedExpiry, name, expiresAt string) []malformedExpiry {
	if _, malformed := ExpiryStatus(expiresAt, time.Now()); malformed {
		return append(found, malformedExpiry{name: name, digest: hashToken(expiresAt)})
	}
	return found
}

// SetExpiryAuditHook registers fn to be called with the key name the first time
// a given key carries a given malformed stored expires_at, whether found at
// load, on Reload, AddKey or PatchKey. Setting the hook also scans the keys
// already loaded, so a malformed expiry present before the hook was wired is
// reported too. Each (key name, malformed value) pair is reported at most once
// per process; a new malformed value for the same key is reported again. fn is
// never called per request and never while a Middleware lock is held, so it may
// call back into the Middleware. The hook receives only the key name. A nil fn
// disables reporting.
func (m *Middleware) SetExpiryAuditHook(fn func(keyName string)) {
	m.expiryMu.Lock()
	m.expiryHook = fn
	m.expiryMu.Unlock()
	if fn == nil {
		return
	}
	var found []malformedExpiry
	m.mu.RLock()
	for name, ks := range m.byName {
		ks.mu.RLock()
		found = collectMalformed(found, name, ks.expiresAt)
		ks.mu.RUnlock()
	}
	m.mu.RUnlock()
	m.reportMalformedExpiries(found)
}

// reportMalformedExpiries invokes the audit hook for each entry not yet
// reported. Callers must hold no Middleware or keyState lock.
func (m *Middleware) reportMalformedExpiries(found []malformedExpiry) {
	if len(found) == 0 {
		return
	}
	m.expiryMu.Lock()
	hook := m.expiryHook
	var fresh []string
	if hook != nil {
		if m.expiryAudited == nil {
			m.expiryAudited = make(map[malformedExpiry]struct{})
		}
		for _, f := range found {
			if _, seen := m.expiryAudited[f]; seen {
				continue
			}
			m.expiryAudited[f] = struct{}{}
			fresh = append(fresh, f.name)
		}
	}
	m.expiryMu.Unlock()
	for _, name := range fresh {
		hook(name)
	}
}

// expiryWarnInterval bounds how often a key with a malformed expires_at logs
// its warning.
const expiryWarnInterval = time.Minute

// warnMalformedExpiry logs, at most once per expiryWarnInterval per key, that a
// key is being rejected because its persisted expires_at cannot be parsed. Only
// the key name is logged, never the key value.
func (m *Middleware) warnMalformedExpiry(name string) {
	now := time.Now()
	if last, ok := m.expiryWarned.Load(name); ok && now.Sub(last.(time.Time)) < expiryWarnInterval {
		return
	}
	m.expiryWarned.Store(name, now)
	log.Printf("auth: rejecting API key %q: its stored expires_at is malformed and is treated as expired; fix it with PATCH /admin/keys/%s (expires_at)", name, url.PathEscape(name))
}

// ExpiryStatus classifies an API key's expires_at. Empty means no expiry
// (neither expired nor malformed). A bare date ("2006-01-02") is valid through
// the end of that day, a datetime-local value ("2006-01-02T15:04") is read in
// now's location, and an RFC3339 timestamp is exact. Any other non-empty value
// is malformed: callers must treat it as expired, never as non-expiring, so a
// corrupted or hand-edited value cannot extend a credential indefinitely.
// Create and update reject malformed values up front (admin.validateExpiresAt);
// this covers values already persisted.
func ExpiryStatus(expiresAt string, now time.Time) (expired, malformed bool) {
	if expiresAt == "" {
		return false, false
	}
	if t, err := time.ParseInLocation("2006-01-02", expiresAt, now.Location()); err == nil {
		return now.After(t.AddDate(0, 0, 1)), false
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04", expiresAt, now.Location()); err == nil {
		return now.After(t), false
	}
	if t, err := time.Parse(time.RFC3339, expiresAt); err == nil {
		return now.After(t), false
	}
	return false, true
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
