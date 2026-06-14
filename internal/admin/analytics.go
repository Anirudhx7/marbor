package admin

import (
	"sync"
	"time"
)

// HourlyBucket tracks request counts and costs for one UTC hour.
type HourlyBucket struct {
	Hour     string  `json:"hour"` // "2026-05-23T14" format
	Local    int64   `json:"local"`
	Cloud    int64   `json:"cloud"`
	SavedUSD float64 `json:"saved_usd"`
	SpentUSD float64 `json:"spent_usd"`
}

// ModelStat tracks aggregate stats per model.
type ModelStat struct {
	Model    string  `json:"model"`
	Local    int64   `json:"local"`
	Cloud    int64   `json:"cloud"`
	SavedUSD float64 `json:"saved_usd"`
}

// analyticsStore holds in-memory hourly buckets and per-model stats.
// All reads/writes protected by mu.
type analyticsStore struct {
	mu           sync.RWMutex
	refCostPer1K float64                  // reference cloud rate for valuing local tokens (immutable)
	hourly       map[string]*HourlyBucket // key = "2006-01-02T15"
	byModel      map[string]*ModelStat
}

func newAnalyticsStore(refCostPer1K float64) *analyticsStore {
	return &analyticsStore{
		refCostPer1K: refCostPer1K,
		hourly:       make(map[string]*HourlyBucket),
		byModel:      make(map[string]*ModelStat),
	}
}

// hourlyRetention bounds how long hourly buckets are kept. The dashboard only
// reads the last 24h; 48h gives margin while keeping the map from growing
// without bound on a long-lived process.
const hourlyRetention = 48 * time.Hour

// pruneHourlyLocked deletes hourly buckets older than the retention window.
// Caller must hold a.mu. cutoff is "now" in UTC.
func (a *analyticsStore) pruneHourlyLocked(now time.Time) {
	if len(a.hourly) <= 48 {
		return
	}
	for key, b := range a.hourly {
		t, err := time.Parse("2006-01-02T15", b.Hour)
		if err != nil || now.Sub(t) > hourlyRetention {
			delete(a.hourly, key)
		}
	}
}

// recordLocal records a local request. tokens is the real token count parsed
// from the response; 0 contributes nothing to savings.
func (a *analyticsStore) recordLocal(model string, tokens int64) {
	now := time.Now().UTC()
	key := now.Format("2006-01-02T15")
	saved := a.refCostPer1K * float64(tokens) / 1000.0

	a.mu.Lock()
	defer a.mu.Unlock()
	a.pruneHourlyLocked(now)

	if a.hourly[key] == nil {
		a.hourly[key] = &HourlyBucket{Hour: key}
	}
	a.hourly[key].Local++
	a.hourly[key].SavedUSD += saved

	if a.byModel[model] == nil {
		a.byModel[model] = &ModelStat{Model: model}
	}
	a.byModel[model].Local++
	a.byModel[model].SavedUSD += saved
}

// recordCloud records a cloud request. tokens is the real token count parsed
// from the provider response; 0 contributes nothing to spend.
func (a *analyticsStore) recordCloud(model string, costPer1K float64, tokens int64) {
	now := time.Now().UTC()
	key := now.Format("2006-01-02T15")
	spent := costPer1K * float64(tokens) / 1000.0

	a.mu.Lock()
	defer a.mu.Unlock()
	a.pruneHourlyLocked(now)

	if a.hourly[key] == nil {
		a.hourly[key] = &HourlyBucket{Hour: key}
	}
	a.hourly[key].Cloud++
	a.hourly[key].SpentUSD += spent

	if a.byModel[model] == nil {
		a.byModel[model] = &ModelStat{Model: model}
	}
	a.byModel[model].Cloud++
}

// last24hBuckets returns 24 ordered hourly buckets (oldest first).
// Missing hours are returned as zero-value buckets.
func (a *analyticsStore) last24hBuckets() []HourlyBucket {
	now := time.Now().UTC()
	buckets := make([]HourlyBucket, 24)
	a.mu.RLock()
	defer a.mu.RUnlock()
	for i := 0; i < 24; i++ {
		t := now.Add(time.Duration(i-23) * time.Hour)
		key := t.Format("2006-01-02T15")
		if b, ok := a.hourly[key]; ok {
			buckets[i] = *b
		} else {
			buckets[i] = HourlyBucket{Hour: key}
		}
	}
	return buckets
}

// topModels returns all model stats sorted by total requests descending.
func (a *analyticsStore) topModels() []ModelStat {
	a.mu.RLock()
	defer a.mu.RUnlock()
	stats := make([]ModelStat, 0, len(a.byModel))
	for _, v := range a.byModel {
		stats = append(stats, *v)
	}
	// Sort by total requests descending (bubble sort - small N)
	for i := 0; i < len(stats); i++ {
		for j := i + 1; j < len(stats); j++ {
			ti := stats[i].Local + stats[i].Cloud
			tj := stats[j].Local + stats[j].Cloud
			if tj > ti {
				stats[i], stats[j] = stats[j], stats[i]
			}
		}
	}
	return stats
}
