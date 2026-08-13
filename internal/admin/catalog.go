package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/nodeagent"
	"github.com/ollama-mesh/ollama-mesh/internal/router"
)

// shardedGGUFFilename matches a multi-part GGUF split filename, e.g.
// "kimi-k2.5-q3_k_s-00001-of-00010.gguf" - see the exclusion in
// handleModelRepo's GGUF loop for why these are never offered as a variant.
var shardedGGUFFilename = regexp.MustCompile(`-\d+-of-\d+\.gguf$`)

// hfHTTPClient is shared across all Hugging Face search/browse requests.
// http.Client is safe for concurrent use, and a single instance keeps a live
// idle-connection pool instead of forcing a fresh TCP/TLS handshake (and
// TIME_WAIT churn) on every admin browsing request.
var hfHTTPClient = &http.Client{Timeout: 10 * time.Second}

// CatalogModel is a curated, popular Ollama model baked into the binary.
type CatalogModel struct {
	Name        string         `json:"name"`         // e.g. "llama3.2:3b"
	DisplayName string         `json:"display_name"` // e.g. "Llama 3.2 3B"
	Description string         `json:"description"`
	ParamCount  string         `json:"param_count"` // "3B", "8B", "70B"
	Categories  []string       `json:"categories"`  // ["chat", "coding", "reasoning"]
	Variants    []ModelVariant `json:"variants"`    // different quant options
	Popular     bool           `json:"popular"`
	Rank        int            `json:"rank"` // lower = more popular
}

// ModelVariant is a specific quantization of a catalog model.
type ModelVariant struct {
	Tag          string `json:"tag"`          // "llama3.2:3b-instruct-q4_K_M"
	Quantization string `json:"quantization"` // "Q4_K_M"
	VRAMEstMB    int64  `json:"vram_est_mb"`  // estimated VRAM in MB
	SizeMB       int64  `json:"size_mb"`      // download size in MB
	Recommended  bool   `json:"recommended"`  // best quality/size tradeoff
}

