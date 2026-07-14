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
// specify (never overwrites a client-supplied value). runtime selects which
// wire shape to inject into ("" is treated as "ollama" for backwards
// compatibility, matching router.NodeState.Runtime's convention):
//
//   - Ollama (/api/generate, /api/chat): load-time and inference-time
//     params go into the request's "options" object; system/template/keep_alive
//     are top-level fields. Ollama itself detects when a resident model's
//     active options differ from an incoming request's and reloads
//     automatically — the mesh does not need a separate evict-then-reload step.
//   - Every other runtime (vllm/tgi/llamacpp, reached via /v1/chat/completions,
//     /v1/completions): the subset of inference-time params that exist in the
//     strict OpenAI schema are always injected at the top level, plus
//     whatever additional fields store.OpenAICompatExtraFields[runtime]
//     declares that specific runtime's server actually accepts. Ollama-only
//     load-time knobs (num_ctx, num_gpu, top_k via Ollama's own naming,
//     mirostat, etc.) have no equivalent outside Ollama's options object and
//     are skipped unless explicitly re-declared (under the runtime's own
//     field name) in store.OpenAICompatExtraFields.
//
// Returns the original body unchanged if it isn't a JSON object or the
// config carries no fields worth injecting.
func injectModelDefaults(body []byte, runtime string, cfg store.ModelConfig) []byte {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return body
	}

	if runtime == "" || runtime == "ollama" {
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
		// Strict OpenAI schema — always valid regardless of which non-Ollama
		// runtime is on the other end.
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

		// Extra fields specific to this runtime's own OpenAI-compatible
		// server extensions, beyond the strict schema above.
		for _, field := range store.OpenAICompatExtraFields[runtime] {
			switch field {
			case "top_k":
				if cfg.TopK != nil {
					setIfAbsent(top, "top_k", *cfg.TopK)
				}
			case "min_p":
				if cfg.MinP != nil {
					setIfAbsent(top, "min_p", *cfg.MinP)
				}
			case "repetition_penalty": // vLLM's own name for repeat_penalty
				if cfg.RepeatPenalty != nil {
					setIfAbsent(top, "repetition_penalty", *cfg.RepeatPenalty)
				}
			case "repeat_penalty": // llama.cpp's own name for the same value
				if cfg.RepeatPenalty != nil {
					setIfAbsent(top, "repeat_penalty", *cfg.RepeatPenalty)
				}
			case "repeat_last_n":
				if cfg.RepeatLastN != nil {
					setIfAbsent(top, "repeat_last_n", *cfg.RepeatLastN)
				}
			case "tfs_z":
				if cfg.TfsZ != nil {
					setIfAbsent(top, "tfs_z", *cfg.TfsZ)
				}
			case "typical_p":
				if cfg.TypicalP != nil {
					setIfAbsent(top, "typical_p", *cfg.TypicalP)
				}
			case "mirostat":
				if cfg.Mirostat != nil {
					setIfAbsent(top, "mirostat", *cfg.Mirostat)
				}
			case "mirostat_tau":
				if cfg.MirostatTau != nil {
					setIfAbsent(top, "mirostat_tau", *cfg.MirostatTau)
				}
			case "mirostat_eta":
				if cfg.MirostatEta != nil {
					setIfAbsent(top, "mirostat_eta", *cfg.MirostatEta)
				}
			}
		}
	}

	out, err := json.Marshal(top)
	if err != nil {
		return body
	}
	return out
}

// modelRateLimiter enforces optional per-(model,node) requests-per-minute
// and tokens-per-minute caps (store.ModelConfig.RPM/TPM). Single mesh
// process, no distributed state — an in-memory rolling-minute counter per
// (model, node) pair, mirroring the per-key tokenBucket pattern in
// internal/auth. Keyed by node as well as model since ModelConfig itself is
// now a per-(model,node) profile: the same model on two different nodes can
// carry two different rpm/tpm caps. RPM is a real pre-request gate; TPM is
// necessarily post-hoc (token counts are only known after a response
// completes), so it blocks new requests once the current minute's
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

// limiterKey combines model+node into the map key. NUL is used as the
// separator since it can never appear in either a model name or node name.
func limiterKey(model, node string) string {
	return model + "\x00" + node
}

func (l *modelRateLimiter) statsLocked(key string) *modelMinuteStats {
	st, ok := l.stats[key]
	now := time.Now()
	if !ok {
		st = &modelMinuteStats{windowStart: now}
		l.stats[key] = st
		return st
	}
	if now.Sub(st.windowStart) >= time.Minute {
		st.windowStart = now
		st.requests = 0
		st.tokens = 0
	}
	return st
}

// allow reports whether a new request for (model, node) may proceed given
// rpm/tpm caps (nil = unlimited), and increments the request counter if so.
func (l *modelRateLimiter) allow(model, node string, rpm, tpm *int) bool {
	if rpm == nil && tpm == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.statsLocked(limiterKey(model, node))
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
// minute's window, so a subsequent request against the same (model, node)
// sees an accurate running total for the TPM gate.
func (l *modelRateLimiter) recordTokens(model, node string, tokens int64) {
	if tokens <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.statsLocked(limiterKey(model, node))
	st.tokens += tokens
}
