package store

// OpenAICompatBaseFields are the ModelConfig sampling fields that exist in
// the strict OpenAI chat-completions schema, and are therefore always valid
// for any non-Ollama runtime reached via the OpenAI-compatible wire path
// (vLLM, TGI, llama.cpp).
var OpenAICompatBaseFields = []string{
	"temperature", "top_p", "max_tokens", "seed", "stop",
	"presence_penalty", "frequency_penalty", "response_format",
}

// OpenAICompatExtraFields declares, per non-Ollama runtime, which additional
// ModelConfig sampling fields that runtime's OpenAI-compatible server
// accepts as extra top-level JSON fields beyond OpenAICompatBaseFields. A
// runtime absent from this map, or with an empty slice, only supports the
// base fields above.
//
// Load-time/engine params (num_ctx, num_gpu, flash_attention, etc.) are
// never listed here: on every non-Ollama runtime those are launch-time
// flags, not per-request fields, regardless of wire format — so there's no
// runtime for which they'd belong in this table. This is the single source
// of truth for both request injection (internal/proxy) and the admin API
// that lets the UI show only the fields a model's actual node/runtime
// supports (see Server.handleModelConfigCapabilities).
var OpenAICompatExtraFields = map[string][]string{
	"vllm":     {"top_k", "min_p", "repetition_penalty"},
	"llamacpp": {"mirostat", "mirostat_tau", "mirostat_eta", "repeat_penalty", "repeat_last_n", "tfs_z", "typical_p"},
	"tgi":      {}, // strict OpenAI schema only — TGI's OpenAI layer doesn't accept extras
}

// OllamaLoadTimeFields are ModelConfig fields injected into Ollama's
// per-request "options" object that have no meaning on any other runtime
// (they're launch-time flags everywhere else).
var OllamaLoadTimeFields = []string{
	"num_ctx", "num_gpu", "flash_attention", "offload_kv_cache_to_gpu",
	"num_batch", "num_thread", "use_mmap", "use_mlock",
	"rope_frequency_base", "rope_frequency_scale", "ttl", "tensor_parallelism",
}

// OllamaInferenceFields are the inference-time/sampling ModelConfig fields
// Ollama accepts, beyond OpenAICompatBaseFields (which Ollama also accepts
// under different names via its "options" object, handled directly in
// internal/proxy's Ollama injection branch rather than through this table).
var OllamaInferenceFields = []string{
	"top_k", "min_p", "typical_p", "tfs_z", "repeat_penalty", "repeat_last_n",
	"mirostat", "mirostat_tau", "mirostat_eta", "logit_bias",
}

// SupportedFieldsFor returns every ModelConfig field (by JSON key) that
// actually takes effect when injected for the given runtime. "" is treated
// as "ollama" for backwards compatibility, matching NodeRecord.Runtime's
// convention.
func SupportedFieldsFor(runtime string) []string {
	if runtime == "" || runtime == "ollama" {
		fields := append([]string{}, OpenAICompatBaseFields...)
		fields = append(fields, OllamaLoadTimeFields...)
		fields = append(fields, OllamaInferenceFields...)
		fields = append(fields, "system", "template", "rpm", "tpm")
		return fields
	}
	fields := append([]string{}, OpenAICompatBaseFields...)
	fields = append(fields, OpenAICompatExtraFields[runtime]...)
	// system is supported here too: injectModelDefaults prepends it as a
	// leading {"role":"system",...} message on chat-shaped ("messages"
	// array) requests. template stays Ollama-only — it's Ollama's own
	// model-file prompt-templating mechanism, with no equivalent on any
	// OpenAI-compatible runtime.
	fields = append(fields, "system", "rpm", "tpm")
	return fields
}
