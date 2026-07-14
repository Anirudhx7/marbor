package proxy

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/store"
)

// setIfAbsent marshals val into m[key] only when key is not already present,
// so a client-specified field (any value, including an explicit null) is
// never overwritten by a configured default.
func setIfAbsent(m map[string]json.RawMessage, key string, val interface{}) {
	if _, exists := m[key]; exists {
		return
	}
	raw, err := json.Marshal(val)
	if err != nil {
		return
	}
	m[key] = raw
}

// injectModelDefaults applies a model's configured default parameters to an
// outgoing request body, filling only fields the client did not already
// specify (never overwrites a client-supplied value). ollamaNative selects
// which wire shape to inject into:
//
//   - Ollama-native (/api/generate, /api/chat): load-time and inference-time
//     params go into the request's "options" object; system/template/keep_alive
//     are top-level fields. Ollama itself detects when a resident model's
//     active options differ from an incoming request's and reloads
//     automatically — the mesh does not need a separate evict-then-reload step.
//   - OpenAI-compatible (/v1/chat/completions, /v1/completions): only the
//     subset of inference-time params that exist in the OpenAI schema are
//     injected at the top level; Ollama-specific knobs (num_ctx, num_gpu,
//     top_k, mirostat, etc.) have no OpenAI equivalent and are skipped here.
//
// Returns the original body unchanged if it isn't a JSON object or the
// config carries no fields worth injecting.
func injectModelDefaults(body []byte, ollamaNative bool, cfg store.ModelConfig) []byte {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return body
	}

	if ollamaNative {
		var opts map[string]json.RawMessage
		if raw, ok := top["options"]; ok {
			_ = json.Unmarshal(raw, &opts)
		}
		if opts == nil {
			opts = map[string]json.RawMessage{}
		}

		// Load-time / engine.
		if cfg.NumCtx != nil {
			setIfAbsent(opts, "num_ctx", *cfg.NumCtx)
		}
		if cfg.NumGPU != nil {
			setIfAbsent(opts, "num_gpu", *cfg.NumGPU)
		}
		if cfg.NumBatch != nil {
			setIfAbsent(opts, "num_batch", *cfg.NumBatch)
		}
		if cfg.NumThread != nil {
			setIfAbsent(opts, "num_thread", *cfg.NumThread)
		}
		if cfg.UseMmap != nil {
			setIfAbsent(opts, "use_mmap", *cfg.UseMmap)
		}
		if cfg.UseMlock != nil {
			setIfAbsent(opts, "use_mlock", *cfg.UseMlock)
		}
		if cfg.RopeFrequencyBase != nil {
			setIfAbsent(opts, "rope_frequency_base", *cfg.RopeFrequencyBase)
		}
		if cfg.RopeFrequencyScale != nil {
			setIfAbsent(opts, "rope_frequency_scale", *cfg.RopeFrequencyScale)
		}
		// flash_attention, offload_kv_cache_to_gpu, and tensor_parallelism are
		// Ollama server/env-level knobs (OLLAMA_FLASH_ATTENTION, etc.), not
		// per-request "options" fields — persisted for operator visibility and
		// for future non-Ollama runtimes, but intentionally not injected here.

		// Inference-time / sampling.
		if cfg.Temperature != nil {
			setIfAbsent(opts, "temperature", *cfg.Temperature)
		}
		if cfg.TopP != nil {
			setIfAbsent(opts, "top_p", *cfg.TopP)
		}
		if cfg.TopK != nil {
			setIfAbsent(opts, "top_k", *cfg.TopK)
		}
		if cfg.MinP != nil {
			setIfAbsent(opts, "min_p", *cfg.MinP)
		}
		if cfg.TypicalP != nil {
			setIfAbsent(opts, "typical_p", *cfg.TypicalP)
		}
		if cfg.TfsZ != nil {
			setIfAbsent(opts, "tfs_z", *cfg.TfsZ)
		}
		if cfg.MaxTokens != nil {
			setIfAbsent(opts, "num_predict", *cfg.MaxTokens)
		}
		if cfg.Seed != nil {
			setIfAbsent(opts, "seed", *cfg.Seed)
		}
		if len(cfg.Stop) > 0 {
			setIfAbsent(opts, "stop", cfg.Stop)
		}
		if cfg.RepeatPenalty != nil {
			setIfAbsent(opts, "repeat_penalty", *cfg.RepeatPenalty)
		}
		if cfg.RepeatLastN != nil {
			setIfAbsent(opts, "repeat_last_n", *cfg.RepeatLastN)
		}
		if cfg.PresencePenalty != nil {
			setIfAbsent(opts, "presence_penalty", *cfg.PresencePenalty)
		}
		if cfg.FrequencyPenalty != nil {
			setIfAbsent(opts, "frequency_penalty", *cfg.FrequencyPenalty)
		}
		if cfg.Mirostat != nil {
			setIfAbsent(opts, "mirostat", *cfg.Mirostat)
		}
		if cfg.MirostatTau != nil {
			setIfAbsent(opts, "mirostat_tau", *cfg.MirostatTau)
		}
		if cfg.MirostatEta != nil {
			setIfAbsent(opts, "mirostat_eta", *cfg.MirostatEta)
		}
		// logit_bias / response_format have no Ollama-native "options" equivalent.

		if len(opts) > 0 {
			if raw, err := json.Marshal(opts); err == nil {
				top["options"] = raw
			}
		}

		if cfg.System != nil {
			setIfAbsent(top, "system", *cfg.System)
		}
		if cfg.Template != nil {
			setIfAbsent(top, "template", *cfg.Template)
		}
		if cfg.TTL != nil {
			setIfAbsent(top, "keep_alive", *cfg.TTL)
		}
	} else {
		// OpenAI-compatible: only fields that exist in that schema.
		if cfg.Temperature != nil {
			setIfAbsent(top, "temperature", *cfg.Temperature)
		}
		if cfg.TopP != nil {
			setIfAbsent(top, "top_p", *cfg.TopP)
		}
		if cfg.MaxTokens != nil {
			setIfAbsent(top, "max_tokens", *cfg.MaxTokens)
		}
		if cfg.Seed != nil {
			setIfAbsent(top, "seed", *cfg.Seed)
		}
		if len(cfg.Stop) > 0 {
			setIfAbsent(top, "stop", cfg.Stop)
		}
		if cfg.PresencePenalty != nil {
			setIfAbsent(top, "presence_penalty", *cfg.PresencePenalty)
		}
		if cfg.FrequencyPenalty != nil {
			setIfAbsent(top, "frequency_penalty", *cfg.FrequencyPenalty)
		}
		if cfg.ResponseFormat != nil {
			setIfAbsent(top, "response_format", map[string]string{"type": *cfg.ResponseFormat})
		}
	}

	out, err := json.Marshal(top)
	if err != nil {
		return body
	}
	return out
}