// catalogModels is the static, hardcoded catalog of popular Ollama models.
// VRAM estimates are approximate, in MB, derived from published quant sizes
// plus typical KV-cache/runtime overhead.
var catalogModels = []CatalogModel{
	{
		Name: "llama3.2:3b", DisplayName: "Llama 3.2 3B", ParamCount: "3B",
		Description: "Compact Meta model. Fast, capable general-purpose chat that runs on modest GPUs.",
		Categories:  []string{"chat", "fast"}, Popular: true, Rank: 1,
		Variants: []ModelVariant{
			{Tag: "llama3.2:3b", Quantization: "Q4_K_M", VRAMEstMB: 2048, SizeMB: 2000, Recommended: true},
			{Tag: "llama3.2:3b-instruct-q8_0", Quantization: "Q8_0", VRAMEstMB: 3174, SizeMB: 3100},
		},
	},
	{
		Name: "llama3.2:1b", DisplayName: "Llama 3.2 1B", ParamCount: "1B",
		Description: "Smallest Llama 3.2. Edge and CPU-friendly, ideal for low-latency tasks.",
		Categories:  []string{"chat", "fast", "edge"}, Popular: true, Rank: 8,
		Variants: []ModelVariant{
			{Tag: "llama3.2:1b", Quantization: "Q4_K_M", VRAMEstMB: 1024, SizeMB: 1300, Recommended: true},
		},
	},
	{
		Name: "llama3.1:8b", DisplayName: "Llama 3.1 8B", ParamCount: "8B",
		Description: "The workhorse. Strong general chat and coding, fits most consumer GPUs.",
		Categories:  []string{"chat", "coding"}, Popular: true, Rank: 2,
		Variants: []ModelVariant{
			{Tag: "llama3.1:8b", Quantization: "Q4_K_M", VRAMEstMB: 4813, SizeMB: 4700, Recommended: true},
			{Tag: "llama3.1:8b-instruct-q8_0", Quantization: "Q8_0", VRAMEstMB: 8704, SizeMB: 8500},
			{Tag: "llama3.1:8b-instruct-fp16", Quantization: "F16", VRAMEstMB: 16384, SizeMB: 16000},
		},
	},
	{
		Name: "llama3.1:70b", DisplayName: "Llama 3.1 70B", ParamCount: "70B",
		Description: "Large Llama 3.1. Frontier-class reasoning, needs serious VRAM.",
		Categories:  []string{"chat", "reasoning"}, Popular: true, Rank: 12,
		Variants: []ModelVariant{
			{Tag: "llama3.1:70b", Quantization: "Q4_K_M", VRAMEstMB: 40960, SizeMB: 40000, Recommended: true},
			{Tag: "llama3.1:70b-instruct-q8_0", Quantization: "Q8_0", VRAMEstMB: 76800, SizeMB: 75000},
		},
	},
	{
		Name: "llama3.3:70b", DisplayName: "Llama 3.3 70B", ParamCount: "70B",
		Description: "Latest 70B Llama with 405B-class quality at a fraction of the cost.",
		Categories:  []string{"chat", "reasoning"}, Popular: true, Rank: 5,
		Variants: []ModelVariant{
			{Tag: "llama3.3:70b", Quantization: "Q4_K_M", VRAMEstMB: 40960, SizeMB: 42000, Recommended: true},
		},
	},
	{
		Name: "mistral:7b", DisplayName: "Mistral 7B", ParamCount: "7B",
		Description: "Efficient European model. Solid chat and coding for its size.",
		Categories:  []string{"chat", "coding"}, Popular: true, Rank: 6,
		Variants: []ModelVariant{
			{Tag: "mistral:7b", Quantization: "Q4_K_M", VRAMEstMB: 4198, SizeMB: 4100, Recommended: true},
			{Tag: "mistral:7b-instruct-q8_0", Quantization: "Q8_0", VRAMEstMB: 7885, SizeMB: 7700},
		},
	},
	{
		Name: "mixtral:8x7b", DisplayName: "Mixtral 8x7B", ParamCount: "47B",
		Description: "Sparse mixture-of-experts. Big-model quality with faster inference.",
		Categories:  []string{"chat", "reasoning"}, Popular: true, Rank: 15,
		Variants: []ModelVariant{
			{Tag: "mixtral:8x7b", Quantization: "Q4_K_M", VRAMEstMB: 26624, SizeMB: 26000, Recommended: true},
		},
	},
	{
		Name: "qwen2.5:7b", DisplayName: "Qwen 2.5 7B", ParamCount: "7B",
		Description: "Alibaba's strong multilingual model. Excellent coding and chat.",
		Categories:  []string{"chat", "coding"}, Popular: true, Rank: 3,
		Variants: []ModelVariant{
			{Tag: "qwen2.5:7b", Quantization: "Q4_K_M", VRAMEstMB: 4813, SizeMB: 4700, Recommended: true},
			{Tag: "qwen2.5:7b-instruct-q8_0", Quantization: "Q8_0", VRAMEstMB: 8192, SizeMB: 8000},
		},
	},
	{
		Name: "qwen2.5:14b", DisplayName: "Qwen 2.5 14B", ParamCount: "14B",
		Description: "Mid-size Qwen 2.5. Better reasoning than 7B, still single-GPU friendly.",
		Categories:  []string{"chat", "coding"}, Popular: true, Rank: 9,
		Variants: []ModelVariant{
			{Tag: "qwen2.5:14b", Quantization: "Q4_K_M", VRAMEstMB: 9216, SizeMB: 9000, Recommended: true},
		},
	},
	{
		Name: "qwen2.5:72b", DisplayName: "Qwen 2.5 72B", ParamCount: "72B",
		Description: "Flagship Qwen 2.5. Top-tier reasoning and multilingual performance.",
		Categories:  []string{"chat", "reasoning"}, Popular: true, Rank: 16,
		Variants: []ModelVariant{
			{Tag: "qwen2.5:72b", Quantization: "Q4_K_M", VRAMEstMB: 44032, SizeMB: 43000, Recommended: true},
		},
	},
	{
		Name: "qwen2.5-coder:7b", DisplayName: "Qwen 2.5 Coder 7B", ParamCount: "7B",
		Description: "Code-specialized Qwen. Strong fill-in-the-middle and repo-level coding.",
		Categories:  []string{"coding"}, Popular: true, Rank: 4,
		Variants: []ModelVariant{
			{Tag: "qwen2.5-coder:7b", Quantization: "Q4_K_M", VRAMEstMB: 4813, SizeMB: 4700, Recommended: true},
		},
	},
	{
		Name: "qwen2.5-coder:32b", DisplayName: "Qwen 2.5 Coder 32B", ParamCount: "32B",
		Description: "Largest Qwen coder. GPT-4-class coding ability when it fits in VRAM.",
		Categories:  []string{"coding"}, Popular: true, Rank: 10,
		Variants: []ModelVariant{
			{Tag: "qwen2.5-coder:32b", Quantization: "Q4_K_M", VRAMEstMB: 19456, SizeMB: 19000, Recommended: true},
		},
	},
	{
		Name: "deepseek-r1:7b", DisplayName: "DeepSeek-R1 7B", ParamCount: "7B",
		Description: "Distilled reasoning model. Shows its chain of thought before answering.",
		Categories:  []string{"reasoning"}, Popular: true, Rank: 7,
		Variants: []ModelVariant{
			{Tag: "deepseek-r1:7b", Quantization: "Q4_K_M", VRAMEstMB: 4813, SizeMB: 4700, Recommended: true},
		},
	},
	{
		Name: "deepseek-r1:14b", DisplayName: "DeepSeek-R1 14B", ParamCount: "14B",
		Description: "Mid-size R1 distill. Stronger multi-step reasoning than 7B.",
		Categories:  []string{"reasoning"}, Popular: true, Rank: 11,
		Variants: []ModelVariant{
			{Tag: "deepseek-r1:14b", Quantization: "Q4_K_M", VRAMEstMB: 9216, SizeMB: 9000, Recommended: true},
		},
	},
	{
		Name: "deepseek-r1:32b", DisplayName: "DeepSeek-R1 32B", ParamCount: "32B",
		Description: "Large R1 distill. Excellent math and reasoning on a single big GPU.",
		Categories:  []string{"reasoning"}, Popular: true, Rank: 13,
		Variants: []ModelVariant{
			{Tag: "deepseek-r1:32b", Quantization: "Q4_K_M", VRAMEstMB: 19456, SizeMB: 19000, Recommended: true},
		},
	},
	{
		Name: "deepseek-r1:70b", DisplayName: "DeepSeek-R1 70B", ParamCount: "70B",
		Description: "Flagship R1 distill. Frontier reasoning, needs datacenter-class VRAM.",
		Categories:  []string{"reasoning"}, Popular: true, Rank: 17,
		Variants: []ModelVariant{
			{Tag: "deepseek-r1:70b", Quantization: "Q4_K_M", VRAMEstMB: 43008, SizeMB: 42000, Recommended: true},
		},
	},
	{
		Name: "phi4:14b", DisplayName: "Phi-4 14B", ParamCount: "14B",
		Description: "Microsoft's data-efficient model. Punches above its weight on reasoning.",
		Categories:  []string{"chat", "coding"}, Popular: true, Rank: 14,
		Variants: []ModelVariant{
			{Tag: "phi4:14b", Quantization: "Q4_K_M", VRAMEstMB: 9114, SizeMB: 8900, Recommended: true},
		},
	},
	{
		Name: "phi3.5:3.8b", DisplayName: "Phi-3.5 3.8B", ParamCount: "3.8B",
		Description: "Small Microsoft model with a long context window. Great for edge deploys.",
		Categories:  []string{"chat", "edge"}, Popular: false, Rank: 20,
		Variants: []ModelVariant{
			{Tag: "phi3.5:3.8b", Quantization: "Q4_K_M", VRAMEstMB: 2253, SizeMB: 2200, Recommended: true},
		},
	},
	{
		Name: "gemma2:9b", DisplayName: "Gemma 2 9B", ParamCount: "9B",
		Description: "Google's open model. Polished, safe general-purpose chat.",
		Categories:  []string{"chat"}, Popular: true, Rank: 18,
		Variants: []ModelVariant{
			{Tag: "gemma2:9b", Quantization: "Q4_K_M", VRAMEstMB: 5530, SizeMB: 5400, Recommended: true},
		},
	},
	{
		Name: "gemma2:27b", DisplayName: "Gemma 2 27B", ParamCount: "27B",
		Description: "Larger Gemma 2. Stronger reasoning and knowledge on a single GPU.",
		Categories:  []string{"chat", "reasoning"}, Popular: false, Rank: 19,
		Variants: []ModelVariant{
			{Tag: "gemma2:27b", Quantization: "Q4_K_M", VRAMEstMB: 16384, SizeMB: 16000, Recommended: true},
		},
	},
	{
		Name: "codellama:7b", DisplayName: "Code Llama 7B", ParamCount: "7B",
		Description: "Meta's code model. Reliable autocompletion and code generation.",
		Categories:  []string{"coding"}, Popular: false, Rank: 21,
		Variants: []ModelVariant{
			{Tag: "codellama:7b", Quantization: "Q4_K_M", VRAMEstMB: 3891, SizeMB: 3800, Recommended: true},
		},
	},
	{
		Name: "codellama:13b", DisplayName: "Code Llama 13B", ParamCount: "13B",
		Description: "Larger Code Llama. Better at complex, multi-file coding tasks.",
		Categories:  []string{"coding"}, Popular: false, Rank: 22,
		Variants: []ModelVariant{
			{Tag: "codellama:13b", Quantization: "Q4_K_M", VRAMEstMB: 7578, SizeMB: 7400, Recommended: true},
		},
	},
	{
		Name: "nomic-embed-text", DisplayName: "Nomic Embed Text", ParamCount: "137M",
		Description: "High-quality text embeddings. Tiny footprint, great for RAG.",
		Categories:  []string{"embedding"}, Popular: true, Rank: 23,
		Variants: []ModelVariant{
			{Tag: "nomic-embed-text", Quantization: "F16", VRAMEstMB: 274, SizeMB: 274, Recommended: true},
		},
	},
	{
		Name: "mxbai-embed-large", DisplayName: "MixedBread Embed Large", ParamCount: "335M",
		Description: "Larger embedding model. Top-tier retrieval accuracy for RAG pipelines.",
		Categories:  []string{"embedding"}, Popular: false, Rank: 24,
		Variants: []ModelVariant{
			{Tag: "mxbai-embed-large", Quantization: "F16", VRAMEstMB: 670, SizeMB: 670, Recommended: true},
		},
	},
	{
		Name: "llava:7b", DisplayName: "LLaVA 7B", ParamCount: "7B",
		Description: "Vision-language model. Describe images, answer questions about them.",
		Categories:  []string{"vision", "multimodal"}, Popular: true, Rank: 25,
		Variants: []ModelVariant{
			{Tag: "llava:7b", Quantization: "Q4_K_M", VRAMEstMB: 4608, SizeMB: 4500, Recommended: true},
		},
	},
}

// catalogVariantFit is a variant decorated with per-node fit classification.
type catalogVariantFit struct {
	ModelVariant
	Fit     string `json:"fit"`      // green / yellow / red / unknown
	DiskFit string `json:"disk_fit"` // ok / insufficient / unknown
}

// catalogModelFit is a catalog model decorated for one node: per-variant fit
// plus whether the model is already downloaded on that node.
type catalogModelFit struct {
	CatalogModel
	Variants   []catalogVariantFit `json:"variants"`
	Downloaded bool                `json:"downloaded"`
}

