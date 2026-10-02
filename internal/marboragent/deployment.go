package marboragent

import (
	"regexp"
	"strconv"
	"strings"
)

// RuntimeCaps describes what parallelism modes a runtime build is capable of.
// Structured, not map[string]any, so future PP/EP/DP additions are type-safe
// and dark-variant parity is explicit. Additive only - new caps add new
// fields, never reinterpret existing ones.
type RuntimeCaps struct {
	TP         bool `json:"tp,omitempty"`
	PP         bool `json:"pp,omitempty"`
	EP         bool `json:"ep,omitempty"`
	DP         bool `json:"dp,omitempty"`
	MaxTPWidth int  `json:"max_tp_width,omitempty"`
}

// ParallelismInfo is the detected deployment shape for one runtime instance.
// Type/Width carry the primary dimension (tensor parallel when present, else
// pipeline, else data). PipelineWidth/DataWidth carry the other dimensions
// when the command line also sets them, so a tensor=4 x pipeline=2 launch is
// not reported as a plain 4-wide one. Both are zero when the flag is absent.
type ParallelismInfo struct {
	Type          string `json:"type,omitempty"`           // tp|pp|ep|dp
	Width         int    `json:"width,omitempty"`          // 1..64
	PipelineWidth int    `json:"pipeline_width,omitempty"` // secondary pp degree, 0 = not set
	DataWidth     int    `json:"data_width,omitempty"`     // secondary dp degree, 0 = not set
}

// DeploymentReport is one deployment instance the agent observed on the host.
// Port keys it to a runtime instance (same port as RuntimeInfo.Port), so a
// host running two vLLM on :8000 and :8001 with different TP widths does
// not fan a single host-level report to both nodes. Server fans by
// pinnedID/port match (agent_poll.go pattern), not blind Host.
// Additive: new Marbor ignores unknown fields, old agent omits this block.
type DeploymentReport struct {
	Runtime   string `json:"runtime,omitempty"`    // vllm|sglang|tgi|llamacpp|mlx|ollama
	Port      int    `json:"port,omitempty"`       // runtime's listening port, 0 if unknown
	RuntimeID string `json:"runtime_id,omitempty"` // stable ID from runtime_registry when known
	// GPUGroup is the legacy device list kept for older marbor versions. It is
	// filled only when the scope is a clean list of device indices (or was
	// resolved to indices by a per-process check); it is never a truncated
	// view of a mixed list and never a guess at "all GPUs".
	GPUGroup []int `json:"gpu_group,omitempty"`
	// GPUScope is the full, honest record of which devices this runtime was
	// pointed at and how that was learned. Absent means unknown.
	GPUScope    *GPUScope        `json:"gpu_scope,omitempty"`
	Parallelism *ParallelismInfo `json:"parallelism,omitempty"`
	Caps        *RuntimeCaps     `json:"capabilities,omitempty"`
	// Source says where the parallelism shape came from: ps|docker. It does
	// not describe the GPU scope; GPUScope.Source does that.
	Source string `json:"source,omitempty"`
	// Topology is multi-host launch evidence, present only when the launch
	// itself states distributed intent. Absent means none was found or it
	// could not be read, never a single-node claim.
	Topology *Topology `json:"topology,omitempty"`
}

// parallelArgsREs matches runtime-specific parallelism flags in a process
// command line. Keep minimal - only flags actually observed in real fleets.
// No new flags without evidence.
var (
	vllmTPRE            = regexp.MustCompile(`--tensor-parallel-size[ =]+(\d+)`)
	vllmPPRE            = regexp.MustCompile(`--pipeline-parallel-size[ =]+(\d+)`)
	vllmDPRE            = regexp.MustCompile(`--data-parallel-size[ =]+(\d+)`)
	sglangTPRE          = regexp.MustCompile(`--tp[ =]+(\d+)|--tensor-parallel-size[ =]+(\d+)`)
	tgiShardRE          = regexp.MustCompile(`--num-shard[ =]+(\d+)|--sharded[ =]+(true|false)`)
	dockerContainerIDRE = regexp.MustCompile(`^[0-9a-fA-F]{12,64}$`)
	portArgRE           = regexp.MustCompile(`--port[ =]+(\d+)|-p[ =]+(\d+)`)
)

