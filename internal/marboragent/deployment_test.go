package marboragent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestParseGPUScope(t *testing.T) {
	tests := []struct {
		name        string
		raw         string
		wantIdx     []int
		wantUUIDs   []string
		wantLegacy  []int // what gpu_group (legacy) must hold
		wantNoteSet bool
	}{
		{"indices", "0,1,2,3", []int{0, 1, 2, 3}, nil, []int{0, 1, 2, 3}, false},
		{"single", "0", []int{0}, nil, []int{0}, false},
		{"spaces", " 0, 1 , 2 ", []int{0, 1, 2}, nil, []int{0, 1, 2}, false},
		{"empty entries skipped", "0,,1", []int{0, 1}, nil, []int{0, 1}, false},
		{"uuids kept not dropped", "GPU-8932f937,GPU-1a2b3c4d", nil, []string{"GPU-8932f937", "GPU-1a2b3c4d"}, nil, false},
		{"mixed never truncated into a wrong group", "0,GPU-8932f937", []int{0}, []string{"GPU-8932f937"}, nil, false},
		{"mig id kept", "MIG-4f1e2d3c-aaaa-bbbb-cccc-0123456789ab", nil, []string{"MIG-4f1e2d3c-aaaa-bbbb-cccc-0123456789ab"}, nil, false},
		{"level zero sub-device mask kept", "0.1,1.0", nil, []string{"0.1", "1.0"}, nil, false},
		{"set but empty means none visible", "", nil, nil, nil, true},
		{"container toolkit all", "all", nil, nil, nil, true},
		{"container toolkit none", "none", nil, nil, nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sc, legacy := parseGPUScope("environ:", "CUDA_VISIBLE_DEVICES", tc.raw)
			if !reflect.DeepEqual(sc.Indices, tc.wantIdx) {
				t.Errorf("indices: got %v want %v", sc.Indices, tc.wantIdx)
			}
			if !reflect.DeepEqual(sc.UUIDs, tc.wantUUIDs) {
				t.Errorf("uuids: got %v want %v", sc.UUIDs, tc.wantUUIDs)
			}
			if !reflect.DeepEqual(legacy, tc.wantLegacy) {
				t.Errorf("legacy group: got %v want %v", legacy, tc.wantLegacy)
			}
			if sc.Raw != strings.TrimSpace(tc.raw) {
				t.Errorf("raw not preserved: %q", sc.Raw)
			}
			if sc.Source != "environ:CUDA_VISIBLE_DEVICES" {
				t.Errorf("source: %q", sc.Source)
			}
			if (sc.Note != "") != tc.wantNoteSet {
				t.Errorf("note %q, want set=%v", sc.Note, tc.wantNoteSet)
			}
			if sc.CrossChecked {
				t.Errorf("a freshly parsed scope is never cross-checked")
			}
		})
	}
}

