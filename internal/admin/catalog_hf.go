package admin

// catalogHFRepo is the Hugging Face equivalent of one curated catalog model
// for the safetensors runtimes (vLLM and TGI). The catalog itself holds
// Ollama-library tags, which those runtimes cannot load, so each mapped model
// offers exactly one full-precision repo instead of its Ollama variants.
type catalogHFRepo struct {
	Repo string // Hugging Face repo id, org/name
	// SizeMB is the approximate total of the repo's .safetensors files in MiB,
	// typed here at curation time (like the Ollama sizes in catalogModels) so
	// the catalog handler never calls Hugging Face. 0 means unknown: the row
	// shows "-" and is never picked.
	SizeMB int64
	Quant  string // BF16 or FP16, the dtype the repo's weights are stored in
}

// hfListContextTokens is the context length the catalog list sizes a
// Hugging Face row against. The list has no config.json, so it uses the
// estimated (linear) path of computeContextFeasibility at this typical
// length; the repo detail view can differ because it reads real architecture
// facts.
const hfListContextTokens = 8192

// catalogHFRepos maps a catalogModels Name to its Hugging Face equivalent.
// A model absent here (and present in catalogHFUnmapped) keeps showing as
// another format on vLLM and TGI nodes.
//
// Licence rule, the single criterion for this table: a repo is mapped only if
// Hugging Face reports it as not gated (TestCatalogHFRepos_Live proves it,
// plus the presence of safetensors weights). Licence text is not a criterion.
// A repo that is gated is unmapped with reason "gated", whatever its family. A
// model whose repo was not checked is unmapped with reason "unverified" until a
// curated edit maps it (the Mistral and Mixtral instruct repos report not gated
// but are left out until someone confirms the choice of repo).
var catalogHFRepos = map[string]catalogHFRepo{
	"qwen2.5:7b":        {Repo: "Qwen/Qwen2.5-7B-Instruct", SizeMB: 14525, Quant: "BF16"},
	"qwen2.5:14b":       {Repo: "Qwen/Qwen2.5-14B-Instruct", SizeMB: 28171, Quant: "BF16"},
	"qwen2.5:72b":       {Repo: "Qwen/Qwen2.5-72B-Instruct", SizeMB: 138676, Quant: "BF16"},
	"qwen2.5-coder:7b":  {Repo: "Qwen/Qwen2.5-Coder-7B-Instruct", SizeMB: 14525, Quant: "BF16"},
	"qwen2.5-coder:32b": {Repo: "Qwen/Qwen2.5-Coder-32B-Instruct", SizeMB: 62492, Quant: "BF16"},
	"deepseek-r1:7b":    {Repo: "deepseek-ai/DeepSeek-R1-Distill-Qwen-7B", SizeMB: 14525, Quant: "BF16"},
	"deepseek-r1:14b":   {Repo: "deepseek-ai/DeepSeek-R1-Distill-Qwen-14B", SizeMB: 28171, Quant: "BF16"},
	"deepseek-r1:32b":   {Repo: "deepseek-ai/DeepSeek-R1-Distill-Qwen-32B", SizeMB: 62492, Quant: "BF16"},
	"deepseek-r1:70b":   {Repo: "deepseek-ai/DeepSeek-R1-Distill-Llama-70B", SizeMB: 134570, Quant: "BF16"},
	"phi4:14b":          {Repo: "microsoft/phi-4", SizeMB: 27960, Quant: "BF16"},
	"phi3.5:3.8b":       {Repo: "microsoft/Phi-3.5-mini-instruct", SizeMB: 7288, Quant: "BF16"},
	"codellama:7b":      {Repo: "codellama/CodeLlama-7b-Instruct-hf", SizeMB: 12852, Quant: "BF16"},
	"codellama:13b":     {Repo: "codellama/CodeLlama-13b-Instruct-hf", SizeMB: 24826, Quant: "BF16"},
}

// catalogHFUnmapped lists every catalog model without a Hugging Face
// equivalent, with the reason. Every catalogModels Name is in exactly one of
// catalogHFRepos and catalogHFUnmapped.
var catalogHFUnmapped = map[string]string{
	"llama3.2:3b":       "gated",
	"llama3.2:1b":       "gated",
	"llama3.1:8b":       "gated",
	"llama3.1:70b":      "gated",
	"llama3.3:70b":      "gated",
	"mistral:7b":        "unverified",
	"mixtral:8x7b":      "unverified",
	"gemma2:9b":         "gated",
	"gemma2:27b":        "gated",
	"nomic-embed-text":  "embedding model",
	"mxbai-embed-large": "embedding model",
	"llava:7b":          "multimodal, not curated",
}

// catalogHFVariant returns the single Hugging Face row a vLLM or TGI node
// shows for a mapped catalog model, plus its fit verdict. ok is false for any
// other runtime or an unmapped model, which keep the Ollama variants.
//
// A SizeMB of 0 or less is unknown: the fit is set to "unknown" directly
// because classifyFit would read a zero estimate as comfortably green, and the
// row is not marked Recommended.
func catalogHFVariant(name, runtime string, vramTotalBytes int64, vramSource string) (ModelVariant, string, bool) {
	if runtime != "vllm" && runtime != "tgi" {
		return ModelVariant{}, "", false
	}
	e, mapped := catalogHFRepos[name]
	if !mapped {
		return ModelVariant{}, "", false
	}
	v := ModelVariant{Tag: e.Repo, Quantization: e.Quant, SizeMB: e.SizeMB, Recommended: e.SizeMB > 0}
	if e.SizeMB <= 0 {
		return v, "unknown", true
	}
	estBytes, fit, _ := computeContextFeasibility(e.SizeMB, hfListContextTokens, safetensorsOverheadMult, safetensorsPerTokenMBFallback, vramTotalBytes, vramSource, nil, "")
	v.VRAMEstMB = estBytes / (1024 * 1024)
	return v, fit, true
}