// modelRateLimiter enforces optional per-model requests-per-minute and
// tokens-per-minute caps (store.ModelConfig.RPM/TPM). Single mesh process, no
// distributed state — an in-memory rolling-minute counter per model, mirroring
// the per-key tokenBucket pattern in internal/auth. RPM is a real pre-request
// gate; TPM is necessarily post-hoc (token counts are only known after a
// response completes), so it blocks new requests once the current minute's
// already-consumed tokens reach the cap — the same "count now, gate later"
// shape used for the existing daily/monthly per-key usage caps.
type modelRateLimiter struct {
	mu    sync.Mutex
	stats map[string]*modelMinuteStats
}

type modelMinuteStats struct {
	windowStart time.Time
	requests    int
	tokens      int64
}

func newModelRateLimiter() *modelRateLimiter {
	return &modelRateLimiter{stats: make(map[string]*modelMinuteStats)}
}

func (l *modelRateLimiter) statsLocked(model string) *modelMinuteStats {
	st, ok := l.stats[model]
	now := time.Now()
	if !ok {
		st = &modelMinuteStats{windowStart: now}
		l.stats[model] = st
		return st
	}
	if now.Sub(st.windowStart) >= time.Minute {
		st.windowStart = now
		st.requests = 0
		st.tokens = 0
	}
	return st
}

// allow reports whether a new request for model may proceed given rpm/tpm
// caps (nil = unlimited), and increments the request counter if so.
func (l *modelRateLimiter) allow(model string, rpm, tpm *int) bool {
	if rpm == nil && tpm == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.statsLocked(model)
	if rpm != nil && *rpm > 0 && st.requests >= *rpm {
		return false
	}
	if tpm != nil && *tpm > 0 && st.tokens >= int64(*tpm) {
		return false
	}
	st.requests++
	return true
}

// recordTokens adds tokens consumed by a completed request into the current
// minute's window, so a subsequent request against the same model sees an
// accurate running total for the TPM gate.
func (l *modelRateLimiter) recordTokens(model string, tokens int64) {
	if tokens <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.statsLocked(model)
	st.tokens += tokens
}