func TestAllowlistedEnvExtractsOnlyScopeKeys(t *testing.T) {
	blob := []byte("PATH=/usr/bin\x00HF_TOKEN=hf_secret_value\x00CUDA_VISIBLE_DEVICES=0,1\x00OPENAI_API_KEY=sk-secret\x00ROCR_VISIBLE_DEVICES=2\x00=broken\x00NOEQUALS\x00")
	got := allowlistedEnv(blob)
	want := map[string]string{"CUDA_VISIBLE_DEVICES": "0,1", "ROCR_VISIBLE_DEVICES": "2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestParseParallelismVLLM(t *testing.T) {
	par, caps := parseParallelismFromArgs("vllm", "python -m vllm.entrypoints.openai.api_server --model meta-llama/Llama-3 --tensor-parallel-size 8 --port 8000")
	if par == nil || par.Type != "tp" || par.Width != 8 {
		t.Fatalf("vllm tp8: got %v", par)
	}
	if par.PipelineWidth != 0 || par.DataWidth != 0 {
		t.Fatalf("no pp/dp flag: got %+v", par)
	}
	if caps == nil || !caps.TP || caps.MaxTPWidth != 8 {
		t.Fatalf("caps: got %v", caps)
	}
}

func TestParseParallelismVLLMPipelineAndData(t *testing.T) {
	par, caps := parseParallelismFromArgs("vllm", "vllm serve foo --tensor-parallel-size 4 --pipeline-parallel-size 2 --data-parallel-size=2")
	if par == nil || par.Type != "tp" || par.Width != 4 || par.PipelineWidth != 2 || par.DataWidth != 2 {
		t.Fatalf("tp4 pp2 dp2: got %+v", par)
	}
	if caps == nil || !caps.TP || !caps.PP || !caps.DP {
		t.Fatalf("caps should show tp/pp/dp: %+v", caps)
	}
}

func TestParseParallelismVLLMPipelineOnly(t *testing.T) {
	par, _ := parseParallelismFromArgs("vllm", "vllm serve foo --pipeline-parallel-size 4")
	if par == nil || par.Type != "pp" || par.Width != 4 || par.PipelineWidth != 0 {
		t.Fatalf("pp only: got %+v", par)
	}
}

func TestParseParallelismVLLMDataOnly(t *testing.T) {
	par, _ := parseParallelismFromArgs("vllm", "vllm serve foo --data-parallel-size 2")
	if par == nil || par.Type != "dp" || par.Width != 2 {
		t.Fatalf("dp only: got %+v", par)
	}
}

func TestParseParallelismSGLang(t *testing.T) {
	par, caps := parseParallelismFromArgs("sglang", "python -m sglang.launch_server --model foo --tp 4 --port 3000")
	if par == nil || par.Width != 4 {
		t.Fatalf("sglang tp4: got %v caps %v", par, caps)
	}
}

func TestParseParallelismTGI(t *testing.T) {
	par, caps := parseParallelismFromArgs("tgi", "text-generation-launcher --num-shard 2 --port 3000")
	if par == nil || par.Width != 2 {
		t.Fatalf("tgi 2: got %v caps %v", par, caps)
	}
}

// ---- collector fakes ----

// fakeCollector builds a collector with every outside dependency replaced:
// no real ps, no real /proc, no real nvidia-smi.
func fakeCollector(readEnv bool, environ map[int]string, cmds map[string]string) *deploymentCollector {
	c := newDeploymentCollector(readEnv)
	c.readEnviron = func(pid int) ([]byte, error) {
		v, ok := environ[pid]
		if !ok {
			return nil, errors.New("permission denied")
		}
		return []byte(v), nil
	}
	// Never read the real /proc of the machine running the tests.
	c.readCmdline = func(int) ([]byte, error) { return nil, errors.New("no such process") }
	c.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		key := name + " " + strings.Join(args, " ")
		out, ok := cmds[key]
		if !ok {
			return nil, fmt.Errorf("fake: no such command %q", key)
		}
		return []byte(out), nil
	}
	return c
}

const (
	cmdComputeApps = "nvidia-smi --query-compute-apps=pid,gpu_uuid --format=csv,noheader,nounits"
	cmdGPUUUIDs    = "nvidia-smi --query-gpu=index,gpu_uuid --format=csv,noheader,nounits"
)

func nvidiaCmds(computeApps string) map[string]string {
	return map[string]string{
		cmdGPUUUIDs:    "0, GPU-aaaa0000\n1, GPU-bbbb1111\n2, GPU-cccc2222\n3, GPU-dddd3333\n",
		cmdComputeApps: computeApps,
	}
}

