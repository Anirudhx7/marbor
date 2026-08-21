package admin

import (
	"sync"
	"time"

	"github.com/Anirudhx7/marbor/internal/store"
)

// HourlyBucket tracks request counts and costs for one UTC hour.
type HourlyBucket struct {
	Hour     string  `json:"hour"` // "2026-05-23T14" format
	Local    int64   `json:"local"`
	Cloud    int64   `json:"cloud"`
	SavedUSD float64 `json:"saved_usd"`
	SpentUSD float64 `json:"spent_usd"`
	// Tokens and GenDurationMs are raw accumulators for local (Ollama-native)
	// requests, real eval_count/eval_duration parsed from responses.
	// TokensPerSec is derived from them (Tokens / GenDurationMs) only when
	// GenDurationMs > 0 for the hour; otherwise 0, meaning no real
	// generation-time data was available for this hour - never a fabricated
	// rate.
	Tokens        int64   `json:"tokens"`
	GenDurationMs int64   `json:"gen_duration_ms"`
	TokensPerSec  float64 `json:"tokens_per_sec"`
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
		// Hour keys are always written from time.Now().UTC(); a bare
		// time.Parse would interpret them in the host's local zone, skewing
		// this age check by the local UTC offset.
		t, err := time.ParseInLocation("2006-01-02T15", b.Hour, time.UTC)
		if err != nil || now.Sub(t) > hourlyRetention {
			delete(a.hourly, key)
		}
	}
}

// recordLocal records a local request. tokens is the real token count parsed
// from the response; 0 contributes nothing to savings. genDurationMs is
// Ollama's real eval_duration in milliseconds; 0 means unavailable and is
// excluded from the hourly tokens-per-second rollup.
func (a *analyticsStore) recordLocal(model string, tokens, genDurationMs int64) {
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
	a.hourly[key].Tokens += tokens
	a.hourly[key].GenDurationMs += genDurationMs

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

// restoreFromStore backfills the in-memory hourly buckets from persisted
// SQLite records read at startup. It is intended to be called once, before
// the server starts accepting traffic, so the dashboard's traffic chart shows
// continuous history immediately after a restart instead of a gap.
//
// Only hourly buckets are restored. store.HourlyBucket persists a genuine
// Local/Cloud split (LocalRequests/CloudRequests) plus cloud spend (CostUSD),
// so it can be mapped onto admin.HourlyBucket without inventing data  --
// SavedUSD is intentionally left at zero since per-hour local savings are not
// persisted. store.ModelStat, by contrast, only persists an aggregate
// Requests count with no local/cloud split; attributing that total to either
// admin.ModelStat.Local or .Cloud would fabricate a 100%-local or 100%-cloud
// breakdown that never happened, so model stats are deliberately NOT
// backfilled here (see docs/LIMITATIONS.md).
func (a *analyticsStore) restoreFromStore(buckets []store.HourlyBucket) {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Tokens/TokensPerSec are deliberately NOT restored here: store.HourlyBucket.Tokens
	// is a combined local+cloud total (both TrackLocalRequestModel and
	// TrackCloudCostModel add to the same column), so it cannot be attributed
	// to local-only generation without fabricating a split - same reasoning
	// as the SavedUSD omission below. GenDurationMs is local-only by
	// construction but restoring it alone (with Tokens left at 0) would
	// render as a bogus 0 tokens/sec instead of "no data yet"; both are left
	// zero until fresh local traffic repopulates them post-restart.
	for _, b := range buckets {
		key := b.Hour.UTC().Format("2006-01-02T15")
		a.hourly[key] = &HourlyBucket{
			Hour:     key,
			Local:    int64(b.LocalRequests),
			Cloud:    int64(b.CloudRequests),
			SpentUSD: b.CostUSD,
		}
	}
	a.pruneHourlyLocked(time.Now().UTC())
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
		if buckets[i].GenDurationMs > 0 {
			buckets[i].TokensPerSec = float64(buckets[i].Tokens) / (float64(buckets[i].GenDurationMs) / 1000.0)
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