// catalogNodeEntry holds the fit results for a single node.
type catalogNodeEntry struct {
	Name           string  `json:"name"`
	URL            string  `json:"url"`
	Runtime        string  `json:"runtime,omitempty"`
	VRAMFreeBytes  int64   `json:"vram_free_bytes"`
	VRAMTotalBytes int64   `json:"vram_total_bytes"`
	VRAMUsedBytes  int64   `json:"vram_used_bytes"`
	VRAMSource     string  `json:"vram_source"`
	DiskFreeGB     float64 `json:"disk_free_gb"`
	DiskTotalGB    float64 `json:"disk_total_gb"`
	DiskKnown      bool    `json:"disk_known"` // false when the agent has never reported disk telemetry (R1 - never fabricate a reading)
	// DockerDeployed flags that DiskFreeGB/DiskTotalGB are this agent's own
	// *host* filesystem reading (host_linux.go's readDiskStatsGB("/")) while
	// the runtime itself is Docker-controlled - the container's actual model
	// storage can live on a separate, differently-sized volume the host
	// reading knows nothing about. The mesh's pre-pull disk-fit gate already
	// checks the container's real number before actually pulling
	// (admin.go's containerDiskStatsViaAgent); this field lets the UI flag
	// the same caveat on the figure it displays, rather than showing a
	// number that may silently not match what the container has.
	DockerDeployed bool `json:"docker_deployed"`
	// GPUCount is how many local GPUs this node's agent reports (0 when no
	// per-device breakdown has ever been reported). VRAMFitBasis explains
	// which capacity VRAMTotalBytes/fit verdicts above were sized against on
	// a node with more than one: "combined" (summed across all GPUs, for a
	// runtime that shards a model across them) or "largest" (single biggest
	// GPU, for a runtime that pins a model to one device). Empty on a
	// single-GPU (or GPU-unknown) node, where sizing is unchanged from
	// VRAMTotalBytes alone - see nodeVRAMCapacity's doc comment.
	GPUCount     int    `json:"gpu_count,omitempty"`
	VRAMFitBasis string `json:"vram_fit_basis,omitempty"`
	// GPUCountUnknown is true when this node has no agent-confirmed
	// per-device GPU reading at all (no agent, or VRAMSource=="declared" -
	// a manually-entered whole-node total with no per-device breakdown) -
	// P75 Gap D. Without this, such a node's GPUCount==0 looks identical, at
	// the same apparent confidence, to a confirmed single-GPU node - R1
	// requires disclosing "unknown" rather than implying a reading that was
	// never taken.
	GPUCountUnknown bool `json:"gpu_count_unknown,omitempty"`
	// Capabilities lists the node's effective Node Agent action capabilities
	// (e.g. "models.pull", "runtime.restart") - empty/omitted when there is no
	// agent, the agent hasn't reported yet, or the agent is disabled in
	// settings, matching exactly what handleNodePull's dispatch would do
	// (never the raw agent-advertised list when settings would refuse to use
	// it - see handleModelCatalog).
	Capabilities []string          `json:"capabilities,omitempty"`
	Models       []catalogModelFit `json:"models"`
}

// classifyPullTagFormat buckets a pull tag string by the download mechanism
// it requires, so both the curated-catalog fit path and handleNodePull's
// hard-block gate can tell a genuine format incompatibility apart from an
// ordinary capacity verdict, without guessing at anything the tag string
// alone can't prove:
//
//   - "ollama-library": a bare "name[:tag]" with no "/" - Ollama's own
//     official-library shorthand (every compiled catalogModels variant tag
//     is exactly this shape, e.g. "llama3.2:3b"). Only `ollama pull` resolves
//     this. The node agent's own pull fallback for every other runtime
//     (nodeagent/actions.go pullViaHFHub/pullViaTGI) shells out to
//     huggingface-cli/text-generation-server with a Hugging Face "org/repo"
//     id - a bare Ollama library name is not that, and llama.cpp has no
//     other pull mechanism of its own either.
//   - "gguf-hf": "hf.co/..." prefix, or a bare filename ending ".gguf" -
//     Ollama/llama.cpp's convention for pulling a GGUF quant straight from a
//     Hugging Face repo (ggufOnlyRuntime's own two runtimes). vLLM, TGI, and
//     MLX never load GGUF.
//   - "hf-repo": a bare "org/repo"[:revision] Hugging Face identifier - the
//     shape handleModelRepo already generates for vLLM/TGI/MLX's own browse
//     variants. Deliberately never flagged incompatible: it's ambiguous with
//     an Ollama community-published model of the same shape
//     (e.g. "someuser/somemodel"), and even once confirmed as a real HF repo,
//     nothing in the bare string says whether it's plain-safetensors, an
//     AWQ/GPTQ quant, or MLX-converted without fetching the repo's own
//     metadata (which handleModelRepo does, but handleNodePull's tag-only
//     hard-block gate never has) - R1 forbids fabricating that certainty.
func classifyPullTagFormat(model string) string {
	lower := strings.ToLower(model)
	if strings.HasPrefix(lower, "hf.co/") || strings.HasSuffix(lower, ".gguf") {
		return "gguf-hf"
	}
	if strings.Contains(model, "/") {
		// Includes a namespaced Ollama community tag (e.g. "someuser/somemodel")
		// misclassified as "hf-repo" - indistinguishable from a real HF repo id
		// by shape alone. Errs safe (never blocked here), not fixed - see the
		// "hf-repo" bucket's doc comment above for why.
		return "hf-repo"
	}
	return "ollama-library"
}

// pullFormatIncompatible reports whether format (see classifyPullTagFormat)
// can never be loaded by runtime - the only two format buckets confident
// enough to hard-block on. An empty/undeclared runtime is treated as Ollama,
// matching ggufOnlyRuntime's existing default elsewhere in this file.
//
// Law 5 compatibility matrix, stated explicitly (nothing here silently
// narrows to Ollama-vs-everything-else):
//   - "ollama-library" is compatible with {ollama, ""} only - incompatible
//     with vllm, tgi, llamacpp, and mlx.
//   - "gguf-hf" is compatible with ggufOnlyRuntime's {"", ollama, llamacpp} -
//     incompatible with vllm, tgi, and mlx.
//   - "hf-repo" is never flagged incompatible by this function (see
//     classifyPullTagFormat's doc comment on why that's deliberate, not an
//     oversight).
func pullFormatIncompatible(format, runtime string) bool {
	switch format {
	case "ollama-library":
		return runtime != "" && runtime != "ollama"
	case "gguf-hf":
		return !ggufOnlyRuntime(runtime)
	default:
		return false
	}
}

// pullFormatDescription renders a classifyPullTagFormat bucket as the
// operator-facing phrase used in handleNodePull's rejection message.
func pullFormatDescription(format string) string {
	switch format {
	case "gguf-hf":
		return "a GGUF file/repo reference"
	default:
		return "an Ollama library tag"
	}
}

// nodeRuntimeByName looks up a node's declared runtime by name, matching the
// fail-closed-to-empty-string behavior every other by-name lookup in this
// file uses (see nodeDiskState). Returns ok=false for an unknown node name.
func nodeRuntimeByName(nodes []*router.NodeState, name string) (runtime string, ok bool) {
	for _, n := range nodes {
		if n.Name != name {
			continue
		}
		n.RLock()
		runtime = n.Runtime
		n.RUnlock()
		return runtime, true
	}
	return "", false
}