func TestCollectFromPS_SingleVLLM_NoOptIn_NoScope(t *testing.T) {
	// The agent's own environment must never be treated as the runtime's.
	t.Setenv("CUDA_VISIBLE_DEVICES", "0,1,2,3,4,5,6,7")
	ps := "  PID  PPID ARGS\n  123     1 vllm serve foo --tensor-parallel-size 8 --port 8000\n"
	detected := []DetectedRuntime{{Name: "vllm", Port: 8000, URL: "http://localhost:8000"}}
	c := fakeCollector(false, nil, nil)
	reps := c.collectFromPS(ps, detected)
	if len(reps) != 1 {
		t.Fatalf("want 1 rep got %d %v", len(reps), reps)
	}
	r := reps[0]
	if r.Parallelism == nil || r.Parallelism.Width != 8 {
		t.Fatalf("width 8: got %v", r.Parallelism)
	}
	if r.GPUGroup != nil || r.GPUScope != nil {
		t.Fatalf("no opt-in and no probe: scope must be unknown, got group=%v scope=%+v", r.GPUGroup, r.GPUScope)
	}
	if r.Source != "ps" || r.Port != 8000 {
		t.Fatalf("source/port: %v %v", r.Source, r.Port)
	}
}

func TestCollectFromPS_NeverFabricatesGroup(t *testing.T) {
	ps := "123 1 vllm serve foo --tensor-parallel-size 4 --port 8000\n"
	c := fakeCollector(true, map[int]string{123: "PATH=/bin\x00"}, nil)
	reps := c.collectFromPS(ps, []DetectedRuntime{{Name: "vllm", Port: 8000}})
	if len(reps) != 1 {
		t.Fatalf("want 1 got %v", reps)
	}
	if len(reps[0].GPUGroup) != 0 {
		t.Fatalf("group must not be invented: %v", reps[0].GPUGroup)
	}
	sc := reps[0].GPUScope
	if sc == nil || len(sc.Indices) != 0 || sc.CrossChecked || sc.Note == "" {
		t.Fatalf("readable env with no variable: want a note and no indices, got %+v", sc)
	}
}

func TestCollectFromPS_OptInReadsRuntimeEnvNotAgentEnv(t *testing.T) {
	t.Setenv("CUDA_VISIBLE_DEVICES", "7")
	ps := "500 1 vllm serve foo --tensor-parallel-size 2 --port 8000\n"
	environ := map[int]string{500: "HF_TOKEN=hf_super_secret\x00CUDA_VISIBLE_DEVICES=0,1\x00"}
	c := fakeCollector(true, environ, nil)
	reps := c.collectFromPS(ps, []DetectedRuntime{{Name: "vllm", Port: 8000}})
	if len(reps) != 1 {
		t.Fatalf("got %v", reps)
	}
	r := reps[0]
	if !reflect.DeepEqual(r.GPUGroup, []int{0, 1}) {
		t.Fatalf("group: %v", r.GPUGroup)
	}
	if r.GPUScope == nil || r.GPUScope.Source != "environ:CUDA_VISIBLE_DEVICES" || r.GPUScope.Raw != "0,1" {
		t.Fatalf("scope: %+v", r.GPUScope)
	}
	if r.GPUScope.CrossChecked {
		t.Fatalf("no nvidia probe available: must not claim cross-check")
	}
}

func TestCollectFromPS_EnvironUnreadableIsNotEmpty(t *testing.T) {
	ps := "500 1 vllm serve foo --port 8000\n"
	c := fakeCollector(true, map[int]string{}, nil) // read fails
	reps := c.collectFromPS(ps, []DetectedRuntime{{Name: "vllm", Port: 8000}})
	sc := reps[0].GPUScope
	if sc == nil || sc.Note == "" || len(sc.Indices) != 0 || sc.Raw != "" {
		t.Fatalf("unreadable env must be reported as unknown with a note: %+v", sc)
	}
	if !strings.Contains(sc.Note, "not readable") {
		t.Fatalf("note should say not readable: %q", sc.Note)
	}
}

