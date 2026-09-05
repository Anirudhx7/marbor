package marboragent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
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
type ParallelismInfo struct {
	Type  string `json:"type,omitempty"`  // tp|pp|ep|dp
	Width int    `json:"width,omitempty"` // 1..64
}

// DeploymentReport is one deployment instance the agent observed on the host.
// Port keys it to a runtime instance (same port as RuntimeInfo.Port), so a
// host running two vLLM on :8000 and :8001 with different TP widths does
// not fan a single host-level report to both nodes. Server fans by
// pinnedID/port match (agent_poll.go:184/343 pattern), not blind Host.
// Additive: new Marbor ignores unknown fields, old agent omits this block.
type DeploymentReport struct {
	Runtime     string           `json:"runtime,omitempty"`    // vllm|sglang|tgi|llamacpp|mlx|ollama
	Port        int              `json:"port,omitempty"`       // runtime's listening port, 0 if unknown
	RuntimeID   string           `json:"runtime_id,omitempty"` // stable ID from runtime_registry when known
	GPUGroup    []int            `json:"gpu_group,omitempty"`  // CUDA_VISIBLE_DEVICES indices or fallback len(GPUs)
	Parallelism *ParallelismInfo `json:"parallelism,omitempty"`
	Caps        *RuntimeCaps     `json:"capabilities,omitempty"`
	Source      string           `json:"source,omitempty"` // ps|docker|env|fallback|unknown
}