// classifyDiskFit reports whether a pull of sizeMB (download size, MiB) would
// fit in diskFreeGB (decimal GB, from Node.DiskFreeGB) of free disk space.
// Unlike VRAM fit, disk space is not a transient snapshot the mesh's own
// scheduling causes to fluctuate wrongly - a pull exceeding free disk WILL
// fail (partial download, or worst case fills the node's disk and disrupts
// the OS/other running models), so this is a hard yes/no, not a
// green/yellow/red gradient. "unknown" (never a fabricated "ok") applies
// whenever the agent hasn't reported disk telemetry - no agent, an agent
// build predating disk telemetry, or a non-Linux agent (host_other.go
// reports no disk stats today, R1) - so the caller must treat "unknown" as
// "cannot verify" and never silently allow-by-default on it for a
// hard-block decision.
//
// diskTotalGB (not diskFreeGB) is the "do we have real telemetry" signal:
// both fields come from the same syscall.Statfs read (host_linux.go), so
// DiskFreeGB legitimately reports 0 for a node that is genuinely completely
// full - using diskFreeGB<=0 as the unknown-check would misclassify that
// worst case as "unknown" and silently skip the hard block precisely when
// it matters most. DiskTotalGB is only 0/absent when the agent has never
// reported real disk stats at all.
func classifyDiskFit(sizeMB int64, diskFreeGB, diskTotalGB float64, agentPresent bool) string {
	if !agentPresent || diskTotalGB <= 0 {
		return "unknown"
	}
	neededBytes := float64(sizeMB) * 1024 * 1024
	freeBytes := diskFreeGB * 1e9
	if neededBytes > freeBytes {
		return "insufficient"
	}
	return "ok"
}

// findCatalogVariantSizeMB looks up the download size (MiB) of a curated
// catalog variant by its pull tag, using the same tag/base-name matching
// isDownloaded already uses. Returns ok=false for any tag not in the static
// catalog (e.g. an HF repo tag, or an Ollama registry model not curated
// here) - callers must skip the disk check rather than guess a size (R1).
func findCatalogVariantSizeMB(model string) (int64, bool) {
	for _, cm := range catalogModels {
		for _, v := range cm.Variants {
			if v.Tag == model || v.Tag+":latest" == model {
				return v.SizeMB, true
			}
		}
		if cm.Name == model || cm.Name+":latest" == model {
			// Bare catalog name with no explicit variant tag (e.g. "llama3.2:3b"
			// pulled directly) - use the recommended variant's size, falling
			// back to the first variant.
			for _, v := range cm.Variants {
				if v.Recommended {
					return v.SizeMB, true
				}
			}
			if len(cm.Variants) > 0 {
				return cm.Variants[0].SizeMB, true
			}
		}
	}
	return 0, false
}

// nodeDiskState looks up a node's agent-reported disk telemetry by name for
// use by the pull-time hard-block gate (handleNodePull). Returns
// agentPresent=false for an unknown node name, matching every other
// agent-derived-field lookup's fail-closed-to-unknown behavior in this file.
func nodeDiskState(nodes []*router.NodeState, name string) (diskFreeGB, diskTotalGB float64, agentPresent bool) {
	for _, n := range nodes {
		if n.Name != name {
			continue
		}
		n.RLock()
		diskFreeGB = n.DiskFreeGB
		diskTotalGB = n.DiskTotalGB
		agentPresent = n.AgentPresent
		n.RUnlock()
		return
	}
	return 0, 0, false
}

// classifyFit returns the fit color for an estimated VRAM requirement (in bytes)
// against the node's total VRAM capacity (in bytes), not its currently-free
// VRAM. Free VRAM is a transient snapshot of whatever else happens to be warm
// on the node right now - classifying against it produces false "red"
// verdicts for models that would fit once other models evict/idle-timeout.
// The advisor answers "will this model ever fit on this hardware", not
// "does it fit this instant" (that's routing's free_vram_headroom factor).
func classifyFit(vramEstBytes, vramCapacityBytes int64, vramSource string) string {
	if vramSource == "unknown" || vramSource == "inferred" {
		return "unknown"
	}
	switch {
	case vramEstBytes <= int64(float64(vramCapacityBytes)*0.85):
		return "green"
	case vramEstBytes <= vramCapacityBytes:
		return "yellow"
	default:
		return "red"
	}
}

// nodeVRAMCapacity picks the VRAM capacity (MB) to size catalog fit against
// for a node, and reports how many local GPUs it reflects. A node with only
// one reported device (or no per-device breakdown at all - agentGPUs is
// empty until an agent with the multi-GPU telemetry array reports in) keeps
// today's single aggregate figure unchanged (basis ""), and usedMB is -1 -
// callers should keep using their own whole-node used figure (from /api/ps)
// against it, exactly as before this function existed.
//
// For a node with more than one device, the correct capacity depends on
// whether the target runtime can actually spread one model across all of
// them:
//   - Ollama and llama.cpp shard a model across every local GPU, so the
//     model's real ceiling is the combined total (basis "combined") - the
//     whole-node used figure is the right pair for it, so usedMB is still -1.
//   - vLLM, TGI, and MLX pin a model to a single device (no built-in
//     multi-GPU sharding in the mesh's deployment model), so the real
//     ceiling is whichever single card is biggest (basis "largest") - never
//     the sum. Summing here would turn a genuine "won't fit on one card"
//     into a false green, which is strictly worse than the false red it
//     would otherwise show (R1: no dressing up an estimate as more certain,
//     or more favorable, than it is). For this basis usedMB is that same
//     device's own agent-reported VRAMUsedMB, never the whole-node figure -
//     pairing a single device's capacity with every other GPU's usage too
//     would make free VRAM read artificially low (or clamp to zero) even
//     when the biggest card itself has room.
//
// declaredIndices (P75 Gap B/C) is the operator-declared set of physical GPU
// indices this specific node/runtime instance actually uses - see
// scopeGPUsToDeclared's doc comment for why host-scoped agentGPUs alone
// cannot answer that question, and how a declaration also settles the
// combined-vs-largest basis for runtimes ggufOnlyRuntime would otherwise
// default to "largest" (Gap C: a declared multi-GPU scope on e.g. vLLM is
// itself evidence of a configured tensor-parallel deployment).
func nodeVRAMCapacity(vramTotalMB int64, agentGPUs []nodeagent.GPUInfo, runtime string, declaredIndices []int) (capacityMB int64, usedMB int64, gpuCount int, basis string) {
	scoped, applied := scopeGPUsToDeclared(agentGPUs, declaredIndices)
	gpuCount = len(scoped)
	if gpuCount < 2 {
		if applied && gpuCount == 1 {
			// A declared scope narrowed a multi-GPU host down to exactly one
			// GPU this node actually uses - vramTotalMB is the whole HOST's
			// aggregate (from agent telemetry), which would silently
			// reintroduce Gap B's double-count; the single scoped device's
			// own reading is the only correct capacity/used pair here.
			return scoped[0].VRAMTotalMB, scoped[0].VRAMUsedMB, gpuCount, ""
		}
		return vramTotalMB, -1, gpuCount, ""
	}
	if applied || ggufOnlyRuntime(runtime) {
		var sum int64
		for _, g := range scoped {
			sum += g.VRAMTotalMB
		}
		if sum > 0 {
			return sum, -1, gpuCount, "combined"
		}
		if applied {
			// A declared scope's devices all report zero VRAM (a transient
			// telemetry glitch, not "no breakdown available") - vramTotalMB is
			// the whole HOST's aggregate, which would silently reintroduce
			// Gap B/C's double-count; report unknown instead of the wrong total.
			return 0, -1, gpuCount, ""
		}
		return vramTotalMB, -1, gpuCount, ""
	}
	largestIdx := -1
	var largest int64
	for i, g := range scoped {
		if g.VRAMTotalMB > largest {
			largest = g.VRAMTotalMB
			largestIdx = i
		}
	}
	if largest > 0 {
		return largest, scoped[largestIdx].VRAMUsedMB, gpuCount, "largest"
	}
	return vramTotalMB, -1, gpuCount, ""
}