func TestCollectFromPS_SecretsNeverLeakIntoReports(t *testing.T) {
	ps := "500 1 vllm serve foo --api-key cli_secret_key --tensor-parallel-size 2 --port 8000\n"
	environ := map[int]string{500: "HF_TOKEN=hf_super_secret\x00AWS_SECRET_ACCESS_KEY=aws_super_secret\x00CUDA_VISIBLE_DEVICES=0,1\x00"}
	c := fakeCollector(true, environ, nvidiaCmds("500, GPU-aaaa0000\n500, GPU-bbbb1111\n"))
	reps := c.collectFromPS(ps, []DetectedRuntime{{Name: "vllm", Port: 8000}})
	b, err := json.Marshal(Telemetry{Deployments: reps})
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"hf_super_secret", "aws_super_secret", "cli_secret_key", "HF_TOKEN", "AWS_SECRET"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("telemetry leaked %q: %s", secret, b)
		}
	}
}

func TestCollectFromPS_CrossCheckAgrees(t *testing.T) {
	ps := "500 1 vllm serve foo --tensor-parallel-size 2 --port 8000\n"
	c := fakeCollector(true, map[int]string{500: "CUDA_VISIBLE_DEVICES=0,1\x00"}, nvidiaCmds("500, GPU-aaaa0000\n500, GPU-bbbb1111\n"))
	reps := c.collectFromPS(ps, []DetectedRuntime{{Name: "vllm", Port: 8000}})
	sc := reps[0].GPUScope
	if sc == nil || !sc.CrossChecked {
		t.Fatalf("env and compute-apps agree: want cross_checked, got %+v", sc)
	}
	if !reflect.DeepEqual(sc.Indices, []int{0, 1}) || !reflect.DeepEqual(reps[0].GPUGroup, []int{0, 1}) {
		t.Fatalf("indices %v group %v", sc.Indices, reps[0].GPUGroup)
	}
}

func TestCollectFromPS_CrossCheckCountsWorkerChildren(t *testing.T) {
	// The API server process holds no GPU context; its worker children do.
	ps := "500 1 vllm serve foo --tensor-parallel-size 2 --port 8000\n501 500 python worker\n502 500 python worker\n"
	c := fakeCollector(true, map[int]string{500: "CUDA_VISIBLE_DEVICES=0,1\x00"}, nvidiaCmds("501, GPU-aaaa0000\n502, GPU-bbbb1111\n"))
	reps := c.collectFromPS(ps, []DetectedRuntime{{Name: "vllm", Port: 8000}})
	if sc := reps[0].GPUScope; sc == nil || !sc.CrossChecked {
		t.Fatalf("children hold the GPUs: want cross_checked, got %+v", sc)
	}
}

func TestCollectFromPS_CrossCheckMismatchIsReportedNotTrusted(t *testing.T) {
	// Env says GPUs 0,1 but the process actually sits on 2,3 (for example
	// CUDA_DEVICE_ORDER reorders indices). Report both, trust neither.
	ps := "500 1 vllm serve foo --tensor-parallel-size 2 --port 8000\n"
	c := fakeCollector(true, map[int]string{500: "CUDA_VISIBLE_DEVICES=0,1\x00"}, nvidiaCmds("500, GPU-cccc2222\n500, GPU-dddd3333\n"))
	reps := c.collectFromPS(ps, []DetectedRuntime{{Name: "vllm", Port: 8000}})
	sc := reps[0].GPUScope
	if sc == nil || sc.CrossChecked {
		t.Fatalf("mismatch must not be cross-checked: %+v", sc)
	}
	if !strings.Contains(sc.Note, "2, 3") {
		t.Fatalf("note should name the observed GPUs: %q", sc.Note)
	}
	if !reflect.DeepEqual(sc.Indices, []int{0, 1}) {
		t.Fatalf("declared-by-env indices still reported as-is: %v", sc.Indices)
	}
	if reps[0].GPUGroup != nil {
		t.Fatalf("a contradicted list must not reach legacy gpu_group: %v", reps[0].GPUGroup)
	}
}

