package marboragent

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestParseGPUGroup(t *testing.T) {
	tests := []struct {
		in   string
		want []int
	}{
		{"0,1,2,3", []int{0, 1, 2, 3}},
		{"0", []int{0}},
		{" 0, 1 , 2 ", []int{0, 1, 2}},
		{"", nil},
		{" , , ", nil},
		{"0,,1", []int{0, 1}},
	}
	for _, tc := range tests {
		if got := ParseGPUGroupForTest(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parse %q: got %v want %v", tc.in, got, tc.want)
		}
	}
}

func TestParseParallelismVLLM(t *testing.T) {
	par, caps := ParseParallelismForTest("vllm", "python -m vllm.entrypoints.openai.api_server --model meta-llama/Llama-3 --tensor-parallel-size 8 --port 8000")
	if par == nil || par.Type != "tp" || par.Width != 8 {
		t.Fatalf("vllm tp8: got %v", par)
	}
	if caps == nil || !caps.TP || caps.MaxTPWidth != 8 {
		t.Fatalf("caps: got %v", caps)
	}
}

func TestParseParallelismSGLang(t *testing.T) {
	par, caps := ParseParallelismForTest("sglang", "python -m sglang.launch_server --model foo --tp 4 --port 3000")
	if par == nil || par.Width != 4 {
		t.Fatalf("sglang tp4: got %v caps %v", par, caps)
	}
}

func TestParseParallelismTGI(t *testing.T) {
	par, caps := ParseParallelismForTest("tgi", "text-generation-launcher --num-shard 2 --port 3000")
	if par == nil || par.Width != 2 {
		t.Fatalf("tgi 2: got %v caps %v", par, caps)
	}
}

func TestCollectDeploymentFromPS_SingleVLLM(t *testing.T) {
	ps := "USER PID %CPU COMMAND\nroot 123 vllm --tensor-parallel-size 8 --port 8000 --model foo\n"
	detected := []DetectedRuntime{{Name: "vllm", Port: 8000, URL: "http://localhost:8000"}}
	reps := CollectDeploymentForTest(ps, detected, 8, map[string]string{"CUDA_VISIBLE_DEVICES": "0,1,2,3,4,5,6,7"})
	if len(reps) != 1 {
		t.Fatalf("want 1 rep got %d %v", len(reps), reps)
	}
	if reps[0].Parallelism == nil || reps[0].Parallelism.Width != 8 {
		t.Fatalf("width 8: got %v", reps[0].Parallelism)
	}
	if !reflect.DeepEqual(reps[0].GPUGroup, []int{0, 1, 2, 3, 4, 5, 6, 7}) {
		t.Fatalf("gpu group: %v", reps[0].GPUGroup)
	}
	if reps[0].Source != "ps" {
		t.Fatalf("source ps: %v", reps[0].Source)
	}
	if reps[0].Port != 8000 {
		t.Fatalf("port 8000: %v", reps[0].Port)
	}
}

func TestCollectDeploymentFromPS_FallbackGPUCount(t *testing.T) {
	ps := "root 123 vllm --tensor-parallel-size 4 --port 8000\n"
	detected := []DetectedRuntime{{Name: "vllm", Port: 8000}}
	reps := CollectDeploymentForTest(ps, detected, 4, map[string]string{})
	if len(reps) != 1 {
		t.Fatalf("want 1 got %v", reps)
	}
	if len(reps[0].GPUGroup) != 4 {
		t.Fatalf("fallback group len 4: %v", reps[0].GPUGroup)
	}
}

func TestCollectDeploymentFromPS_TwoRuntimesDifferentTP(t *testing.T) {
	ps := "root 1 vllm --tensor-parallel-size 8 --port 8000\nroot 2 vllm --tensor-parallel-size 4 --port 8001\n"
	detected := []DetectedRuntime{{Name: "vllm", Port: 8000}, {Name: "vllm", Port: 8001}}
	reps := CollectDeploymentForTest(ps, detected, 8, map[string]string{})
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

func TestCollectDeploymentFromPS_NoRuntimeReturnsEmpty(t *testing.T) {
	ps := "root 1 nginx master\nroot 2 bash\n"
	detected := []DetectedRuntime{{Name: "vllm", Port: 8000}}
	reps := CollectDeploymentForTest(ps, detected, 4, map[string]string{})
	if len(reps) != 0 {
		t.Fatalf("want 0 got %v", reps)
	}
}

func TestCollectDeploymentFromPS_UnknownReturnsNil(t *testing.T) {
	// ps isolated - no output, fallback nil
	ps := ""
	detected := []DetectedRuntime{{Name: "vllm", Port: 8000}}
	reps := CollectDeploymentForTest(ps, detected, 0, map[string]string{})
	if len(reps) != 0 {
		t.Fatalf("want 0 got %v", reps)
	}
}

func TestParseDockerInspect(t *testing.T) {
	inspect := `{
		"Config": {"Env": ["CUDA_VISIBLE_DEVICES=0,1,2,3", "FOO=bar"], "Cmd": ["--tensor-parallel-size", "4", "--port", "8000"], "Image": "vllm/vllm-openai:latest"},
		"Args": ["--model", "foo"],
		"NetworkSettings": {"Ports": {"8000/tcp": [{"HostPort": "8000"}]}}
	}`
	rep := ParseDockerInspectForTest(inspect, 4)
	if rep == nil {
		t.Fatalf("nil rep")
	}
	if rep.Runtime != "vllm" {
		t.Fatalf("runtime vllm: %v", rep.Runtime)
	}
	if rep.Parallelism == nil || rep.Parallelism.Width != 4 {
		t.Fatalf("width 4: %v", rep.Parallelism)
	}
	if !reflect.DeepEqual(rep.GPUGroup, []int{0, 1, 2, 3}) {
		t.Fatalf("group: %v", rep.GPUGroup)
	}
	if rep.Source != "docker" {
		t.Fatalf("docker source: %v", rep.Source)
	}
}

func TestTelemetryDeploymentMarshal(t *testing.T) {
	tel := Telemetry{
		Agent: Agent{Version: "v0.1", ProtocolVersion: 1},
		Deployments: []DeploymentReport{
			{Runtime: "vllm", Port: 8000, GPUGroup: []int{0, 1}, Parallelism: &ParallelismInfo{Type: "tp", Width: 2}, Caps: &RuntimeCaps{TP: true, MaxTPWidth: 2}, Source: "ps"},
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
	// Old marbor ignores deployments - ensure roundtrip
	var decoded Telemetry
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded.Deployments) != 1 || decoded.Deployments[0].Parallelism.Width != 2 {
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