// scopeGPUsToDeclared restricts agentGPUs to the operator-declared subset of
// physical GPU indices this specific node/runtime instance actually uses
// (P75 Gap B/C). Host-scoped agent telemetry (pollAgentHost in
// internal/router/agent_poll.go) reports every physical GPU on a Host
// identically to every node sharing it - two separate runtime processes each
// pinned to a different GPU (e.g. via CUDA_VISIBLE_DEVICES) still both see
// the full agentGPUs array, so without a declared scope, nodeVRAMCapacity
// would size each of them against hardware it cannot actually reach
// (double-counting one physical VRAM pool across two node entries).
//
// declaredIndices empty (the default - nothing declared) is a no-op: returns
// agentGPUs unchanged and applied=false, preserving existing behavior for
// every node that hasn't opted in. A declaration that matches none of the
// currently-reported devices (stale or misconfigured) also falls back to the
// unscoped set with applied=false, rather than reporting a hard zero -
// same "no breakdown available" fallback nodeVRAMCapacity already uses for
// agentGPUs itself.
// gpuCountUnknown reports whether a node's GPU-count/capacity reading has no
// agent-confirmed per-device backing at all (P75 Gap D) - true when there is
// no agent, the total is an operator-declared whole-node figure with no
// per-device breakdown, or an agent is present but reported zero devices
// (e.g. before its first successful GPU-collector read). See
// catalogNodeEntry.GPUCountUnknown's doc comment for why this must never be
// conflated with a confirmed single-GPU reading.
func gpuCountUnknown(agentPresent bool, vramSource string, gpuCount int) bool {
	return !agentPresent || vramSource == "declared" || gpuCount == 0
}

func scopeGPUsToDeclared(agentGPUs []nodeagent.GPUInfo, declaredIndices []int) (scoped []nodeagent.GPUInfo, applied bool) {
	if len(declaredIndices) == 0 {
		return agentGPUs, false
	}
	want := make(map[int]bool, len(declaredIndices))
	for _, idx := range declaredIndices {
		want[idx] = true
	}
	out := make([]nodeagent.GPUInfo, 0, len(agentGPUs))
	for _, g := range agentGPUs {
		if want[g.Index] {
			out = append(out, g)
		}
	}
	if len(out) == 0 {
		return agentGPUs, false
	}
	return out, true
}

// handleModelCatalog serves the curated model catalog with per-node fit status.
// GET /admin/models/catalog (also /admin/v1/models/catalog)
//
// The catalog itself is static data compiled into the binary. For each node we
// reuse the same VRAM accounting as handleModelFit: nvidia-smi total minus the
// VRAM reported loaded by the last /api/ps poll. Each catalog variant is then
// classified green/yellow/red/unknown against that node's free VRAM. We also
// cross-reference /api/tags (cached in the router) to flag models already on disk.
func (s *Server) handleModelCatalog(w http.ResponseWriter, r *http.Request) {
	nodes := s.router.Nodes()
	nodeEntries := make([]catalogNodeEntry, 0, len(nodes))

	for _, n := range nodes {
		n.RLock()
		nodeURL := n.URL
		nodeName := n.Name
		nodeRuntime := n.Runtime
		vramTotalMB := n.VRAMTotalMB
		agentGPUs := append([]nodeagent.GPUInfo(nil), n.AgentGPUs...)
		declaredGPUIndices := append([]int(nil), n.DeclaredGPUIndices...)
		vramUsedMBFromPS := int64(0)
		rawVramSource := n.VRAMSource
		for _, m := range n.LoadedModels {
			vramUsedMBFromPS += m.SizeVRAM / (1024 * 1024)
		}
		agentPresent := n.AgentPresent
		diskFreeGB := n.DiskFreeGB
		diskTotalGB := n.DiskTotalGB
		agentCapabilities := append([]string(nil), n.AgentCapabilities...)
		n.RUnlock()

		// Effective capabilities: only what handleNodePull's dispatch would
		// actually use - a node whose agent is present-but-disabled in
		// settings must report no capabilities, not the raw agent-advertised
		// list, so the catalog and real pull dispatch never disagree.
		var capabilities []string
		if agentCfg, agentOK := s.router.NodeAgentSetting(nodeName); agentOK && agentCfg.Enabled {
			capabilities = agentCapabilities
		}

		capacityMB, capacityUsedMB, gpuCount, vramFitBasis := nodeVRAMCapacity(vramTotalMB, agentGPUs, nodeRuntime, declaredGPUIndices)

		var vramFreeBytes int64
		var vramTotalBytes int64
		vramSource := "unknown"
		if capacityMB > 0 {
			vramTotalBytes = capacityMB * 1024 * 1024
			usedMB := vramUsedMBFromPS
			if capacityUsedMB >= 0 {
				usedMB = capacityUsedMB
			}
			vramUsedBytes := usedMB * 1024 * 1024
			vramFreeBytes = vramTotalBytes - vramUsedBytes
			if vramFreeBytes < 0 {
				vramFreeBytes = 0
			}
			if rawVramSource == "nvidia" {
				vramSource = "nvidia-smi"
			} else if rawVramSource == "declared" {
				vramSource = "declared"
			} else {
				vramSource = "nvidia-smi" // fallback
			}
		} else if vramUsedMBFromPS > 0 {
			vramSource = "inferred"
		} else {
			vramSource = "unknown"
		}

		// Build a set of downloaded model names/tags from /api/tags (cached 30s).
		downloaded := make(map[string]bool)
		if tagModels, err := s.router.FetchModelTags(nodeURL); err == nil {
			for _, tm := range tagModels {
				downloaded[tm.Name] = true
			}
		}

		// See catalogNodeEntry.DockerDeployed's doc comment: diskFreeGB/
		// diskTotalGB above are this agent's host filesystem, which can
		// differ from a Docker-controlled runtime's actual storage volume.
		dockerDeployed := false
		if ctrl, configured := s.router.NodeControlSetting(nodeName); configured && ctrl.Driver == "docker" {
			dockerDeployed = true
		}

		// Every compiled catalogModels tag is an Ollama-library-format string
		// (see classifyPullTagFormat) - computed once per node, not per
		// variant, since it depends only on nodeRuntime.
		catalogIncompatible := pullFormatIncompatible("ollama-library", nodeRuntime)

		models := make([]catalogModelFit, 0, len(catalogModels))
		for _, cm := range catalogModels {
			variants := make([]catalogVariantFit, 0, len(cm.Variants))
			for _, v := range cm.Variants {
				estBytes := v.VRAMEstMB * 1024 * 1024
				fit := classifyFit(estBytes, vramTotalBytes, vramSource)
				// "incompatible" overrides any capacity-based verdict: a
				// capacity word (green/yellow/red) must never also carry a
				// compatibility fact, and there is no VRAM amount that makes
				// a format this runtime can't load fit anyway.
				if catalogIncompatible {
					fit = "incompatible"
				}
				variants = append(variants, catalogVariantFit{
					ModelVariant: v,
					Fit:          fit,
					DiskFit:      classifyDiskFit(v.SizeMB, diskFreeGB, diskTotalGB, agentPresent),
				})
			}
			models = append(models, catalogModelFit{
				CatalogModel: cm,
				Variants:     variants,
				Downloaded:   isDownloaded(cm, downloaded),
			})
		}

		nodeEntries = append(nodeEntries, catalogNodeEntry{
			Name:            nodeName,
			URL:             nodeURL,
			Runtime:         nodeRuntime,
			VRAMFreeBytes:   vramFreeBytes,
			VRAMTotalBytes:  vramTotalBytes,
			VRAMUsedBytes:   vramUsedMBFromPS * 1024 * 1024,
			VRAMSource:      vramSource,
			DiskFreeGB:      diskFreeGB,
			DiskTotalGB:     diskTotalGB,
			DiskKnown:       agentPresent && diskTotalGB > 0,
			DockerDeployed:  dockerDeployed,
			Capabilities:    capabilities,
			Models:          models,
			GPUCount:        gpuCount,
			VRAMFitBasis:    vramFitBasis,
			GPUCountUnknown: gpuCountUnknown(agentPresent, rawVramSource, gpuCount),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"catalog": catalogModels,
		"nodes":   nodeEntries,
	})
}

// isDownloaded reports whether any of a catalog model's tags (or its base name)
// appears in the node's downloaded set. Ollama tags often carry an implicit
// ":latest", so we match on the base name and on each variant tag.
func isDownloaded(cm CatalogModel, downloaded map[string]bool) bool {
	if downloaded[cm.Name] || downloaded[cm.Name+":latest"] {
		return true
	}
	for _, v := range cm.Variants {
		if downloaded[v.Tag] || downloaded[v.Tag+":latest"] {
			return true
		}
		// Match base name before any quant suffix (e.g. "llama3.1:8b" from
		// a downloaded "llama3.1:8b-instruct-q4_K_M").
		if base, _, ok := strings.Cut(v.Tag, "-"); ok && downloaded[base] {
			return true
		}
	}
	return false
}

// HFModelInfo represents the metadata returned by Hugging Face API
type HFModelInfo struct {
	ID           string   `json:"id"`
	Downloads    int      `json:"downloads"`
	Likes        int      `json:"likes"`
	Tags         []string `json:"tags"`
	LastModified string   `json:"lastModified"`
	PipelineTag  string   `json:"pipeline_tag"`
	CreatedAt    string   `json:"createdAt"`
}

// ggufOnlyRuntime reports whether a node runtime only ever loads GGUF weight
// files. Ollama and llama.cpp both load GGUF; vLLM and TGI load standard HF
// `safetensors` repos (optionally AWQ/GPTQ quantized), never GGUF. An empty
// runtime defaults to the historical GGUF-only behavior (Ollama).
func ggufOnlyRuntime(runtime string) bool {
	switch runtime {
	case "", "ollama", "llamacpp":
		return true
	default:
		return false
	}
}

// hfSearchFilters returns the HF format filter tag (for the "filter=" param)
// and the set of pipeline_tag values relevant to a given runtime's search.
// GGUF runtimes use the "gguf" format filter alone with no pipeline_tag
// restriction, as before (unaffected by this).
//
// vLLM/TGI/MLX need a format filter too - verified live against HF's
// /api/models: the bare "safetensors"/"mlx" format filters alone return
// mostly embeddings/vision-classification/ASR/time-series repos (only ~1 in
// 10 of the top-downloaded "safetensors" results was an actual LLM). Adding
// pipeline_tag=text-generation narrows that correctly, but a single
// pipeline_tag excludes vision-language (VLM) repos - e.g.
// llava-hf/llava-1.5-7b-hf and the entire mlx-community/llava-* catalog are
// tagged pipeline_tag=image-text-to-text, not text-generation, and vLLM/MLX
// both genuinely serve VLMs. HF's REST API has no OR syntax for pipeline_tag
// (repeated params and comma-separated values both verified live to return
// zero results, not a union) - handleModelSearch below issues one upstream
// request per tag in the returned slice and merges them. "conversational"
// (the older VLM/chat tag) was checked and is dead - zero live results - so
// it's deliberately excluded rather than silently missing.
func hfSearchFilters(runtime string) (formatFilter string, pipelineTags []string) {
	if ggufOnlyRuntime(runtime) {
		return "gguf", nil
	}
	switch runtime {
	case "vllm", "tgi":
		return "safetensors", []string{"text-generation", "image-text-to-text"}
	case "mlx":
		return "mlx", []string{"text-generation", "image-text-to-text"}
	default:
		return "", nil
	}
}

// fetchHFModelList performs one GET against HF's /api/models and decodes the
// result. Extracted so handleModelSearch can issue more than one request
// (one per pipeline_tag) and merge them - see hfSearchFilters.
func (s *Server) fetchHFModelList(ctx context.Context, targetURL string) ([]HFModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", targetURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	if s.cfg.HuggingFace.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.cfg.HuggingFace.Token)
	}
	resp, err := hfHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch from Hugging Face: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Hugging Face API returned status %d", resp.StatusCode)
	}
	var models []HFModelInfo
	if err := json.NewDecoder(resp.Body).Decode(&models); err != nil {
		return nil, fmt.Errorf("decode Hugging Face response: %w", err)
	}
	return models, nil
}

