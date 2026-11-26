package router

// Model-format buckets understood by FormatServable. A bucket describes how a
// model reference has to be fetched and what weight format it implies, using
// only what the reference string itself proves.
const (
	// FormatOllamaLibrary is a bare "name[:tag]" with no slash: Ollama's own
	// library shorthand. Only `ollama pull` resolves it.
	FormatOllamaLibrary = "ollama-library"
	// FormatGGUFHF is an "hf.co/..." reference or a ".gguf" file name: the
	// convention Ollama and llama.cpp use to load a GGUF quant.
	FormatGGUFHF = "gguf-hf"
	// FormatHFRepo is a bare "org/repo" Hugging Face identifier. It is
	// deliberately never judged: by shape it is ambiguous with a namespaced
	// Ollama community tag, and the string alone does not say whether the
	// weights are safetensors, AWQ/GPTQ or MLX.
	FormatHFRepo = "hf-repo"
)

// FormatServable reports whether a node running runtime can load a model of the
// given format bucket. This is the single runtime x format matrix; the admin
// pull block and advisor read it through here so they cannot drift apart.
//
// known is false when the matrix has no confident answer (the hf-repo bucket,
// FP8, EXL3, safetensors, an empty or unrecognised format). In that case ok is
// always true: an unknown combination must never exclude a node.
//
// For the two known buckets an empty runtime means Ollama, matching the rest
// of the code base:
//   - ollama-library loads on {"", ollama} only.
//   - gguf-hf loads on {"", ollama, llamacpp}. vLLM can load GGUF only as an
//     upstream-experimental path, TGI and MLX do not, so all three are treated
//     as not servable.
//
// The function is pure and holds no state, so it is safe for concurrent use.
func FormatServable(runtime, format string) (ok, known bool) {
	switch format {
	case FormatOllamaLibrary:
		return runtime == "" || runtime == "ollama", true
	case FormatGGUFHF:
		switch runtime {
		case "", "ollama", "llamacpp":
			return true, true
		default:
			return false, true
		}
	default:
		return true, false
	}
}
