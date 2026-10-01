package marboragent

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// GPUScope records which GPUs one runtime instance was pointed at, how the
// agent learned that, and whether an independent per-process check agreed.
// It is always evidence with provenance, never a guess: when the agent could
// not learn the scope it sends no GPUScope at all (or one whose Note says why).
type GPUScope struct {
	// Indices are device indices: either the plain integers in the visibility
	// variable, or (when CrossChecked) the indices resolved from the GPU
	// process list.
	Indices []int `json:"indices,omitempty"`
	// UUIDs holds every identifier that is not a plain integer, exactly as
	// the operator wrote it: GPU UUIDs (full or abbreviated), MIG ids and
	// sub-device masks. Nothing in the variable is dropped.
	UUIDs []string `json:"uuids,omitempty"`
	// Raw is the exact visibility value read from the runtime's environment.
	Raw string `json:"raw,omitempty"`
	// Source names where the value came from, for example
	// "environ:CUDA_VISIBLE_DEVICES", "docker-env:CUDA_VISIBLE_DEVICES" or
	// "nvidia-compute-apps".
	Source string `json:"source,omitempty"`
	// CrossChecked is true only when the GPUs the runtime's process tree
	// actually holds (from the NVIDIA process list) equal the GPUs the
	// environment names. Never set from a single source.
	CrossChecked bool `json:"cross_checked,omitempty"`
	// Note explains an unknown, unverified or mismatched result in plain text.
	Note string `json:"note,omitempty"`
}

// scopeEnvKeys is the complete allowlist of environment variables the agent
// extracts from another process or container. Every other variable (tokens,
// API keys, paths) is discarded in memory and never stored, logged or sent.
// Order is priority: the first variable that is present wins.
var scopeEnvKeys = []string{
	"CUDA_VISIBLE_DEVICES",   // NVIDIA: indices, UUIDs, MIG ids
	"NVIDIA_VISIBLE_DEVICES", // NVIDIA container toolkit: indices, UUIDs, all, none, void
	"ROCR_VISIBLE_DEVICES",   // AMD ROCm runtime (Linux)
	"HIP_VISIBLE_DEVICES",    // AMD HIP (Windows and some Linux setups)
	"ZE_AFFINITY_MASK",       // Intel Level Zero
}

// nvidiaScopeKeys are the variables whose values can be checked against the
// NVIDIA process list.
var nvidiaScopeKeys = map[string]bool{"CUDA_VISIBLE_DEVICES": true, "NVIDIA_VISIBLE_DEVICES": true}

// allowlistedEnv extracts only the scope variables from a /proc/<pid>/environ
// blob (NUL separated KEY=VALUE entries).
func allowlistedEnv(environ []byte) map[string]string {
	return allowlistedEnvList(strings.Split(string(environ), "\x00"))
}

// allowlistedEnvList extracts only the scope variables from KEY=VALUE entries.
func allowlistedEnvList(entries []string) map[string]string {
	out := make(map[string]string)
	for _, e := range entries {
		idx := strings.Index(e, "=")
		if idx <= 0 {
			continue
		}
		key := e[:idx]
		for _, allowed := range scopeEnvKeys {
			if key == allowed {
				out[key] = e[idx+1:]
				break
			}
		}
	}
	return out
}

// scopeFromEnv builds the scope for a runtime from an already-allowlisted
// environment. sourcePrefix is "environ:" or "docker-env:". It returns nil
// when no scope variable is present at all. legacy is the value for the old
// gpu_group field (see DeploymentReport.GPUGroup).
func scopeFromEnv(env map[string]string, sourcePrefix string) (scope *GPUScope, legacy []int) {
	for _, key := range scopeEnvKeys {
		if v, ok := env[key]; ok {
			sc, g := parseGPUScope(sourcePrefix, key, v)
			return &sc, g
		}
	}
	return nil, nil
}

// parseGPUScope parses one visibility variable. Plain integers become
// Indices; every other token (UUID, MIG id, sub-device mask) is kept verbatim
// in UUIDs, so a mixed list is never silently truncated. legacy is non-nil
// only when every token is a plain index.
func parseGPUScope(sourcePrefix, key, raw string) (scope GPUScope, legacy []int) {
	raw = strings.TrimSpace(raw)
	scope = GPUScope{Raw: raw, Source: sourcePrefix + key}
	switch strings.ToLower(raw) {
	case "":
		scope.Note = key + " is set but empty, which means no GPU is visible to this runtime"
		return scope, nil
	case "all":
		scope.Note = key + "=all: every GPU is visible, no specific subset"
		return scope, nil
	case "none", "void", "nodevfiles":
		scope.Note = key + "=" + raw + ": no GPU is visible to this runtime"
		return scope, nil
	}
	allIndices := true
	for _, tok := range strings.Split(raw, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if v, err := strconv.Atoi(tok); err == nil && v >= 0 && isPlainInt(tok) {
			scope.Indices = append(scope.Indices, v)
			continue
		}
		allIndices = false
		scope.UUIDs = append(scope.UUIDs, tok)
	}
	if allIndices && len(scope.Indices) > 0 {
		legacy = append([]int(nil), scope.Indices...)
	}
	return scope, legacy
}

