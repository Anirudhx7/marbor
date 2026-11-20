package admin

import (
	"log"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/Anirudhx7/marbor/internal/store"
)

// cacheWindowSize is how many of the most recent request_log rows the cache
// hit figures are computed over. It matches the table's own retention, so the
// window is "everything still on record", and it is always labelled as such.
const cacheWindowSize = 1000

// modelArrow separates the names a logged model can carry: a request log row
// stores "requested -> resolved", or "alias -> requested -> resolved".
const modelArrow = " -> "

// unknownLabel names the group of rows that carry no model or no key name
// (auth off) in the per-model and per-key lists.
const unknownLabel = "-"

// MaxSplitTokens caps one request's prompt or cached token count. A reported
// value above it is not believable for a single request and is treated as not
// reported rather than clamped, so a hostile or buggy upstream cannot skew the
// window sums.
const MaxSplitTokens = store.MaxCacheSplitTokens

// CacheStats is one group's share of the window. HitPct is nil (JSON null)
// when no request in the group reported a cache split, so an unknown rate is
// never shown as 0. CachedTokens and InputTokens are sums over the reporting
// requests only; they are 0 when ReportingRequests is 0, and a reader must
// gate on ReportingRequests before showing them.
type CacheStats struct {
	WindowRequests    int64    `json:"window_requests"`
	ReportingRequests int64    `json:"reporting_requests"`
	CachedTokens      int64    `json:"cached_tokens"`
	InputTokens       int64    `json:"input_tokens"`
	HitPct            *float64 `json:"hit_pct"`
}

// CacheModelStat is a model's cache stats over the window.
type CacheModelStat struct {
	Model string `json:"model"`
	CacheStats
}

// CacheKeyStat is an API key's cache stats over the window. NoKey marks the
// group of requests that carried no key name; its Key is the unknown label,
// which a key literally named the same can never share a bucket with.
type CacheKeyStat struct {
	Key   string `json:"key"`
	NoKey bool   `json:"no_key,omitempty"`
	CacheStats
}

// CacheWindow is the last-N-requests view of prompt-cache reuse. Token sums
// and hit rates cover only the requests that reported a split.
type CacheWindow struct {
	WindowSize        int              `json:"window_size"`
	WindowRequests    int64            `json:"window_requests"`
	ReportingRequests int64            `json:"reporting_requests"`
	CachedTokens      int64            `json:"cached_tokens"`
	InputTokens       int64            `json:"input_tokens"`
	WindowOldest      *time.Time       `json:"window_oldest"`
	ByModel           []CacheModelStat `json:"by_model"`
	ByKey             []CacheKeyStat   `json:"by_key"`
}

// cacheWindowUnavailable is the cache_window_error value sent when the window
// could not be read; the detail goes to the server log only.
const cacheWindowUnavailable = "cache window unavailable"

// resolvedModel returns the model a request was finally served as: the text
// after the last arrow in the logged name.
func resolvedModel(logged string) string {
	if i := strings.LastIndex(logged, modelArrow); i >= 0 {
		return logged[i+len(modelArrow):]
	}
	return logged
}