// sortHFModelsInPlace re-sorts a merged multi-request model list back into
// the single sort order the caller asked for (each individual upstream
// request is already sorted the same way, but merging N sorted lists needs
// a re-sort, not a concatenation).
func sortHFModelsInPlace(models []HFModelInfo, sortField, direction string) {
	ascLess := func(i, j int) bool {
		switch sortField {
		case "likes":
			return models[i].Likes < models[j].Likes
		case "createdAt":
			ti, _ := time.Parse(time.RFC3339, models[i].CreatedAt)
			tj, _ := time.Parse(time.RFC3339, models[j].CreatedAt)
			return ti.Before(tj)
		default: // "downloads"
			return models[i].Downloads < models[j].Downloads
		}
	}
	if direction == "1" {
		sort.SliceStable(models, func(i, j int) bool { return ascLess(i, j) })
	} else {
		sort.SliceStable(models, func(i, j int) bool { return ascLess(j, i) })
	}
}

// detectSafetensorsQuant labels a non-GGUF repo's quantization method from
// its HF tags. Falls back to "FP16/BF16" for unquantized full-precision repos.
// MLX repos are tagged "library:mlx" on HF and carry their own quant format
// (distinct from AWQ/GPTQ/BNB) - the actual bit-width lives in config.json,
// not in tags, so it's labeled generically as "MLX" here rather than
// guessing a specific bit-width from tags alone (R1: no fabricated
// precision).
func detectSafetensorsQuant(tags []string) string {
	isMLX := false
	for _, t := range tags {
		lt := strings.ToLower(t)
		switch {
		case strings.Contains(lt, "awq"):
			return "AWQ"
		case strings.Contains(lt, "gptq"):
			return "GPTQ"
		case strings.Contains(lt, "bitsandbytes"), strings.Contains(lt, "bnb"):
			return "BNB"
		case lt == "mlx" || strings.Contains(lt, "library:mlx"):
			isMLX = true
		}
	}
	if isMLX {
		return "MLX"
	}
	return "FP16/BF16"
}

// HFRepoResponse represents the detail returned by Hugging Face API for a single repository
type HFRepoResponse struct {
	ID           string   `json:"id"`
	Downloads    int      `json:"downloads"`
	Likes        int      `json:"likes"`
	Tags         []string `json:"tags"`
	LastModified string   `json:"lastModified"`
	Siblings     []struct {
		Rfilename string `json:"rfilename"`
		Size      int64  `json:"size"`
	} `json:"siblings"`
}

type ModelVariantFit struct {
	Tag          string `json:"tag"`          // "hf.co/username/repo:quant"
	Quantization string `json:"quantization"` // "Q4_K_M"
	VRAMEstMB    int64  `json:"vram_est_mb"`
	SizeMB       int64  `json:"size_mb"`
	Fit          string `json:"fit"`      // "green", "yellow", "red", "unknown"
	DiskFit      string `json:"disk_fit"` // "ok", "insufficient", "unknown"
	Downloaded   bool   `json:"downloaded"`
}