func TestCollectFromPS_CrossCheckNoProcessWithholdsLegacyGroup(t *testing.T) {
	ps := "500 1 vllm serve foo --port 8000\n"
	c := fakeCollector(true, map[int]string{500: "CUDA_VISIBLE_DEVICES=0,1\x00"}, nvidiaCmds("999, GPU-aaaa0000\n"))
	reps := c.collectFromPS(ps, []DetectedRuntime{{Name: "vllm", Port: 8000}})
	sc := reps[0].GPUScope
	if sc == nil || sc.CrossChecked || !reflect.DeepEqual(sc.Indices, []int{0, 1}) {
		t.Fatalf("scope should stay informational: %+v", sc)
	}
	if reps[0].GPUGroup != nil {
		t.Fatalf("unverified list must not reach legacy gpu_group: %v", reps[0].GPUGroup)
	}
}

func TestParseGPUScopeCapsRawAndUUIDs(t *testing.T) {
	var toks []string
	for i := 0; i < 100; i++ {
		toks = append(toks, fmt.Sprintf("GPU-%040d", i))
	}
	sc, _ := parseGPUScope("environ:", "CUDA_VISIBLE_DEVICES", strings.Join(toks, ","))
	if len(sc.UUIDs) != 64 {
		t.Fatalf("uuids not capped: %d", len(sc.UUIDs))
	}
	if len(sc.Raw) != 256 {
		t.Fatalf("raw not capped: %d", len(sc.Raw))
	}
}

func TestCollectFromPS_CrossCheckResolvesUUIDScope(t *testing.T) {
	ps := "500 1 vllm serve foo --tensor-parallel-size 2 --port 8000\n"
	c := fakeCollector(true, map[int]string{500: "CUDA_VISIBLE_DEVICES=GPU-cccc2222,GPU-dddd3333\x00"}, nvidiaCmds("500, GPU-cccc2222\n500, GPU-dddd3333\n"))
	reps := c.collectFromPS(ps, []DetectedRuntime{{Name: "vllm", Port: 8000}})
	sc := reps[0].GPUScope
	if sc == nil || !sc.CrossChecked {
		t.Fatalf("uuid scope should verify: %+v", sc)
	}
	if !reflect.DeepEqual(sc.UUIDs, []string{"GPU-cccc2222", "GPU-dddd3333"}) {
		t.Fatalf("uuids must stay: %v", sc.UUIDs)
	}
	if !reflect.DeepEqual(sc.Indices, []int{2, 3}) || !reflect.DeepEqual(reps[0].GPUGroup, []int{2, 3}) {
		t.Fatalf("resolved indices %v group %v", sc.Indices, reps[0].GPUGroup)
	}
}

func TestCollectFromPS_CrossCheckAbbreviatedUUID(t *testing.T) {
	ps := "500 1 vllm serve foo --port 8000\n"
	c := fakeCollector(true, map[int]string{500: "CUDA_VISIBLE_DEVICES=GPU-aaaa\x00"}, nvidiaCmds("500, GPU-aaaa0000\n"))
	reps := c.collectFromPS(ps, []DetectedRuntime{{Name: "vllm", Port: 8000}})
	if sc := reps[0].GPUScope; sc == nil || !sc.CrossChecked || !reflect.DeepEqual(sc.Indices, []int{0}) {
		t.Fatalf("abbreviated uuid should resolve: %+v", sc)
	}
}

func TestCollectFromPS_NoNvidiaSmiNotCrossChecked(t *testing.T) {
	ps := "500 1 vllm serve foo --port 8000\n"
	c := fakeCollector(true, map[int]string{500: "CUDA_VISIBLE_DEVICES=0,1\x00"}, map[string]string{}) // every command fails
	reps := c.collectFromPS(ps, []DetectedRuntime{{Name: "vllm", Port: 8000}})
	sc := reps[0].GPUScope
	if sc == nil || sc.CrossChecked || !reflect.DeepEqual(sc.Indices, []int{0, 1}) {
		t.Fatalf("env-only value stays informational: %+v", sc)
	}
}

