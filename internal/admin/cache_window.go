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

// CacheModelStat is a model's cache stats over the window. NoModel marks the
// group of requests that carried no model name; its Model is the unknown label,
// which a model literally named the same can never share a bucket with.
type CacheModelStat struct {
	Model   string `json:"model"`
	NoModel bool   `json:"no_model,omitempty"`
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

// CacheWindow is the last-N-requests view of prompt-cache reuse. Its embedded
// CacheStats is the whole window (token sums and hit rate cover only the
// requests that reported a split); the by-model and by-key lists break it down.
type CacheWindow struct {
	WindowSize   int        `json:"window_size"`
	WindowOldest *time.Time `json:"window_oldest"`
	CacheStats
	ByModel []CacheModelStat `json:"by_model"`
	ByKey   []CacheKeyStat   `json:"by_key"`
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

// add counts one request. A row counts as reporting only when both values are
// present and form a valid split; anything else is treated as not reported.
func (c *CacheStats) add(input, cached *int64) {
	c.WindowRequests++
	if !store.ValidCacheSplit(input, cached) {
		return
	}
	c.ReportingRequests++
	c.CachedTokens += *cached
	c.InputTokens += *input
}

// finish sets HitPct once at least one request reported a split. Sums cannot
// overflow: each value is capped at store.MaxCacheSplitTokens and the window
// holds at most cacheWindowSize rows.
func (c *CacheStats) finish() {
	if c.ReportingRequests == 0 {
		return
	}
	pct := math.Round(float64(c.CachedTokens)*1000/float64(c.InputTokens)) / 10
	c.HitPct = &pct
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
	type groupID struct {
		name    string
		unknown bool
	}
	byModel := map[groupID]*CacheStats{}
	byKey := map[groupID]*CacheStats{}
	for _, row := range rows {
		model := groupID{name: resolvedModel(row.Model)}
		if model.name == "" {
			model = groupID{name: unknownLabel, unknown: true}
		}
		key := groupID{name: row.KeyName}
		if key.name == "" {
			key = groupID{name: unknownLabel, unknown: true}
		}
		if byModel[model] == nil {
			byModel[model] = &CacheStats{}
		}
		if byKey[key] == nil {
			byKey[key] = &CacheStats{}
		}
		byModel[model].add(row.InputTokens, row.CachedTokens)
		byKey[key].add(row.InputTokens, row.CachedTokens)
		out.add(row.InputTokens, row.CachedTokens)
		if oldest := row.TS; out.WindowOldest == nil || oldest.Before(*out.WindowOldest) {
			t := oldest
			out.WindowOldest = &t
		}
	}
	out.finish()
	for g, c := range byModel {
		c.finish()
		out.ByModel = append(out.ByModel, CacheModelStat{Model: g.name, NoModel: g.unknown, CacheStats: *c})
	}
	for g, c := range byKey {
		c.finish()
		out.ByKey = append(out.ByKey, CacheKeyStat{Key: g.name, NoKey: g.unknown, CacheStats: *c})
	}
	sort.SliceStable(out.ByModel, func(i, j int) bool {
		a, b := out.ByModel[i], out.ByModel[j]
		if a.WindowRequests != b.WindowRequests {
			return a.WindowRequests > b.WindowRequests
		}
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		return !a.NoModel && b.NoModel
	})
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