func satAdd(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// add counts one request and reports whether it carried a split that was
// rejected as inconsistent (a half-set pair counts as inconsistent too; both
// nil is the normal "not reported" and is not).
func (c *CacheStats) add(cached, input *int64) (inconsistent bool) {
	c.WindowRequests++
	if cached == nil && input == nil {
		return false
	}
	// A row counts as reporting only when both values are present and
	// consistent; anything else is treated as not reported.
	if cached == nil || input == nil || *input <= 0 || *cached < 0 || *cached > *input ||
		*input > MaxSplitTokens {
		return true
	}
	c.ReportingRequests++
	c.CachedTokens = satAdd(c.CachedTokens, *cached)
	c.InputTokens = satAdd(c.InputTokens, *input)
	return false
}

func (c *CacheStats) finish() {
	if c.ReportingRequests == 0 || c.InputTokens <= 0 {
		return
	}
	pct := math.Round(float64(c.CachedTokens)*1000/float64(c.InputTokens)) / 10
	c.HitPct = &pct
}

func sortedByRequests[T any](items []T, reqs func(T) int64, name func(T) string) {
	sort.Slice(items, func(i, j int) bool {
		if a, b := reqs(items[i]), reqs(items[j]); a != b {
			return a > b
		}
		return name(items[i]) < name(items[j])
	})
}

// cacheWindow aggregates the most recent request_log rows by resolved model
// and by key. It reads the database rather than the 50-row in-memory ring so
// the window really is the last cacheWindowSize requests, and runs outside the
// server lock. On a store error it returns nil and a short error string, never
// an empty window that would read as "no requests".
func (s *Server) cacheWindow() (*CacheWindow, string) {
	rows, err := s.st.CacheWindowRows(cacheWindowSize)
	if err != nil {
		log.Printf("admin: cache window (last %d requests): %v", cacheWindowSize, err)
		return nil, cacheWindowUnavailable
	}
	out := &CacheWindow{
		WindowSize: cacheWindowSize,
		ByModel:    []CacheModelStat{},
		ByKey:      []CacheKeyStat{},
	}
	type keyID struct {
		name  string
		noKey bool
	}
	byModel := map[string]*CacheStats{}
	byKey := map[keyID]*CacheStats{}
	var total CacheStats
	var dropped int
	for _, row := range rows {
		model := resolvedModel(row.Model)
		if model == "" {
			model = unknownLabel
		}
		key := keyID{name: row.KeyName}
		if key.name == "" {
			key = keyID{name: unknownLabel, noKey: true}
		}
		if byModel[model] == nil {
			byModel[model] = &CacheStats{}
		}
		if byKey[key] == nil {
			byKey[key] = &CacheStats{}
		}
		byModel[model].add(row.CachedTokens, row.InputTokens)
		byKey[key].add(row.CachedTokens, row.InputTokens)
		if total.add(row.CachedTokens, row.InputTokens) {
			dropped++
		}
		if oldest := row.TS; out.WindowOldest == nil || oldest.Before(*out.WindowOldest) {
			t := oldest
			out.WindowOldest = &t
		}
	}
	if dropped > 0 {
		log.Printf("admin: cache window: ignored %d of %d requests with an inconsistent cache split", dropped, len(rows))
	}
	out.WindowRequests = total.WindowRequests
	out.ReportingRequests = total.ReportingRequests
	out.CachedTokens = total.CachedTokens
	out.InputTokens = total.InputTokens
	for name, c := range byModel {
		c.finish()
		out.ByModel = append(out.ByModel, CacheModelStat{Model: name, CacheStats: *c})
	}
	for k, c := range byKey {
		c.finish()
		out.ByKey = append(out.ByKey, CacheKeyStat{Key: k.name, NoKey: k.noKey, CacheStats: *c})
	}
	sortedByRequests(out.ByModel, func(m CacheModelStat) int64 { return m.WindowRequests }, func(m CacheModelStat) string { return m.Model })
	sort.SliceStable(out.ByKey, func(i, j int) bool {
		a, b := out.ByKey[i], out.ByKey[j]
		if a.WindowRequests != b.WindowRequests {
			return a.WindowRequests > b.WindowRequests
		}
		if a.Key != b.Key {
			return a.Key < b.Key
		}
		return !a.NoKey && b.NoKey
	})
	return out, ""
}

// withCacheWindow adds the cache window to a JSON response body: the window
// itself, or null plus cache_window_error when it could not be read.
func (s *Server) withCacheWindow(body map[string]interface{}) map[string]interface{} {
	cw, errMsg := s.cacheWindow()
	body["cache_window"] = cw
	if errMsg != "" {
		body["cache_window_error"] = errMsg
	}
	return body
}