func TestCollectFromPS_AMDEnvIsNeverCrossChecked(t *testing.T) {
	ps := "500 1 vllm serve foo --port 8000\n"
	c := fakeCollector(true, map[int]string{500: "ROCR_VISIBLE_DEVICES=0,1\x00"}, nvidiaCmds("500, GPU-aaaa0000\n500, GPU-bbbb1111\n"))
	reps := c.collectFromPS(ps, []DetectedRuntime{{Name: "vllm", Port: 8000}})
	sc := reps[0].GPUScope
	if sc == nil || sc.CrossChecked || sc.Source != "environ:ROCR_VISIBLE_DEVICES" {
		t.Fatalf("non-NVIDIA scope must not be marked verified: %+v", sc)
	}
}

func TestCollectFromPS_ComputeAppsOnlyIsEvidenceNotTrusted(t *testing.T) {
	// Opt-in off: the process list alone still tells us where the runtime is
	// running, but it is reported as unconfirmed evidence, not a verified scope.
	ps := "500 1 vllm serve foo --port 8000\n"
	c := fakeCollector(false, nil, nvidiaCmds("500, GPU-aaaa0000\n500, GPU-bbbb1111\n"))
	reps := c.collectFromPS(ps, []DetectedRuntime{{Name: "vllm", Port: 8000}})
	sc := reps[0].GPUScope
	if sc == nil || sc.CrossChecked || sc.Source != "nvidia-compute-apps" || !reflect.DeepEqual(sc.Indices, []int{0, 1}) {
		t.Fatalf("scope: %+v", sc)
	}
	if reps[0].GPUGroup != nil {
		t.Fatalf("legacy gpu_group must stay empty for unconfirmed evidence: %v", reps[0].GPUGroup)
	}
}

func TestCollectFromPS_TwoRuntimesDifferentTP(t *testing.T) {
	ps := "1 0 vllm --tensor-parallel-size 8 --port 8000\n2 0 vllm --tensor-parallel-size 4 --port 8001\n"
	detected := []DetectedRuntime{{Name: "vllm", Port: 8000}, {Name: "vllm", Port: 8001}}
	reps := fakeCollector(false, nil, nil).collectFromPS(ps, detected)
	if len(reps) != 2 {
		t.Fatalf("want 2 reps got %d %v", len(reps), reps)
	}
	m := make(map[int]int)
	for _, r := range reps {
		if r.Parallelism != nil {
			m[r.Port] = r.Parallelism.Width
		}
	}
	if m[8000] != 8 || m[8001] != 4 {
		t.Fatalf("per-port widths: %v", m)
	}
}

func TestCollectFromPS_NoRuntimeReturnsEmpty(t *testing.T) {
	ps := "1 0 nginx master\n2 0 bash\n"
	reps := fakeCollector(false, nil, nil).collectFromPS(ps, []DetectedRuntime{{Name: "vllm", Port: 8000}})
	if len(reps) != 0 {
		t.Fatalf("want 0 got %v", reps)
	}
}

func TestCollectFromPS_EmptyPSReturnsNil(t *testing.T) {
	reps := fakeCollector(false, nil, nil).collectFromPS("", []DetectedRuntime{{Name: "vllm", Port: 8000}})
	if len(reps) != 0 {
		t.Fatalf("want 0 got %v", reps)
	}
}