// handleModelSearch searches Hugging Face GGUF models.
// GET /admin/models/search?q={query} (also /admin/v1/models/search)
func (s *Server) handleModelSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	runtime := r.URL.Query().Get("runtime")

	sortField := "downloads"
	direction := "-1"
	switch r.URL.Query().Get("sort") {
	case "likes":
		sortField = "likes"
	case "newest":
		sortField = "createdAt"
	case "oldest":
		sortField = "createdAt"
		direction = "1"
	}

	baseURL := fmt.Sprintf("https://huggingface.co/api/models?sort=%s&direction=%s&limit=25", sortField, direction)
	if query != "" {
		baseURL += "&search=" + url.QueryEscape(query)
	}

	formatFilter, pipelineTags := hfSearchFilters(runtime)

	var hfModels []HFModelInfo
	if len(pipelineTags) == 0 {
		targetURL := baseURL
		if formatFilter != "" {
			targetURL += "&filter=" + formatFilter
		}
		models, err := s.fetchHFModelList(r.Context(), targetURL)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusBadGateway)
			return
		}
		hfModels = models
	} else {
		// Fetched concurrently, not sequentially - hfHTTPClient's 10s timeout
		// would otherwise compound to ~20s worst case for a 2-tag runtime.
		// Concurrency also means one tag's transient failure doesn't erase
		// results the other tag already got - only fail the request if every
		// tag failed (mirrors the "drop the bad row, don't fail the whole
		// list" pattern already used for secret decrypt failures elsewhere in
		// this codebase, applied here to a multi-fetch merge instead).
		type fetchOutcome struct {
			models []HFModelInfo
			err    error
		}
		outcomes := make([]fetchOutcome, len(pipelineTags))
		var wg sync.WaitGroup
		for i, pt := range pipelineTags {
			wg.Add(1)
			go func(i int, pt string) {
				defer wg.Done()
				targetURL := baseURL + "&filter=" + formatFilter + "&pipeline_tag=" + pt
				models, err := s.fetchHFModelList(r.Context(), targetURL)
				outcomes[i] = fetchOutcome{models: models, err: err}
			}(i, pt)
		}
		wg.Wait()

		seen := make(map[string]bool, len(pipelineTags)*25)
		var lastErr error
		successCount := 0
		for _, o := range outcomes {
			if o.err != nil {
				lastErr = o.err
				continue
			}
			successCount++
			for _, m := range o.models {
				if seen[m.ID] {
					continue
				}
				seen[m.ID] = true
				hfModels = append(hfModels, m)
			}
		}
		if successCount == 0 {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, lastErr.Error()), http.StatusBadGateway)
			return
		}
		sortHFModelsInPlace(hfModels, sortField, direction)
		if len(hfModels) > 25 {
			hfModels = hfModels[:25]
		}
	}

	// Post-filter on fields the HF search API returns per-model but has no
	// query param for (min downloads/likes, created-date range). All of these
	// come from real HF response fields, never estimated.
	minDownloads, _ := strconv.Atoi(r.URL.Query().Get("min_downloads"))
	minLikes, _ := strconv.Atoi(r.URL.Query().Get("min_likes"))
	var createdAfter, createdBefore time.Time
	if v := r.URL.Query().Get("created_after"); v != "" {
		createdAfter, _ = time.Parse("2006-01-02", v)
	}
	if v := r.URL.Query().Get("created_before"); v != "" {
		createdBefore, _ = time.Parse("2006-01-02", v)
	}

	if minDownloads > 0 || minLikes > 0 || !createdAfter.IsZero() || !createdBefore.IsZero() {
		filtered := make([]HFModelInfo, 0, len(hfModels))
		for _, m := range hfModels {
			if minDownloads > 0 && m.Downloads < minDownloads {
				continue
			}
			if minLikes > 0 && m.Likes < minLikes {
				continue
			}
			if !createdAfter.IsZero() || !createdBefore.IsZero() {
				created, err := time.Parse(time.RFC3339, m.CreatedAt)
				if err == nil {
					if !createdAfter.IsZero() && created.Before(createdAfter) {
						continue
					}
					if !createdBefore.IsZero() && created.After(createdBefore.AddDate(0, 0, 1)) {
						continue
					}
				}
			}
			filtered = append(filtered, m)
		}
		hfModels = filtered
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(hfModels)
}