// parallelArgsREs matches runtime-specific parallelism flags in a process
// command line. Keep minimal - only flags actually observed in real fleets.
// No new flags without evidence.
var (
	vllmTPRE            = regexp.MustCompile(`--tensor-parallel-size[ =]+(\d+)`)
	sglangTPRE          = regexp.MustCompile(`--tp[ =]+(\d+)|--tensor-parallel-size[ =]+(\d+)`)
	tgiShardRE          = regexp.MustCompile(`--num-shard[ =]+(\d+)|--sharded[ =]+(true|false)`)
	llamaGPULayersRE    = regexp.MustCompile(`--gpu-layers[ =]+(\d+)`)
	dockerContainerIDRE = regexp.MustCompile(`^[0-9a-fA-F]{12,64}$`)
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
		if m := vllmTPRE.FindStringSubmatch(args); m != nil {
			w := firstNonEmptyInt(m[1:])
			if w > 0 {
				caps.TP = true
				caps.MaxTPWidth = w
				return &ParallelismInfo{Type: "tp", Width: w}, &caps
			}
		}
		if strings.Contains(args, "vllm") {
			caps.TP = true
		}
	case "sglang":
		if m := sglangTPRE.FindStringSubmatch(args); m != nil {
			w := firstNonEmptyInt(m[1:])
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
	case "llamacpp":
		if m := llamaGPULayersRE.FindStringSubmatch(args); m != nil {
			_ = m
			caps.TP = false
		}
	}
	if caps.TP || caps.PP || caps.EP || caps.DP {
		return nil, &caps
	}
	return nil, nil
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

func firstNonEmptyInt(strs []string) int {
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

func detectRuntimeFromArgs(args string) string {
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

// parseGPUGroupFromEnv parses CUDA_VISIBLE_DEVICES and vendor equivalents.
// Env string is "0,1,2" or "0,1" - returns []int or nil if not set/invalid.
// Never fabricates: empty/invalid -> nil.
func parseGPUGroupFromEnv(envVal string) []int {
	envVal = strings.TrimSpace(envVal)
	if envVal == "" {
		return nil
	}
	parts := strings.Split(envVal, ",")
	var out []int
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if v, err := strconv.Atoi(p); err == nil {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// envGPUGroup tries CUDA_VISIBLE_DEVICES and vendor equivalents in priority
// order. Returns group and the env key that provided it, or nil.
func envGPUGroup(getenv func(string) string) ([]int, string) {
	for _, key := range []string{"CUDA_VISIBLE_DEVICES", "ROCR_VISIBLE_DEVICES", "XPU_VISIBLE_DEVICES", "HSA_OVERRIDE_GFX_VERSION"} {
		if key == "HSA_OVERRIDE_GFX_VERSION" {
			continue // not a GPU index list - skip for group parsing
		}
		if v := getenv(key); v != "" {
			if g := parseGPUGroupFromEnv(v); g != nil {
				return g, key
			}
		}
	}
	return nil, ""
}

// collectFromPS parses ps output for runtime flags. psOutput is the full
// `ps -eo args` stdout (one process per line). gpuCount is fallback len.
// runtimeHint maps port -> runtime name from detected list to avoid
// re-detecting runtime from args alone when we already know it.
func collectFromPS(psOutput string, detected []DetectedRuntime, gpuCount int, getenv func(string) string) []DeploymentReport {
	var reports []DeploymentReport
	envGroup, envKey := envGPUGroup(getenv)
	lines := strings.Split(psOutput, "\n")
	// Build port->runtime map for hinting
	portToRuntime := make(map[int]string)
	for _, d := range detected {
		if d.Port > 0 {
			portToRuntime[d.Port] = d.Name
		}
	}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Heuristic: only consider lines that look like inference runtimes
		lower := strings.ToLower(line)
		if !strings.Contains(lower, "vllm") && !strings.Contains(lower, "sglang") && !strings.Contains(lower, "tgi") && !strings.Contains(lower, "llama") && !strings.Contains(lower, "ollama") {
			continue
		}
		runtimeHint := detectRuntimeFromArgs(lower)
		if runtimeHint == "" {
			continue
		}
		par, caps := parseParallelismFromArgs(runtimeHint, line)
		// Extract port if present in args e.g. --port 8000
		port := extractPortFromArgs(line)
		// If we have a detected runtime on same port, prefer its Name
		if port > 0 {
			if name, ok := portToRuntime[port]; ok {
				runtimeHint = name
			}
		} else if len(detected) == 1 {
			// Single runtime host - attribute lone detection's port
			port = detected[0].Port
			if detected[0].Name != "" {
				runtimeHint = detected[0].Name
			}
		}
		var gpuGroup []int
		if envGroup != nil {
			gpuGroup = append([]int(nil), envGroup...)
			_ = envKey
		} else if gpuCount > 0 {
			// Fallback len: use 0..gpuCount-1 as group
			gpuGroup = make([]int, gpuCount)
			for i := range gpuGroup {
				gpuGroup[i] = i
			}
		}
		rep := DeploymentReport{
			Runtime:     runtimeHint,
			Port:        port,
			GPUGroup:    gpuGroup,
			Parallelism: par,
			Caps:        caps,
			Source:      "ps",
		}
		// Deduplicate by port+runtime
		dup := false
		for i, existing := range reports {
			if existing.Port == rep.Port && existing.Runtime == rep.Runtime {
				// Keep one with parallelism if existing had none
				if existing.Parallelism == nil && rep.Parallelism != nil {
					reports[i] = rep
				}
				dup = true
				break
			}
		}
		if !dup {
			reports = append(reports, rep)
		}
	}
	return reports
}

func extractPortFromArgs(args string) int {
	// matches --port 8000, --port=8000, -p 8000
	re := regexp.MustCompile(`--port[ =]+(\d+)|-p[ =]+(\d+)`)
	if m := re.FindStringSubmatch(args); m != nil {
		return firstInt(m[1], m[2])
	}
	return 0
}

// dockerContainer holds the minimal fields we need from docker inspect.
type dockerContainer struct {
	Config struct {
		Env   []string `json:"Env"`
		Cmd   []string `json:"Cmd"`
		Image string   `json:"Image"`
	} `json:"Config"`
	Args            []string `json:"Args"`
	NetworkSettings struct {
		Ports map[string][]struct {
			HostPort string `json:"HostPort"`
		} `json:"Ports"`
	} `json:"NetworkSettings"`
}

// parseDockerInspect parses a single docker inspect JSON blob for deployment info.
func parseDockerInspect(inspectJSON string, gpuCount int, getenv func(string) string) *DeploymentReport {
	var c dockerContainer
	if err := json.Unmarshal([]byte(inspectJSON), &c); err != nil {
		return nil
	}
	// Build env map
	envMap := make(map[string]string)
	for _, e := range c.Config.Env {
		if idx := strings.Index(e, "="); idx > 0 {
			envMap[e[:idx]] = e[idx+1:]
		}
	}
	// GPU group from container env
	var gpuGroup []int
	if v, ok := envMap["CUDA_VISIBLE_DEVICES"]; ok {
		gpuGroup = parseGPUGroupFromEnv(v)
	}
	if gpuGroup == nil {
		if v, ok := envMap["ROCR_VISIBLE_DEVICES"]; ok {
			gpuGroup = parseGPUGroupFromEnv(v)
		}
	}
	if gpuGroup == nil {
		if v, ok := envMap["XPU_VISIBLE_DEVICES"]; ok {
			gpuGroup = parseGPUGroupFromEnv(v)
		}
	}
	if gpuGroup == nil && gpuCount > 0 {
		gpuGroup = make([]int, gpuCount)
		for i := range gpuGroup {
			gpuGroup[i] = i
		}
	}
	// Args string
	argsStr := strings.Join(append(c.Config.Cmd, c.Args...), " ")
	if argsStr == "" {
		argsStr = c.Config.Image
	}
	runtimeHint := detectRuntimeFromArgs(strings.ToLower(argsStr + " " + c.Config.Image))
	if runtimeHint == "" {
		// Try env for runtime hint
		if img := strings.ToLower(c.Config.Image); strings.Contains(img, "vllm") {
			runtimeHint = "vllm"
		} else if strings.Contains(img, "tgi") {
			runtimeHint = "tgi"
		}
	}
	if runtimeHint == "" {
		return nil
	}
	par, caps := parseParallelismFromArgs(runtimeHint, argsStr)
	// Port from NetworkSettings or args
	port := extractPortFromArgs(argsStr)
	if port == 0 {
		for _, bindings := range c.NetworkSettings.Ports {
			for _, b := range bindings {
				if p, err := strconv.Atoi(b.HostPort); err == nil && p > 0 {
					port = p
					break
				}
			}
		}
	}
	return &DeploymentReport{
		Runtime:     runtimeHint,
		Port:        port,
		GPUGroup:    gpuGroup,
		Parallelism: par,
		Caps:        caps,
		Source:      "docker",
	}
}

// CollectDeployments is the agent-side deployment detector used by
// scheduler.refresh. It tries: 1) ps 2) docker.sock 3) env 4) fallback nil.
// Never fabricates: on error or when host PID namespace is isolated, returns
// nil (server shows "unknown - add docker.sock or run agent on host").
// gpuBlock provides fallback len when no env is present.
func CollectDeployments(detected []DetectedRuntime, gpuBlock *GPUBlock) []DeploymentReport {
	gpuCount := 0
	if gpuBlock != nil {
		gpuCount = gpuBlock.Count
		if gpuCount == 0 && len(gpuBlock.Devices) > 0 {
			gpuCount = len(gpuBlock.Devices)
		}
	}
	getenv := os.Getenv
	// 1) Try ps
	if psOut, err := runPS(); err == nil && strings.TrimSpace(psOut) != "" {
		if reps := collectFromPS(psOut, detected, gpuCount, getenv); len(reps) > 0 {
			// Fill any missing parallelism with env fallback width? No - keep nil, never fabricate.
			return reps
		}
		// ps succeeded but found no runtime lines - could be docker-isolated PID ns
		// fall through to docker attempt rather than returning empty (so we can show unknown vs empty correctly)
	}
	// 2) Try docker.sock via Unix socket
	if reps := collectFromDockerSocket(detected, gpuCount); len(reps) > 0 {
		return reps
	}
	// 3) Try env-only fallback: if we have detected runtimes and an env group
	if envGroup, src := envGPUGroup(getenv); envGroup != nil && len(detected) > 0 {
		var out []DeploymentReport
		for _, d := range detected {
			// Try to infer parallelism from env? No flag -> caps only
			out = append(out, DeploymentReport{
				Runtime:  d.Name,
				Port:     d.Port,
				GPUGroup: append([]int(nil), envGroup...),
				Source:   "env:" + src,
			})
		}
		return out
	}
	// 4) Fallback nil - server treats as unknown, fail-open
	return nil
}

// runPS executes `ps -eo args` (or `ps aux` fallback) and returns stdout.
// Isolated for test override via psExec var.
var psExec = func() (string, error) {
	// Try Linux `ps -eo args` first, then busybox/OSX `ps aux`
	for _, cmd := range [][]string{{"ps", "-eo", "args"}, {"ps", "aux"}} {
		out, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput()
		if err == nil {
			return string(out), nil
		}
	}
	return "", fmt.Errorf("ps failed")
}

func runPS() (string, error) { return psExec() }

// collectFromDockerSocket queries /var/run/docker.sock if present.
// Returns nil if socket not present or no deployments found (unknown, not fabricated).
func collectFromDockerSocket(detected []DetectedRuntime, gpuCount int) []DeploymentReport {
	sock := "/var/run/docker.sock"
	if _, err := os.Stat(sock); err != nil {
		return nil
	}
	// Build HTTP client over Unix socket
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "unix", sock)
			},
		},
		Timeout: 3 * time.Second,
	}
	// List containers
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/containers/json", nil)
	if err != nil {
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var list []struct {
		Id string `json:"Id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil
	}
	var reports []DeploymentReport
	for _, c := range list {
		if !dockerContainerIDRE.MatchString(c.Id) {
			continue
		}
		ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
		r2, _ := http.NewRequestWithContext(ctx2, http.MethodGet, "http://docker/containers/"+c.Id+"/json", nil)
		rs, err := client.Do(r2)
		cancel2()
		if err != nil || rs.StatusCode != http.StatusOK {
			if rs != nil {
				rs.Body.Close()
			}
			continue
		}
		body, err := io.ReadAll(io.LimitReader(rs.Body, 256*1024))
		rs.Body.Close()
		if err != nil {
			continue
		}
		rep := parseDockerInspect(string(body), gpuCount, os.Getenv)
		if rep != nil {
			reports = append(reports, *rep)
		}
	}
	// Deduplicate and map to detected ports where possible
	if len(reports) == 0 {
		return nil
	}
	// Keep only reports matching detected runtimes where possible
	if len(detected) > 0 {
		portSet := make(map[int]bool)
		for _, d := range detected {
			if d.Port > 0 {
				portSet[d.Port] = true
			}
		}
		if len(portSet) > 0 {
			var filtered []DeploymentReport
			for _, r := range reports {
				if r.Port != 0 && portSet[r.Port] {
					filtered = append(filtered, r)
				} else if r.Port == 0 {
					filtered = append(filtered, r)
				}
			}
			if len(filtered) > 0 {
				return filtered
			}
		}
	}
	return reports
}

// CollectDeploymentForTest exports collectFromPS with injectable ps output for unit tests.
// Kept as top-level func so tests don't need to mock exec.
func CollectDeploymentForTest(psOutput string, detected []DetectedRuntime, gpuCount int, env map[string]string) []DeploymentReport {
	getenv := func(k string) string { return env[k] }
	return collectFromPS(psOutput, detected, gpuCount, getenv)
}

// ParseDockerInspectForTest exports parseDockerInspect for unit tests.
func ParseDockerInspectForTest(inspectJSON string, gpuCount int) *DeploymentReport {
	return parseDockerInspect(inspectJSON, gpuCount, os.Getenv)
}

// ParseGPUGroupForTest exports parseGPUGroupFromEnv for unit tests.
func ParseGPUGroupForTest(env string) []int { return parseGPUGroupFromEnv(env) }

// ParseParallelismForTest exports parseParallelismFromArgs for unit tests.
func ParseParallelismForTest(runtimeHint, args string) (*ParallelismInfo, *RuntimeCaps) {
	return parseParallelismFromArgs(runtimeHint, args)
}