func TestParseDockerInspect(t *testing.T) {
	inspect := `{
		"State": {"Pid": 4242},
		"Config": {"Env": ["CUDA_VISIBLE_DEVICES=0,1,2,3", "HF_TOKEN=hf_super_secret", "FOO=bar"], "Cmd": ["--tensor-parallel-size", "4", "--port", "8000"], "Image": "vllm/vllm-openai:latest"},
		"Args": ["--model", "foo"],
		"NetworkSettings": {"Ports": {"8000/tcp": [{"HostPort": "8000"}]}}
	}`
	d := parseDockerInspect(inspect)
	if d == nil {
		t.Fatalf("nil")
	}
	rep := d.report
	if rep.Runtime != "vllm" || rep.Source != "docker" || d.pid != 4242 {
		t.Fatalf("runtime/source/pid: %v %v %v", rep.Runtime, rep.Source, d.pid)
	}
	if rep.Parallelism == nil || rep.Parallelism.Width != 4 {
		t.Fatalf("width 4: %v", rep.Parallelism)
	}
	if !reflect.DeepEqual(rep.GPUGroup, []int{0, 1, 2, 3}) {
		t.Fatalf("group: %v", rep.GPUGroup)
	}
	if rep.GPUScope == nil || rep.GPUScope.Source != "docker-env:CUDA_VISIBLE_DEVICES" {
		t.Fatalf("scope: %+v", rep.GPUScope)
	}
	b, _ := json.Marshal(rep)
	if strings.Contains(string(b), "hf_super_secret") || strings.Contains(string(b), "FOO") {
		t.Fatalf("docker env leaked: %s", b)
	}
}

func TestParseDockerInspect_NoEnvNoFabricatedGroup(t *testing.T) {
	inspect := `{"Config": {"Env": ["FOO=bar"], "Cmd": ["--port", "8000"], "Image": "vllm/vllm-openai:latest"}}`
	d := parseDockerInspect(inspect)
	if d == nil {
		t.Fatal("nil")
	}
	if d.report.GPUGroup != nil || (d.report.GPUScope != nil && len(d.report.GPUScope.Indices) > 0) {
		t.Fatalf("must not invent a group: %+v", d.report)
	}
}

func TestProbeNVIDIA(t *testing.T) {
	run := fakeCollector(false, nil, nvidiaCmds("12, GPU-aaaa0000\n13, GPU-bbbb1111\n")).run
	p, err := probeNVIDIA(run)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.pidUUIDs[12], []string{"GPU-aaaa0000"}) || p.indexUUID[1] != "GPU-bbbb1111" {
		t.Fatalf("probe: %+v", p)
	}
	if _, err := probeNVIDIA(func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("not found") }); err == nil {
		t.Fatalf("missing nvidia-smi must be an error, not an empty probe")
	}
}

func TestTelemetryDeploymentMarshal(t *testing.T) {
	tel := Telemetry{
		Agent: Agent{Version: "v0.1", ProtocolVersion: 1},
		Deployments: []DeploymentReport{
			{Runtime: "vllm", Port: 8000, GPUGroup: []int{0, 1}, GPUScope: &GPUScope{Indices: []int{0, 1}, Raw: "0,1", Source: "environ:CUDA_VISIBLE_DEVICES", CrossChecked: true},
				Parallelism: &ParallelismInfo{Type: "tp", Width: 2, PipelineWidth: 2}, Caps: &RuntimeCaps{TP: true, MaxTPWidth: 2}, Source: "ps"},
		},
	}
	b, err := json.Marshal(tel)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["deployments"]; !ok {
		t.Fatalf("deployments missing: %v", string(b))
	}
	var decoded Telemetry
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded.Deployments) != 1 || decoded.Deployments[0].Parallelism.Width != 2 || !decoded.Deployments[0].GPUScope.CrossChecked {
		t.Fatalf("decoded: %v", decoded.Deployments)
	}
}

func TestTelemetryDeploymentOmittedWhenNil(t *testing.T) {
	tel := Telemetry{Agent: Agent{Version: "v0.1", ProtocolVersion: 1}}
	b, _ := json.Marshal(tel)
	var m map[string]interface{}
	json.Unmarshal(b, &m)
	if _, ok := m["deployments"]; ok {
		t.Fatalf("should be omitted when nil: %v", string(b))
	}
}

func TestDeploymentReportOmitsScopeWhenUnknown(t *testing.T) {
	b, _ := json.Marshal(DeploymentReport{Runtime: "vllm", Port: 8000, Source: "ps"})
	if strings.Contains(string(b), "gpu_scope") || strings.Contains(string(b), "gpu_group") {
		t.Fatalf("unknown scope must be omitted, never a zero value: %s", b)
	}
}
