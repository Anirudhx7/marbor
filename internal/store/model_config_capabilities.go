package store

// OpenAICompatBaseFields are the ModelConfig sampling fields that exist in
// the strict OpenAI chat-completions schema, and are therefore always valid
// for any non-Ollama runtime reached via the OpenAI-compatible wire path
// (vLLM, TGI, llama.cpp). Verified against TGI's live OpenAPI schema
// (ChatRequest) as the narrowest of the three - it accepts exactly this set
// and nothing more via its OpenAI-compatible endpoint.
var OpenAICompatBaseFields = []string{
	"temperature", "top_p", "max_tokens", "seed", "stop",
	"presence_penalty", "frequency_penalty", "response_format",
}

// OpenAICompatExtraFields declares, per non-Ollama runtime, which additional
// ModelConfig sampling fields that runtime's OpenAI-compatible server
// accepts as extra top-level JSON fields beyond OpenAICompatBaseFields. A
// runtime absent from this map, or with an empty slice, only supports the
// base fields above. Verified against each runtime's current source/schema
// (not docs, which were found to be stale in places):
//   - vLLM: vllm/entrypoints/openai/chat_completion/protocol.py
//     (ChatCompletionRequest), github.com/vllm-project/vllm main branch.
//   - llama.cpp: tools/server/README.md, which explicitly states its
//     /completion-specific sampling features are also accepted on the
//     OpenAI-compatible endpoints.
//   - TGI: live OpenAPI schema - its ChatRequest genuinely does not declare
//     any of these properties, so it gets none.
//
// Load-time/engine params (num_ctx, num_gpu, etc.) are never listed here: on
// every non-Ollama runtime those are launch-time flags, not per-request
// fields, regardless of wire format - so there's no runtime for which they'd
// belong in this table. This is the single source of truth for both request
// injection (internal/proxy) and the admin API that lets the UI show only
// the fields a model's actual node/runtime supports (see
// Server.handleModelConfigCapabilities).
var OpenAICompatExtraFields = map[string][]string{
	"vllm": {
		"top_k", "min_p", "repetition_penalty",
		"length_penalty", "stop_token_ids", "include_stop_str_in_output",
		"ignore_eos", "min_tokens", "skip_special_tokens", "truncate_prompt_tokens",
	},
	"llamacpp": {
		"repeat_penalty", "repeat_last_n", "typical_p", "mirostat", "mirostat_tau", "mirostat_eta",
		"num_keep", "logit_bias", "n_probs", "min_keep",
		"dry_multiplier", "dry_base", "dry_allowed_length", "dry_penalty_last_n",
		"xtc_probability", "xtc_threshold", "ignore_eos",
	},
	"tgi": {}, // strict OpenAI schema only - TGI's OpenAI layer doesn't accept extras
	"mlx": {
		"top_k", "min_p", "repetition_penalty", "logit_bias",
	}, // verified against mlx-lm/mlx_lm/SERVER.md (github.com/ml-explore/mlx-lm,
	// main branch): the /v1/chat/completions request fields documented there
	// beyond strict OpenAI include top_k, min_p, repetition_penalty (plus its
	// repetition_context_size, not currently a ModelConfig field), and
	// logit_bias.
}

// OpenAICompatUnsupportedBaseFields declares, per non-Ollama runtime, which
// OpenAICompatBaseFields that runtime's OpenAI-compatible server does NOT
// actually accept, despite being in the strict-schema baseline assumed valid
// everywhere else. A runtime absent from this map supports the full base set.
var OpenAICompatUnsupportedBaseFields = map[string][]string{
	"mlx": {"seed", "response_format"}, // verified against mlx-lm/mlx_lm/SERVER.md -
	// its documented request fields include no "seed" and no "response_format"/
	// structured-output equivalent.
}

// OllamaLoadTimeFields are ModelConfig fields injected into Ollama's
// per-request "options" object that have no meaning on any other runtime
// (they're launch-time flags everywhere else, or don't exist elsewhere at
// all). Verified against Ollama's current api/types.go Options/Runner
// structs (github.com/ollama/ollama) - flash_attention,
// offload_kv_cache_to_gpu, rope_frequency_base/scale, use_mlock, and
// tensor_parallelism are deliberately absent: none of them are real
// per-request (or even current Modelfile) parameters anymore.
var OllamaLoadTimeFields = []string{
	"num_ctx", "num_gpu", "main_gpu", "num_batch", "num_thread", "use_mmap", "draft_num_predict", "ttl",
}

// OllamaInferenceFields are the inference-time/sampling ModelConfig fields
// Ollama accepts, beyond OpenAICompatBaseFields (which Ollama also accepts
// under different names via its "options" object, handled directly in
// internal/proxy's Ollama injection branch rather than through this table).
// mirostat/mirostat_tau/mirostat_eta and tfs_z were removed from this list:
// they do not exist in Ollama's current Options struct (confirmed against
// source, not older docs/blog posts describing a previous version that
// wrapped llama.cpp's full option set directly) - they remain valid
// ModelConfig fields for llama.cpp, just no longer injected for Ollama.
var OllamaInferenceFields = []string{
	"top_k", "min_p", "typical_p", "num_keep", "repeat_penalty", "repeat_last_n",
}

// SupportedFieldsFor returns every ModelConfig field (by JSON key) that
// actually takes effect when injected for the given runtime. "" is treated
// as "ollama" for backwards compatibility, matching NodeRecord.Runtime's
// convention.
func SupportedFieldsFor(runtime string) []string {
	if runtime == "" || runtime == "ollama" {
		// Built explicitly rather than reusing OpenAICompatBaseFields wholesale
		// (P136): that slice includes response_format, but Ollama has no
		// native "options" equivalent for it and internal/proxy's Ollama
		// injection branch deliberately never sets it (see model_config.go's
		// comment beside its options-building code) - advertising it here
		// showed operators a UI control for Ollama-resident models that
		// silently did nothing.
		fields := make([]string, 0, len(OpenAICompatBaseFields))
		for _, f := range OpenAICompatBaseFields {
			if f != "response_format" {
				fields = append(fields, f)
			}
		}
		fields = append(fields, OllamaLoadTimeFields...)
		fields = append(fields, OllamaInferenceFields...)
		fields = append(fields, "system", "template", "rpm", "tpm")
		return fields
	}
	fields := make([]string, 0, len(OpenAICompatBaseFields))
	unsupported := OpenAICompatUnsupportedBaseFields[runtime]
	for _, f := range OpenAICompatBaseFields {
		skip := false
		for _, u := range unsupported {
			if f == u {
				skip = true
				break
			}
		}
		if !skip {
			fields = append(fields, f)
		}
	}
	fields = append(fields, OpenAICompatExtraFields[runtime]...)
	// system is supported here too: injectModelDefaults prepends it as a
	// leading {"role":"system",...} message on chat-shaped ("messages"
	// array) requests. template stays Ollama-only - it's Ollama's own
	// model-file prompt-templating mechanism, with no equivalent on any
	// OpenAI-compatible runtime.
	fields = append(fields, "system", "rpm", "tpm")
	return fields
}
