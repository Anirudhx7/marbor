package admin

import (
	"log"
	"math"
	"sort"
	"strings"
	"time"
)

// cacheWindowSize is how many of the most recent request_log rows the cache
// hit figures are computed over. It matches the table's own retention, so the
// window is "everything still on record", and it is always labelled as such.
const cacheWindowSize = 1000

// modelArrow separates the names a logged model can carry: a request log row
// stores "requested -> resolved", or "alias -> requested -> resolved".
const modelArrow = " -> "

// noKeyLabel groups rows that carry no key name (auth off) in the per-key list.
const noKeyLabel = "-"

// CacheStats is one group's share of the window. HitPct is nil (JSON null)
// when no request in the group reported a cache split, so an unknown rate is
// never shown as 0.
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

// CacheKeyStat is an API key's cache stats over the window.
type CacheKeyStat struct {
	Key string `json:"key"`
	CacheStats
}

// CacheWindow is the last-N-requests view of prompt-cache reuse. Token sums
// and hit rates cover only the requests that reported a split.
type CacheWindow struct {
	WindowSize        int              `json:"window_size"`
	WindowRequests    int64            `json:"window_requests"`
	ReportingRequests int64            `json:"reporting_requests"`
	WindowOldest      *time.Time       `json:"window_oldest"`
	ByModel           []CacheModelStat `json:"by_model"`
	ByKey             []CacheKeyStat   `json:"by_key"`
}

// resolvedModel returns the model a request was finally served as: the text
// after the last arrow in the logged name.
func resolvedModel(logged string) string {
	if i := strings.LastIndex(logged, modelArrow); i >= 0 {
		return logged[i+len(modelArrow):]
	}
	return logged
}

func (c *CacheStats) add(cached, input *int64) {
	c.WindowRequests++
	// A row counts as reporting only when both values are present and
	// consistent; anything else is treated as not reported.
	if cached == nil || input == nil || *input <= 0 || *cached < 0 || *cached > *input {
		return
	}
	c.ReportingRequests++
	c.CachedTokens += *cached
	c.InputTokens += *input
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
// server lock. A store error yields an empty window, never invented numbers.
func (s *Server) cacheWindow() CacheWindow {
	out := CacheWindow{
		WindowSize: cacheWindowSize,
		ByModel:    []CacheModelStat{},
		ByKey:      []CacheKeyStat{},
	}
	rows, err := s.st.CacheWindowRows(cacheWindowSize)
	if err != nil {
		log.Printf("admin: cache window: %v", err)
		return out
	}
	byModel := map[string]*CacheStats{}
	byKey := map[string]*CacheStats{}
	var total CacheStats
	for _, row := range rows {
		model := resolvedModel(row.Model)
		key := row.KeyName
		if key == "" {
			key = noKeyLabel
		}
		if byModel[model] == nil {
			byModel[model] = &CacheStats{}
		}
		if byKey[key] == nil {
			byKey[key] = &CacheStats{}
		}
		byModel[model].add(row.CachedTokens, row.InputTokens)
		byKey[key].add(row.CachedTokens, row.InputTokens)
		total.add(row.CachedTokens, row.InputTokens)
		if oldest := row.TS; out.WindowOldest == nil || oldest.Before(*out.WindowOldest) {
			t := oldest
			out.WindowOldest = &t
		}
	}
	out.WindowRequests = total.WindowRequests
	out.ReportingRequests = total.ReportingRequests
	for name, c := range byModel {
		c.finish()
		out.ByModel = append(out.ByModel, CacheModelStat{Model: name, CacheStats: *c})
	}
	for name, c := range byKey {
		c.finish()
		out.ByKey = append(out.ByKey, CacheKeyStat{Key: name, CacheStats: *c})
	}
	sortedByRequests(out.ByModel, func(m CacheModelStat) int64 { return m.WindowRequests }, func(m CacheModelStat) string { return m.Model })
	sortedByRequests(out.ByKey, func(k CacheKeyStat) int64 { return k.WindowRequests }, func(k CacheKeyStat) string { return k.Key })
	return out
}