// parseParallelismFromArgs extracts parallelism from a single process arg string.
// Returns type+width when a runtime flag is present, else nil. Caps always
// reflects that tp was seen (even when width parse fails, caps still shows tp capability).
func parseParallelismFromArgs(runtimeHint, args string) (*ParallelismInfo, *RuntimeCaps) {
	args = strings.ToLower(args)
	var caps RuntimeCaps
	switch runtimeHint {
	case "vllm", "sglang", "tgi", "llamacpp", "mlx", "ollama":
	default:
		runtimeHint = detectRuntimeFromArgs(args)
	}
	switch runtimeHint {
	case "vllm":
		return parseVLLMParallelism(args, &caps)
	case "sglang":
		if m := sglangTPRE.FindStringSubmatch(args); m != nil {
			w := firstInt(m[1:]...)
			// sglang uses --tp for tensor parallel
			if w > 0 {
				caps.TP = true
				caps.MaxTPWidth = w
				return &ParallelismInfo{Type: "tp", Width: w}, &caps
			}
		}
		if strings.Contains(args, "sglang") {
			caps.TP = true
		}
	case "tgi":
		if m := tgiShardRE.FindStringSubmatch(args); m != nil {
			w := firstInt(m[1])
			if w > 0 {
				caps.TP = true
				caps.MaxTPWidth = w
				return &ParallelismInfo{Type: "tp", Width: w}, &caps
			}
			if strings.Contains(m[0], "sharded") {
				caps.TP = true
			}
		}
	}
	if caps.TP || caps.PP || caps.EP || caps.DP {
		return nil, &caps
	}
	return nil, nil
}

// parseVLLMParallelism reads vLLM's tensor, pipeline and data parallel sizes.
// args must already be lower-cased.
func parseVLLMParallelism(args string, caps *RuntimeCaps) (*ParallelismInfo, *RuntimeCaps) {
	tp := flagWidth(vllmTPRE, args)
	pp := flagWidth(vllmPPRE, args)
	dp := flagWidth(vllmDPRE, args)
	if tp > 0 {
		caps.TP = true
		caps.MaxTPWidth = tp
	}
	caps.PP = pp > 0
	caps.DP = dp > 0
	var info *ParallelismInfo
	switch {
	case tp > 0:
		info = &ParallelismInfo{Type: "tp", Width: tp, PipelineWidth: pp, DataWidth: dp}
	case pp > 0:
		info = &ParallelismInfo{Type: "pp", Width: pp, DataWidth: dp}
	case dp > 0:
		info = &ParallelismInfo{Type: "dp", Width: dp}
	}
	if info != nil {
		return info, caps
	}
	if strings.Contains(args, "vllm") {
		caps.TP = true
	}
	if caps.TP || caps.PP || caps.DP {
		return nil, caps
	}
	return nil, nil
}

func flagWidth(re *regexp.Regexp, args string) int {
	if m := re.FindStringSubmatch(args); m != nil {
		return firstInt(m[1:]...)
	}
	return 0
}

func firstInt(strs ...string) int {
	for _, s := range strs {
		if s == "" {
			continue
		}
		if v, err := strconv.Atoi(s); err == nil {
			return v
		}
	}
	return 0
}

// argv0Runtime recognises a runtime from the executable name alone: an exact
// match on the basename of the first argument (".exe" stripped), never a
// substring, so "llama-server-proxy" and a model path that happens to contain
// a runtime name do not count. Only llama-server is recognised; llama-cli is
// interactive and serves no port.
func argv0Runtime(args string) string {
	f := strings.Fields(args)
	if len(f) == 0 {
		return ""
	}
	name := f[0]
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	if strings.TrimSuffix(strings.ToLower(name), ".exe") == "llama-server" {
		return "llamacpp"
	}
	return ""
}

func detectRuntimeFromArgs(args string) string {
	// The executable name is checked first so a llama-server whose model path
	// mentions another runtime is still classified as llama.cpp.
	if rt := argv0Runtime(args); rt != "" {
		return rt
	}
	switch {
	case strings.Contains(args, "vllm"):
		return "vllm"
	case strings.Contains(args, "sglang"):
		return "sglang"
	case strings.Contains(args, "tgi") || strings.Contains(args, "text-generation"):
		return "tgi"
	case strings.Contains(args, "llama") && strings.Contains(args, "cpp"):
		return "llamacpp"
	case strings.Contains(args, "mlx"):
		return "mlx"
	case strings.Contains(args, "ollama"):
		return "ollama"
	}
	return ""
}

func extractPortFromArgs(args string) int {
	// matches --port 8000, --port=8000, -p 8000
	if m := portArgRE.FindStringSubmatch(args); m != nil {
		return firstInt(m[1], m[2])
	}
	return 0
}