func isPlainInt(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// commandRunner runs an external command and returns its stdout. Injectable
// so tests use a fake nvidia-smi instead of real hardware.
type commandRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

// nvidiaProbe is one snapshot of the NVIDIA per-process view: which GPU UUIDs
// each PID holds, and which UUID sits at each nvidia-smi index.
type nvidiaProbe struct {
	pidUUIDs  map[int][]string
	indexUUID map[int]string
}

const nvidiaProbeTimeout = 3 * time.Second

// probeNVIDIA queries nvidia-smi for the compute process list and the
// index-to-UUID map. An error (nvidia-smi missing, driver down) means "cannot
// cross-check", which callers must treat as unverified, not as empty.
func probeNVIDIA(run commandRunner) (*nvidiaProbe, error) {
	ctx, cancel := context.WithTimeout(context.Background(), nvidiaProbeTimeout)
	defer cancel()
	gpuOut, err := run(ctx, "nvidia-smi", "--query-gpu=index,gpu_uuid", "--format=csv,noheader,nounits")
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi gpu query: %w", err)
	}
	appsOut, err := run(ctx, "nvidia-smi", "--query-compute-apps=pid,gpu_uuid", "--format=csv,noheader,nounits")
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi compute-apps query: %w", err)
	}
	p := &nvidiaProbe{pidUUIDs: map[int][]string{}, indexUUID: map[int]string{}}
	for _, row := range csvPairs(string(gpuOut)) {
		idx, err := strconv.Atoi(row[0])
		if err != nil {
			continue
		}
		p.indexUUID[idx] = row[1]
	}
	if len(p.indexUUID) == 0 {
		return nil, fmt.Errorf("nvidia-smi reported no GPUs")
	}
	for _, row := range csvPairs(string(appsOut)) {
		pid, err := strconv.Atoi(row[0])
		if err != nil {
			continue
		}
		p.pidUUIDs[pid] = append(p.pidUUIDs[pid], row[1])
	}
	return p, nil
}

// csvPairs splits "a, b" lines into two trimmed fields, skipping blank and
// malformed lines.
func csvPairs(out string) [][2]string {
	var rows [][2]string
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), ",", 2)
		if len(parts) != 2 {
			continue
		}
		a, b := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		if a == "" || b == "" {
			continue
		}
		rows = append(rows, [2]string{a, b})
	}
	return rows
}

// observedUUIDs is the set of GPU UUIDs held by any of pids.
func (p *nvidiaProbe) observedUUIDs(pids []int) map[string]bool {
	seen := map[string]bool{}
	for _, pid := range pids {
		for _, u := range p.pidUUIDs[pid] {
			seen[u] = true
		}
	}
	return seen
}

func (p *nvidiaProbe) indicesFor(uuids map[string]bool) []int {
	var out []int
	for idx, u := range p.indexUUID {
		if uuids[u] {
			out = append(out, idx)
		}
	}
	sort.Ints(out)
	return out
}

// resolveScopeUUIDs turns an environment scope into the set of full GPU UUIDs
// it names. ok is false when any entry cannot be resolved (a MIG id, a
// sub-device mask, an unknown index or UUID), in which case no cross-check is
// possible.
func (p *nvidiaProbe) resolveScopeUUIDs(sc *GPUScope) (map[string]bool, bool) {
	want := map[string]bool{}
	for _, idx := range sc.Indices {
		u, ok := p.indexUUID[idx]
		if !ok {
			return nil, false
		}
		want[u] = true
	}
	for _, tok := range sc.UUIDs {
		if !strings.HasPrefix(strings.ToUpper(tok), "GPU-") {
			return nil, false
		}
		matched := ""
		for _, full := range p.indexUUID {
			if strings.HasPrefix(strings.ToLower(full), strings.ToLower(tok)) {
				if matched != "" {
					return nil, false // ambiguous abbreviation
				}
				matched = full
			}
		}
		if matched == "" {
			return nil, false
		}
		want[matched] = true
	}
	return want, len(want) > 0
}

func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func joinInts(v []int) string {
	parts := make([]string, len(v))
	for i, n := range v {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ", ")
}

// applyCrossCheck compares an environment-derived scope with the GPUs the
// runtime's processes actually hold. It returns the scope to report and the
// legacy gpu_group value. Only NVIDIA visibility variables are checked; a
// disagreement is reported in Note and the value stays unverified, never
// silently resolved in either direction.
func applyCrossCheck(sc *GPUScope, legacy []int, probe *nvidiaProbe, pids []int) (*GPUScope, []int) {
	if probe == nil {
		return sc, legacy
	}
	observed := probe.observedUUIDs(pids)
	if sc == nil {
		if len(observed) == 0 {
			return nil, nil
		}
		return &GPUScope{
			Indices: probe.indicesFor(observed),
			Source:  "nvidia-compute-apps",
			Note:    "observed from the GPU process list; not confirmed against the runtime environment",
		}, nil
	}
	keyIsNVIDIA := false
	for k := range nvidiaScopeKeys {
		if strings.HasSuffix(sc.Source, ":"+k) {
			keyIsNVIDIA = true
		}
	}
	if !keyIsNVIDIA {
		return sc, legacy
	}
	want, ok := probe.resolveScopeUUIDs(sc)
	if !ok {
		return sc, legacy
	}
	if len(observed) == 0 {
		sc.Note = "no GPU process found for this runtime yet; scope not verified"
		return sc, legacy
	}
	if !sameSet(want, observed) {
		sc.Note = fmt.Sprintf("the GPU process list shows this runtime on GPU %s, but its environment names GPU %s; not verified",
			joinInts(probe.indicesFor(observed)), joinInts(probe.indicesFor(want)))
		return sc, legacy
	}
	sc.CrossChecked = true
	sc.Indices = probe.indicesFor(want)
	return sc, append([]int(nil), sc.Indices...)
}