// handleModelRepo fetches variants and details for a specific repository.
// GET /admin/models/repo?id={repo_id}&node={node}&ctx={ctx} (also /admin/v1/models/repo)
func (s *Server) handleModelRepo(w http.ResponseWriter, r *http.Request) {
	repoID := r.URL.Query().Get("id")
	if repoID == "" {
		http.Error(w, `{"error":"missing id parameter"}`, http.StatusBadRequest)
		return
	}

	ctxLen := int64(8192) // default to 8k context window
	if cStr := r.URL.Query().Get("ctx"); cStr != "" {
		if cVal, err := strconv.ParseInt(cStr, 10, 64); err == nil && cVal > 0 {
			ctxLen = cVal
		}
	}

	nodeName := r.URL.Query().Get("node")
	runtime := r.URL.Query().Get("runtime")

	targetURL := fmt.Sprintf("https://huggingface.co/api/models/%s?blobs=true", repoID)
	req, err := http.NewRequestWithContext(r.Context(), "GET", targetURL, nil)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"create request: %s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	if s.cfg.HuggingFace.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.cfg.HuggingFace.Token)
	}

	resp, err := hfHTTPClient.Do(req)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"fetch from Hugging Face: %s"}`, err.Error()), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		http.Error(w, fmt.Sprintf(`{"error":"Hugging Face API returned status %d"}`, resp.StatusCode), http.StatusBadGateway)
		return
	}

	var repo HFRepoResponse
	if err := json.NewDecoder(resp.Body).Decode(&repo); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"decode Hugging Face response: %s"}`, err.Error()), http.StatusInternalServerError)
		return
	}

	// 1. Gather downloaded status map for the selected node
	downloaded := make(map[string]bool)
	vramFreeBytes := int64(0)
	vramTotalBytes := int64(0)
	vramSource := "unknown"
	diskFreeGB := float64(0)
	diskTotalGB := float64(0)
	agentPresent := false
	gpuCount := 0
	vramFitBasis := ""
	rawVramSource := ""

	nodes := s.router.Nodes()
	var targetNode *router.NodeState
	if nodeName != "" {
		for _, n := range nodes {
			if n.Name == nodeName {
				targetNode = n
				break
			}
		}
	} else if len(nodes) > 0 {
		targetNode = nodes[0]
	}

	dockerDeployed := false
	if targetNode != nil {
		targetNode.RLock()
		nodeURL := targetNode.URL
		nodeName := targetNode.Name
		vramTotalMB := targetNode.VRAMTotalMB
		agentGPUs := append([]nodeagent.GPUInfo(nil), targetNode.AgentGPUs...)
		declaredGPUIndices := append([]int(nil), targetNode.DeclaredGPUIndices...)
		vramUsedMBFromPS := int64(0)
		vramSource = targetNode.VRAMSource
		rawVramSource = targetNode.VRAMSource
		for _, m := range targetNode.LoadedModels {
			vramUsedMBFromPS += m.SizeVRAM / (1024 * 1024)
		}
		agentPresent = targetNode.AgentPresent
		diskFreeGB = targetNode.DiskFreeGB
		diskTotalGB = targetNode.DiskTotalGB
		targetNode.RUnlock()

		// The disk figures above are always this agent's *host* filesystem
		// (host_linux.go's readDiskStatsGB("/")) - for a Docker-controlled
		// node, the runtime's actual model storage can live on a separate,
		// differently-sized container volume the host reading knows nothing
		// about (the exact gap admin.go's handleNodePull pre-pull gate now
		// checks the container's real number for - see
		// containerDiskStatsViaAgent). dockerDeployed lets the UI flag that
		// gap on this figure explicitly rather than silently showing a
		// number that may not match what the container actually has, which
		// is exactly the operator confusion this field exists to prevent.
		if ctrl, configured := s.router.NodeControlSetting(nodeName); configured && ctrl.Driver == "docker" {
			dockerDeployed = true
		}

		capacityMB, capacityUsedMB, gc, vfb := nodeVRAMCapacity(vramTotalMB, agentGPUs, runtime, declaredGPUIndices)
		gpuCount = gc
		vramFitBasis = vfb
		if capacityMB > 0 {
			vramTotalBytes = capacityMB * 1024 * 1024
			usedMB := vramUsedMBFromPS
			if capacityUsedMB >= 0 {
				usedMB = capacityUsedMB
			}
			vramUsedBytes := usedMB * 1024 * 1024
			vramFreeBytes = vramTotalBytes - vramUsedBytes
			if vramFreeBytes < 0 {
				vramFreeBytes = 0
			}
		} else if vramUsedMBFromPS > 0 {
			vramSource = "inferred"
		} else {
			vramSource = "unknown"
		}

		if tagModels, err := s.router.FetchModelTags(nodeURL); err == nil {
			for _, tm := range tagModels {
				downloaded[tm.Name] = true
			}
		}
	}

	// 2. Filter siblings and build variants list. Ollama/llama.cpp load GGUF
	// quant files individually (one variant per file). vLLM/TGI load an
	// entire HF safetensors repo as a single unit, so we aggregate all
	// .safetensors sibling sizes into one variant instead.
	variants := []ModelVariantFit{}
	if ggufOnlyRuntime(runtime) {
		for _, sib := range repo.Siblings {
			lowerName := strings.ToLower(sib.Rfilename)
			if !strings.HasSuffix(lowerName, ".gguf") {
				continue
			}
			// mmproj-*.gguf is a multimodal vision-projector companion file,
			// not a quantization of the model itself - e.g. "mmproj-F16.gguf"
			// is a few hundred MB regardless of the main model's actual size,
			// but extractQuantization would still read "F16" out of its name
			// and offer it as if it were a legitimate (and wildly undersized)
			// "F16" variant of the model. Ollama's own `ollama pull` already
			// fetches a repo's mmproj file automatically alongside the main
			// GGUF when one exists - there is never a reason to list it here
			// as its own pullable variant.
			if strings.HasPrefix(lowerName, "mmproj") {
				continue
			}
			// A sharded GGUF quant (e.g. "Q3_K_S/Kimi-K2.5-Q3_K_S-00001-of-
			// 00010.gguf" - confirmed against unsloth/Kimi-K2.5-GGUF,
			// 2026-08-04) is split across multiple files the way a
			// safetensors repo is - but our variant list is one entry per
			// .gguf file with a single "Pull" button, which has no way to
			// fetch every part together. Ollama's own manifest resolution
			// explicitly refuses these outright ("The specified repository
			// contains sharded GGUF. Ollama does not support this yet.",
			// https://github.com/ollama/ollama/issues/5245) - and even where
			// that 400 wouldn't apply, one Pull click here would only ever
			// fetch one of many required parts, silently producing an
			// unloadable partial model rather than a clear error. Excluding
			// the shard files entirely (not just for ollama) means a user
			// never sees a Pull button that cannot possibly result in a
			// complete, loadable model through this flow.
			if shardedGGUFFilename.MatchString(lowerName) {
				continue
			}

			quant := extractQuantization(sib.Rfilename)
			sizeMB := sib.Size / (1024 * 1024)
			if sizeMB == 0 {
				continue // skip directories/metadata placeholder files
			}

			// Calculate estimated VRAM based on file size and context length
			// Formula: VRAMEstMB = sizeMB * 1.10 + ctxLen * 0.15
			vramEstMB := int64(float64(sizeMB)*1.10 + float64(ctxLen)*0.15)
			estBytes := vramEstMB * 1024 * 1024

			tag := fmt.Sprintf("hf.co/%s:%s", repo.ID, quant)
			if quant == "GGUF" {
				tag = fmt.Sprintf("hf.co/%s", repo.ID)
			}

			// Check downloaded status
			isDl := downloaded[tag] || downloaded[tag+":latest"]
			if !isDl {
				// Check case-insensitive base name cut match
				for k := range downloaded {
					if strings.EqualFold(k, tag) || strings.EqualFold(k, tag+":latest") {
						isDl = true
						break
					}
				}
			}

			variants = append(variants, ModelVariantFit{
				Tag:          tag,
				Quantization: quant,
				VRAMEstMB:    vramEstMB,
				SizeMB:       sizeMB,
				Fit:          classifyFit(estBytes, vramTotalBytes, vramSource),
				DiskFit:      classifyDiskFit(sizeMB, diskFreeGB, diskTotalGB, agentPresent),
				Downloaded:   isDl,
			})
		}
	} else {
		var totalSize int64
		hasSafetensors := false
		for _, sib := range repo.Siblings {
			if strings.HasSuffix(strings.ToLower(sib.Rfilename), ".safetensors") {
				hasSafetensors = true
				totalSize += sib.Size
			}
		}
		if hasSafetensors {
			sizeMB := totalSize / (1024 * 1024)
			// vLLM/TGI carry more runtime overhead (PagedAttention KV cache,
			// CUDA graph buffers) than llama.cpp, hence the higher multiplier.
			vramEstMB := int64(float64(sizeMB)*1.20 + float64(ctxLen)*0.20)
			estBytes := vramEstMB * 1024 * 1024
			isDl := downloaded[repo.ID]
			variants = append(variants, ModelVariantFit{
				Tag:          repo.ID,
				Quantization: detectSafetensorsQuant(repo.Tags),
				VRAMEstMB:    vramEstMB,
				SizeMB:       sizeMB,
				Fit:          classifyFit(estBytes, vramTotalBytes, vramSource),
				DiskFit:      classifyDiskFit(sizeMB, diskFreeGB, diskTotalGB, agentPresent),
				Downloaded:   isDl,
			})
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id":                repo.ID,
		"downloads":         repo.Downloads,
		"likes":             repo.Likes,
		"tags":              repo.Tags,
		"last_modified":     repo.LastModified,
		"variants":          variants,
		"disk_free_gb":      diskFreeGB,
		"disk_total_gb":     diskTotalGB,
		"disk_known":        agentPresent && diskTotalGB > 0,
		"docker_deployed":   dockerDeployed,
		"gpu_count":         gpuCount,
		"vram_fit_basis":    vramFitBasis,
		"gpu_count_unknown": gpuCountUnknown(agentPresent, rawVramSource, gpuCount),
	})
}

// extractQuantization extracts the quantization format from a GGUF filename.
func extractQuantization(filename string) string {
	lower := strings.ToLower(filename)
	// Remove .gguf extension
	name := strings.TrimSuffix(lower, ".gguf")

	// List of known quants sorted by length descending so we don't partially match (e.g. bf16 before f16)
	quants := []string{
		"q2_k_s", "q2_k", "q3_k_s", "q3_k_m", "q3_k_l", "q4_k_s", "q4_k_m", "q5_k_s", "q5_k_m", "q8_0", "q4_0", "q4_1", "q5_0", "q5_1", "q6_k", "q3_k",
		"iq1_s", "iq1_m", "iq2_xxs", "iq2_xs", "iq2_s", "iq2_m", "iq3_xxs", "iq3_xs", "iq3_s", "iq3_m", "iq4_xs", "iq4_nl",
		"bf16", "fp16", "f16", "f32",
	}

	for _, q := range quants {
		if strings.Contains(name, q) {
			return strings.ToUpper(q)
		}
	}

	// Fallback to searching with regex/suffix
	var suffix string
	if idx := strings.LastIndex(name, "-"); idx != -1 && idx+1 < len(name) {
		suffix = strings.ToUpper(name[idx+1:])
	} else if idx := strings.LastIndex(name, "_"); idx != -1 && idx+1 < len(name) {
		suffix = strings.ToUpper(name[idx+1:])
	}

	if suffix != "" {
		ignored := map[string]bool{
			"INSTRUCT": true, "CHAT": true, "LATEST": true, "PREVIEW": true, "BASE": true, "TEXT": true, "EMBED": true,
		}
		if ignored[suffix] || isParamCount(suffix) {
			return "GGUF"
		}
		return suffix
	}

	return "GGUF"
}

func isParamCount(s string) bool {
	if len(s) < 2 {
		return false
	}
	lastChar := s[len(s)-1]
	if lastChar != 'B' {
		return false
	}
	for i := 0; i < len(s)-1; i++ {
		c := s[i]
		if (c < '0' || c > '9') && c != '.' {
			return false
		}
	}
	return true
}
