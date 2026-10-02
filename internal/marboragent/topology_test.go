package marboragent

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func ptrInt(v int) *int { return &v }

// topoCollector is a fake collector whose /proc/<pid>/cmdline answers come
// from cmdlines (argv per pid, joined with NUL like the real file).
func topoCollector(readEnv bool, environ map[int]string, cmdlines map[int][]string) *deploymentCollector {
	c := fakeCollector(readEnv, environ, nil)
	c.readCmdline = func(pid int) ([]byte, error) {
		argv, ok := cmdlines[pid]
		if !ok {
			return nil, errors.New("no such process")
		}
		return []byte(strings.Join(argv, "\x00") + "\x00"), nil
	}
	return c
}

func topoFor(rt, argv string) *Topology {
	return buildTopology(topologyInput{runtime: rt, tokens: strings.Fields(argv), argvSource: "cmdline"})
}

// ---- vLLM ----

func TestCollectFromPS_VLLMMultiNodeHead(t *testing.T) {
	ps := "100 1 vllm serve m --tensor-parallel-size 8 --pipeline-parallel-size 2 --nnodes 2 --node-rank 0 --master-addr 10.0.0.5 --master-port 29501 --port 8000\n"
	c := topoCollector(false, nil, nil)
	reps := c.collectFromPS(ps, []DetectedRuntime{{Name: "vllm", Port: 8000}})
	if len(reps) != 1 {
		t.Fatalf("want 1 report, got %v", reps)
	}
	r := reps[0]
	tp := r.Topology
	if tp == nil {
		t.Fatal("topology missing")
	}
	if tp.Launcher != "vllm-mp" || tp.RoleHint != "" {
		t.Fatalf("launcher/role: %+v", tp)
	}
	if tp.NNodes == nil || *tp.NNodes != 2 || tp.NodeRank == nil || *tp.NodeRank != 0 || tp.MasterPort == nil || *tp.MasterPort != 29501 || tp.MasterAddr != "10.0.0.5" {
		t.Fatalf("fields: %+v", tp)
	}
	wantEv := []string{"cmdline:--nnodes", "cmdline:--node-rank", "cmdline:--master-addr", "cmdline:--master-port"}
	if !reflect.DeepEqual(tp.Evidence, wantEv) {
		t.Fatalf("evidence: %v", tp.Evidence)
	}
	if r.Parallelism == nil || r.Parallelism.Type != "tp" || r.Parallelism.Width != 8 || r.Parallelism.PipelineWidth != 2 {
		t.Fatalf("parallelism must stay intact: %+v", r.Parallelism)
	}
}

func TestCollectFromPS_VLLMHeadlessWorkerAndFlagForms(t *testing.T) {
	ps := "200 1 vllm serve m --headless --node-rank=1 --nnodes=2 --master-addr=Head.Example.COM --master-port=29501\n"
	c := topoCollector(false, nil, nil)
	reps := c.collectFromPS(ps, nil)
	if len(reps) != 1 {
		t.Fatalf("got %v", reps)
	}
	tp := reps[0].Topology
	if tp == nil || tp.RoleHint != "worker" || tp.NodeRank == nil || *tp.NodeRank != 1 || tp.MasterAddr != "Head.Example.COM" {
		t.Fatalf("worker topology: %+v", tp)
	}
	if reps[0].Port != 0 || reps[0].Runtime != "vllm" {
		t.Fatalf("worker must report port 0 and runtime vllm: %+v", reps[0])
	}
}

func TestCollectFromPS_HeadlessWorkerKeepsPortZeroWithOtherRuntimeDetected(t *testing.T) {
	ps := "200 1 vllm serve m --headless --node-rank 1 --nnodes 2 --master-addr 10.0.0.5\n"
	c := topoCollector(false, nil, nil)
	reps := c.collectFromPS(ps, []DetectedRuntime{{Name: "ollama", Port: 11434, URL: "http://localhost:11434"}})
	if len(reps) != 1 {
		t.Fatalf("got %v", reps)
	}
	if reps[0].Port != 0 || reps[0].Runtime != "vllm" {
		t.Fatalf("headless worker must not be attributed to the lone detected runtime: %+v", reps[0])
	}
	if reps[0].Topology == nil || reps[0].Topology.RoleHint != "worker" {
		t.Fatalf("topology lost: %+v", reps[0].Topology)
	}
}

func TestCollectFromPS_NoTopologyWhenNotDistributed(t *testing.T) {
	for name, ps := range map[string]string{
		"plain":       "100 1 vllm serve m --tensor-parallel-size 4 --port 8000\n",
		"one node":    "100 1 vllm serve m --nnodes 1 --port 8000\n",
		"nnodes 1 eq": "100 1 vllm serve m --nnodes=1 --port 8000\n",
	} {
		reps := topoCollector(false, nil, nil).collectFromPS(ps, []DetectedRuntime{{Name: "vllm", Port: 8000}})
		if len(reps) != 1 || reps[0].Topology != nil {
			t.Fatalf("%s: topology must be absent, got %+v", name, reps)
		}
		b, _ := json.Marshal(reps[0])
		if strings.Contains(string(b), "topology") {
			t.Fatalf("%s: topology key must be omitted: %s", name, b)
		}
	}
}

func TestTopologyAbsentRankIsOmittedNotZero(t *testing.T) {
	tp := topoFor("vllm", "vllm serve m --nnodes 2 --master-addr 10.0.0.5")
	if tp == nil || tp.NodeRank != nil || tp.MasterPort != nil {
		t.Fatalf("unknown fields must stay nil: %+v", tp)
	}
	b, _ := json.Marshal(tp)
	if strings.Contains(string(b), "node_rank") || strings.Contains(string(b), "master_port") {
		t.Fatalf("absent keys must be omitted, never 0: %s", b)
	}
}

func TestCollectFromPS_UsesProcCmdlineWhenItAgrees(t *testing.T) {
	// A model path with a space is only intact in /proc cmdline, not in ps.
	argv := []string{"vllm", "serve", "/m/my model", "--nnodes", "2", "--master-addr", "10.0.0.5", "--port", "8000"}
	ps := "100 1 " + strings.Join(argv, " ") + "\n"
	c := topoCollector(false, nil, map[int][]string{100: argv})
	reps := c.collectFromPS(ps, []DetectedRuntime{{Name: "vllm", Port: 8000}})
	if len(reps) != 1 || reps[0].Topology == nil || reps[0].Topology.MasterAddr != "10.0.0.5" {
		t.Fatalf("got %+v", reps)
	}
}

func TestCollectFromPS_IgnoresProcCmdlineThatDisagreesWithPS(t *testing.T) {
	// The pid was reused between ps and the read: the other process's argv
	// must not leak into this report.
	ps := "100 1 vllm serve m --port 8000\n"
	other := []string{"vllm", "serve", "m", "--nnodes", "4", "--master-addr", "10.9.9.9", "--port", "9000"}
	c := topoCollector(false, nil, map[int][]string{100: other})
	reps := c.collectFromPS(ps, []DetectedRuntime{{Name: "vllm", Port: 8000}})
	if len(reps) != 1 || reps[0].Topology != nil {
		t.Fatalf("mismatching cmdline must be ignored: %+v", reps)
	}
}

// ---- llama.cpp ----

func TestCollectFromPS_LlamaServerRPCFromArgv(t *testing.T) {
	ps := "300 1 /usr/bin/llama-server -m x.gguf --rpc 10.0.0.6:50052,10.0.0.7:50052 --port 8080\n"
	c := topoCollector(false, nil, nil)
	reps := c.collectFromPS(ps, []DetectedRuntime{{Name: "llamacpp", Port: 8080}})
	if len(reps) != 1 {
		t.Fatalf("llama-server must be recognised by name, got %v", reps)
	}
	r := reps[0]
	if r.Runtime != "llamacpp" || r.Port != 8080 {
		t.Fatalf("runtime/port: %+v", r)
	}
	tp := r.Topology
	if tp == nil || tp.Launcher != "llamacpp-rpc" || !reflect.DeepEqual(tp.RPCServers, []string{"10.0.0.6:50052", "10.0.0.7:50052"}) {
		t.Fatalf("topology: %+v", tp)
	}
	if !reflect.DeepEqual(tp.Evidence, []string{"cmdline:--rpc"}) {
		t.Fatalf("evidence: %v", tp.Evidence)
	}
}

func TestCollectFromPS_LlamaServerWithoutRPCHasNoTopology(t *testing.T) {
	ps := "300 1 llama-server -m x.gguf --port 8080\n"
	reps := topoCollector(false, nil, nil).collectFromPS(ps, []DetectedRuntime{{Name: "llamacpp", Port: 8080}})
	if len(reps) != 1 || reps[0].Topology != nil {
		t.Fatalf("got %+v", reps)
	}
}

func TestCollectFromPS_LlamaServerEnvRPCOnlyWithOptIn(t *testing.T) {
	ps := "300 1 llama-server -m x.gguf --port 8080\n"
	environ := map[int]string{300: "HF_TOKEN=hf_secret\x00LLAMA_ARG_RPC=10.0.0.8:50052\x00"}
	det := []DetectedRuntime{{Name: "llamacpp", Port: 8080}}

	reads := 0
	off := topoCollector(false, environ, nil)
	inner := off.readEnviron
	off.readEnviron = func(pid int) ([]byte, error) { reads++; return inner(pid) }
	reps := off.collectFromPS(ps, det)
	if reads != 0 {
		t.Fatalf("environment read without opt-in: %d reads", reads)
	}
	if reps[0].Topology != nil {
		t.Fatalf("no opt-in: no env evidence at all, got %+v", reps[0].Topology)
	}

	reps = topoCollector(true, environ, nil).collectFromPS(ps, det)
	tp := reps[0].Topology
	if tp == nil || !reflect.DeepEqual(tp.RPCServers, []string{"10.0.0.8:50052"}) || !reflect.DeepEqual(tp.Evidence, []string{"env:LLAMA_ARG_RPC"}) {
		t.Fatalf("opt-in env: %+v", tp)
	}
}

func TestCollectFromPS_LlamaServerEnvUnreadableIsNotEmpty(t *testing.T) {
	ps := "300 1 llama-server -m x.gguf --port 8080\n"
	reps := topoCollector(true, map[int]string{}, nil).collectFromPS(ps, []DetectedRuntime{{Name: "llamacpp", Port: 8080}})
	tp := reps[0].Topology
	if tp == nil || len(tp.RPCServers) != 0 {
		t.Fatalf("topology: %+v", tp)
	}
	if !reflect.DeepEqual(tp.Evidence, []string{"env-unreadable:LLAMA_ARG_RPC"}) {
		t.Fatalf("evidence: %v", tp.Evidence)
	}
}

func TestCollectFromPS_LlamaServerArgvRPCBeatsEnv(t *testing.T) {
	ps := "300 1 llama-server -m x.gguf --rpc 10.0.0.6:50052 --port 8080\n"
	reads := 0
	c := topoCollector(true, map[int]string{300: "LLAMA_ARG_RPC=10.9.9.9:1\x00"}, nil)
	inner := c.readEnviron
	c.readEnviron = func(pid int) ([]byte, error) { reads++; return inner(pid) }
	reps := c.collectFromPS(ps, []DetectedRuntime{{Name: "llamacpp", Port: 8080}})
	tp := reps[0].Topology
	if tp == nil || !reflect.DeepEqual(tp.RPCServers, []string{"10.0.0.6:50052"}) {
		t.Fatalf("argv must win: %+v", tp)
	}
	for _, e := range tp.Evidence {
		if strings.Contains(e, "env") {
			t.Fatalf("env evidence with argv rpc: %v", tp.Evidence)
		}
	}
}

func TestCollectFromPS_LlamaCliNeverAltersOllamaReport(t *testing.T) {
	ps := "10 1 ollama serve\n" +
		"300 1 llama-cli -m x.gguf --rpc 10.0.0.6:50052\n"
	det := []DetectedRuntime{{Name: "ollama", Port: 11434, URL: "http://localhost:11434"}}
	with := topoCollector(false, nil, nil).collectFromPS(ps, det)
	without := topoCollector(false, nil, nil).collectFromPS("10 1 ollama serve\n", det)
	if !reflect.DeepEqual(with, without) {
		t.Fatalf("llama-cli changed the report set:\nwith    %+v\nwithout %+v", with, without)
	}
	for _, r := range with {
		if r.Topology != nil {
			t.Fatalf("ollama report must carry no topology: %+v", r)
		}
	}
}

func TestCollectFromPS_LlamaServerNotRenamedToUnrelatedLoneRuntime(t *testing.T) {
	// No --port on the llama-server line and a single unrelated runtime
	// detected: the report must not borrow that runtime's name or port.
	ps := "300 1 llama-server -m x.gguf --rpc 10.0.0.6:50052\n"
	det := []DetectedRuntime{{Name: "ollama", Port: 11434, URL: "http://localhost:11434"}}
	reps := topoCollector(false, nil, nil).collectFromPS(ps, det)
	if len(reps) != 1 || reps[0].Runtime != "llamacpp" || reps[0].Port != 0 {
		t.Fatalf("got %+v", reps)
	}
}

func TestDetectRuntimeFromArgs_LlamaServerExactBasename(t *testing.T) {
	cases := []struct{ args, want string }{
		{"/usr/bin/llama-server -m x.gguf", "llamacpp"},
		{"llama-server -m x.gguf", "llamacpp"},
		{`c:\llama\llama-server.exe -m x.gguf`, "llamacpp"},
		{"llama-server -m /models/vllm-q4.gguf", "llamacpp"},
		{"llama-server-proxy --port 1", ""},
		{"/usr/bin/llama-serverx -m x", ""},
		{"/opt/run-inference.sh --rpc a:1 llama-server", ""},
		{"llama-cli -m x.gguf", ""},
		{"python -m vllm.entrypoints.openai.api_server", "vllm"},
		{"vllm serve m", "vllm"},
	}
	for _, tc := range cases {
		if got := detectRuntimeFromArgs(strings.ToLower(tc.args)); got != tc.want {
			t.Errorf("detectRuntimeFromArgs(%q) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

// ---- hostile input ----

func TestTopologyHostileVLLM(t *testing.T) {
	long := strings.Repeat("a", 300)
	nilCases := map[string][]string{
		"negative nnodes":     {"--nnodes", "-2"},
		"huge nnodes":         {"--nnodes", "99999999999999999999"},
		"zero nnodes":         {"--nnodes", "0"},
		"rank over cap":       {"--node-rank", "99999"},
		"userinfo addr":       {"--master-addr", "user:pw@10.0.0.5"},
		"scheme addr":         {"--master-addr", "http://10.0.0.5"},
		"at sign addr":        {"--master-addr=a@b"},
		"flag last no value":  {"--master-addr"},
		"empty value":         {"--master-addr="},
		"port zero":           {"--master-port", "0"},
		"port too big":        {"--master-port", "70000"},
		"long addr":           {"--master-addr", long},
		"non utf8 addr":       {"--master-addr", "\xff\xfe"},
		"control char addr":   {"--master-addr", "10.0.0.5\n"},
		"flag in model path":  {"--model", "/m/--nnodes-2"},
		"flag in model path2": {"--model=/m/--nnodes=2"},
	}
	for name, tokens := range nilCases {
		if tp := buildTopology(topologyInput{runtime: "vllm", tokens: tokens, argvSource: "cmdline"}); tp != nil {
			t.Errorf("%s: want no topology, got %+v", name, tp)
		}
	}

	tp := topoFor("vllm", "vllm serve m --nnodes 2 --nnodes 3")
	if tp == nil || tp.NNodes == nil || *tp.NNodes != 3 {
		t.Fatalf("duplicate flags: last one wins, got %+v", tp)
	}
	tp = topoFor("vllm", "vllm serve m --headless")
	if tp == nil || tp.RoleHint != "worker" || !reflect.DeepEqual(tp.Evidence, []string{"cmdline:--headless"}) {
		t.Fatalf("headless alone: %+v", tp)
	}
	// A rejected value must not survive next to an accepted one.
	tp = topoFor("vllm", "vllm serve m --nnodes 2 --master-addr user:pw@h")
	if tp == nil || tp.MasterAddr != "" || strings.Contains(strings.Join(tp.Evidence, ","), "master-addr") {
		t.Fatalf("hostile addr leaked: %+v", tp)
	}
}

func TestTopologyHostileLlamaCpp(t *testing.T) {
	nilCases := map[string]string{
		"userinfo":      "llama-server --rpc user:pw@h:1",
		"scheme":        "llama-server --rpc http://h:1",
		"empty":         "llama-server --rpc=",
		"flag last":     "llama-server --rpc",
		"no port":       "llama-server --rpc 10.0.0.6",
		"port zero":     "llama-server --rpc 10.0.0.6:0",
		"port too big":  "llama-server --rpc 10.0.0.6:70000",
		"flag in model": "llama-server --model /m/--rpc=1.2.3.4:5",
	}
	for name, argv := range nilCases {
		if tp := topoFor("llamacpp", argv); tp != nil {
			t.Errorf("%s: want no topology, got %+v", name, tp)
		}
	}
	if tp := buildTopology(topologyInput{runtime: "llamacpp", tokens: []string{"--rpc", strings.Repeat("a", 300) + ":1"}, argvSource: "cmdline"}); tp != nil {
		t.Errorf("long entry: got %+v", tp)
	}
	if tp := buildTopology(topologyInput{runtime: "llamacpp", tokens: []string{"--rpc", "\xff\xfe:1"}, argvSource: "cmdline"}); tp != nil {
		t.Errorf("non utf8 entry: got %+v", tp)
	}

	tp := topoFor("llamacpp", "llama-server --rpc 10.0.0.6:50052,evil@h:1,[::1]:50053,10.0.0.7:50052")
	want := []string{"10.0.0.6:50052", "[::1]:50053", "10.0.0.7:50052"}
	if tp == nil || !reflect.DeepEqual(tp.RPCServers, want) {
		t.Fatalf("bad entries must be dropped, order kept: %+v", tp)
	}

	var many []string
	for i := 0; i < 1000; i++ {
		many = append(many, "10.0.0.6:50052")
	}
	tp = topoFor("llamacpp", "llama-server --rpc "+strings.Join(many, ","))
	if tp == nil || len(tp.RPCServers) != maxRPCServers {
		t.Fatalf("rpc list must be capped at %d, got %+v", maxRPCServers, tp)
	}

	// An argv --rpc that is present but unusable must not fall back to env.
	tp = buildTopology(topologyInput{
		runtime: "llamacpp", tokens: []string{"--rpc", "user:pw@h:1"}, argvSource: "cmdline",
		env: map[string]string{"LLAMA_ARG_RPC": "10.9.9.9:1"}, envSource: "env",
	})
	if tp != nil {
		t.Fatalf("argv present: env must not be consulted: %+v", tp)
	}
}

func TestTopologyOtherRuntimesHaveNone(t *testing.T) {
	for _, rt := range []string{"ollama", "tgi", "sglang", "mlx", ""} {
		if tp := topoFor(rt, "x --nnodes 2 --headless --rpc 10.0.0.6:1"); tp != nil {
			t.Errorf("%q: got %+v", rt, tp)
		}
	}
}

// ---- docker ----

func TestParseDockerInspect_TopologyFromCmdAndAllowlistedEnv(t *testing.T) {
	inspect := `{
		"State": {"Pid": 4242},
		"Config": {"Env": ["LLAMA_ARG_RPC=10.0.0.9:50052", "HF_TOKEN=hf_super_secret", "OPENAI_API_KEY=sk-secret"],
			"Cmd": ["--model", "foo", "--nnodes", "2", "--node-rank", "1", "--headless", "--master-addr", "10.0.0.5", "--api-key", "cli_secret"],
			"Image": "vllm/vllm-openai:latest"}
	}`
	d := parseDockerInspect(inspect)
	if d == nil {
		t.Fatal("nil")
	}
	tp := d.report.Topology
	if tp == nil || tp.Launcher != "vllm-mp" || tp.RoleHint != "worker" || tp.NodeRank == nil || *tp.NodeRank != 1 || tp.MasterAddr != "10.0.0.5" {
		t.Fatalf("topology: %+v", tp)
	}
	wantEv := []string{"docker-cmd:--nnodes", "docker-cmd:--node-rank", "docker-cmd:--master-addr", "docker-cmd:--headless"}
	if !reflect.DeepEqual(tp.Evidence, wantEv) {
		t.Fatalf("evidence: %v", tp.Evidence)
	}
	if len(tp.RPCServers) != 0 {
		t.Fatalf("a vLLM container must not take llama.cpp env: %+v", tp)
	}
	b, _ := json.Marshal(d.report)
	for _, s := range []string{"hf_super_secret", "sk-secret", "cli_secret", "HF_TOKEN", "OPENAI_API_KEY"} {
		if strings.Contains(string(b), s) {
			t.Fatalf("docker report leaked %q: %s", s, b)
		}
	}
}

func TestParseDockerInspect_LlamaCppRPCFromEnv(t *testing.T) {
	inspect := `{"Config": {"Env": ["LLAMA_ARG_RPC=10.0.0.9:50052,10.0.0.10:50052", "HF_TOKEN=hf_super_secret"],
		"Cmd": ["-m", "x.gguf", "--port", "8080"], "Image": "ghcr.io/ggml-org/llama.cpp:server"}}`
	d := parseDockerInspect(inspect)
	if d == nil || d.report.Runtime != "llamacpp" {
		t.Fatalf("got %+v", d)
	}
	tp := d.report.Topology
	if tp == nil || !reflect.DeepEqual(tp.RPCServers, []string{"10.0.0.9:50052", "10.0.0.10:50052"}) || !reflect.DeepEqual(tp.Evidence, []string{"docker-env:LLAMA_ARG_RPC"}) {
		t.Fatalf("topology: %+v", tp)
	}
}

func TestParseDockerInspect_PlainContainerHasNoTopology(t *testing.T) {
	inspect := `{"Config": {"Env": ["FOO=bar"], "Cmd": ["--port", "8000"], "Image": "vllm/vllm-openai:latest"}}`
	if d := parseDockerInspect(inspect); d == nil || d.report.Topology != nil {
		t.Fatalf("got %+v", d)
	}
}

// ---- dedupe ----

func TestAppendDeduped_KeepsReportWithTopology(t *testing.T) {
	plain := DeploymentReport{Runtime: "vllm", Port: 0, Source: "ps"}
	withTopo := DeploymentReport{Runtime: "vllm", Port: 0, Source: "ps", Topology: &Topology{Launcher: "vllm-mp", RoleHint: "worker"}}
	out := appendDeduped(appendDeduped(nil, plain), withTopo)
	if len(out) != 1 || out[0].Topology == nil {
		t.Fatalf("topology report must win: %+v", out)
	}
	out = appendDeduped(appendDeduped(nil, withTopo), plain)
	if len(out) != 1 || out[0].Topology == nil {
		t.Fatalf("topology report must be kept: %+v", out)
	}
}

// ---- host addresses ----

func TestCollectHostAddrs(t *testing.T) {
	ipnet := func(s string) net.Addr {
		ip, n, _ := net.ParseCIDR(s)
		n.IP = ip
		return n
	}
	list := []ifaceAddrs{
		{flags: net.FlagUp | net.FlagLoopback, addrs: []net.Addr{ipnet("127.0.0.1/8"), ipnet("::1/128")}},
		{flags: net.FlagUp, addrs: []net.Addr{ipnet("10.0.0.5/24"), ipnet("fe80::1/64"), ipnet("169.254.1.2/16"), ipnet("fd00::5/64"), &net.IPAddr{IP: net.ParseIP("192.168.1.9")}}},
		{flags: 0, addrs: []net.Addr{ipnet("10.9.9.9/24")}},
		{flags: net.FlagUp, addrs: []net.Addr{ipnet("10.0.0.5/24"), &net.UnixAddr{Name: "x", Net: "unix"}}},
	}
	got := collectHostAddrs(list)
	want := []string{"10.0.0.5", "192.168.1.9", "fd00::5"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if got := collectHostAddrs(nil); got != nil {
		t.Fatalf("none must be nil, got %v", got)
	}
	var many []net.Addr
	for i := 0; i < 100; i++ {
		many = append(many, ipnet("10.1."+strconv.Itoa(i)+".1/24"))
	}
	if got := collectHostAddrs([]ifaceAddrs{{flags: net.FlagUp, addrs: many}}); len(got) != maxHostAddrs {
		t.Fatalf("cap: got %d", len(got))
	}
}

func TestHostTelemetryAddrsOmittedWhenEmpty(t *testing.T) {
	b, _ := json.Marshal(HostTelemetry{Hostname: "h"})
	if strings.Contains(string(b), "addrs") {
		t.Fatalf("empty addrs must be omitted: %s", b)
	}
	b, _ = json.Marshal(HostTelemetry{Addrs: []string{"10.0.0.5"}})
	if !strings.Contains(string(b), `"addrs":["10.0.0.5"]`) {
		t.Fatalf("addrs missing: %s", b)
	}
}

func TestSchedulerReportsInjectedHostAddrsWithoutMutatingCollectorResult(t *testing.T) {
	shared := &HostTelemetry{Hostname: "h"}
	s := newSchedulerWithBackends("v-test", fakeGPUCollector{}, fakeHostCollector{telemetry: shared}, noRuntimeDetector)
	s.hostAddrs = func() []string { return []string{"10.0.0.5"} }
	s.Seed()
	snap := s.Snapshot()
	if snap.Host == nil || !reflect.DeepEqual(snap.Host.Addrs, []string{"10.0.0.5"}) || snap.Host.Hostname != "h" {
		t.Fatalf("host: %+v", snap.Host)
	}
	if shared.Addrs != nil {
		t.Fatalf("collector result mutated: %+v", shared)
	}
	s.hostAddrs = func() []string { return nil }
	s.Seed()
	if h := s.Snapshot().Host; h == nil || h.Addrs != nil {
		t.Fatalf("no addrs must stay nil: %+v", h)
	}
}

// ---- capabilities and compatibility ----

func TestCapabilityDoesNotImplyTopology(t *testing.T) {
	s := newSchedulerWithBackends("v-test", fakeGPUCollector{}, fakeHostCollector{telemetry: &HostTelemetry{}},
		fakeRuntimeDetector{name: "vllm", url: "http://localhost:8000", port: 8000, found: true})
	s.hostAddrs = func() []string { return nil }
	s.deploy = topoCollector(false, nil, nil)
	s.deploy.ps = func() (string, error) { return "100 1 vllm serve m --tensor-parallel-size 2 --port 8000\n", nil }
	s.Seed()
	snap := s.Snapshot()
	has := map[string]bool{}
	for _, c := range snap.Capabilities {
		has[c] = true
	}
	if !has["deployment.report"] || !has["deployment.topology"] {
		t.Fatalf("capabilities: %v", snap.Capabilities)
	}
	if len(snap.Deployments) != 1 || snap.Deployments[0].Topology != nil {
		t.Fatalf("a plain launch must carry no topology: %+v", snap.Deployments)
	}
}

func TestTelemetryDecodesWithAndWithoutTopology(t *testing.T) {
	old := `{"agent":{"protocol_version":1,"platform":"linux","architecture":"amd64"},"capabilities":["status"],
		"host":{"hostname":"h"},"health":{"runtime_reachable":true},
		"deployments":[{"runtime":"vllm","port":8000,"source":"ps"}],"last_updated":"2026-07-17T00:00:00Z"}`
	var tel Telemetry
	if err := json.Unmarshal([]byte(old), &tel); err != nil {
		t.Fatal(err)
	}
	if tel.Host == nil || len(tel.Host.Addrs) != 0 || len(tel.Deployments) != 1 || tel.Deployments[0].Topology != nil {
		t.Fatalf("pre-change payload: %+v", tel)
	}

	newer := `{"agent":{"protocol_version":1,"platform":"linux","architecture":"amd64"},"capabilities":["status","deployment.topology"],
		"host":{"hostname":"h","addrs":["10.0.0.5","fd00::5"]},"health":{"runtime_reachable":true},
		"deployments":[{"runtime":"vllm","port":0,"source":"ps","topology":{"launcher":"vllm-mp","role_hint":"worker","nnodes":2,"node_rank":1,"master_addr":"10.0.0.5","evidence":["cmdline:--headless"],"future_field":1}}],
		"last_updated":"2026-07-17T00:00:00Z"}`
	tel = Telemetry{}
	if err := json.Unmarshal([]byte(newer), &tel); err != nil {
		t.Fatal(err)
	}
	tp := tel.Deployments[0].Topology
	if tp == nil || tp.RoleHint != "worker" || tp.NNodes == nil || *tp.NNodes != 2 || tp.NodeRank == nil || *tp.NodeRank != 1 || tp.MasterPort != nil {
		t.Fatalf("topology decode: %+v", tp)
	}
	if !reflect.DeepEqual(tel.Host.Addrs, []string{"10.0.0.5", "fd00::5"}) {
		t.Fatalf("addrs decode: %+v", tel.Host)
	}
}

// ---- secrets ----

func TestTopologyEnvAllowlistIsExactlyOneKey(t *testing.T) {
	if !reflect.DeepEqual(topologyEnvKeys, []string{"LLAMA_ARG_RPC"}) {
		t.Fatalf("topology env allowlist changed: %v", topologyEnvKeys)
	}
	if len(scopeEnvKeys) != 5 {
		t.Fatalf("gpu scope allowlist changed: %v", scopeEnvKeys)
	}
	got := topologyEnvList([]string{"HF_TOKEN=a", "OPENAI_API_KEY=b", "AWS_SECRET_ACCESS_KEY=c", "LLAMA_ARG_RPC=10.0.0.6:1", "CUDA_VISIBLE_DEVICES=0", "LLAMA_ARG_RPC_X=1", "=bad", "noequals"})
	if !reflect.DeepEqual(got, map[string]string{"LLAMA_ARG_RPC": "10.0.0.6:1"}) {
		t.Fatalf("extracted: %v", got)
	}
}

func TestSecretsNeverLeakThroughStatusMetricsOrLogs(t *testing.T) {
	secrets := []string{"hf_super_secret", "aws_super_secret", "sk-openai-secret", "cli_secret_key", "tok_secret", "secret-path-segment", "HF_TOKEN", "OPENAI_API_KEY", "AWS_SECRET"}

	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(log.Writer())

	ps := "500 1 vllm serve /models/secret-path-segment --api-key cli_secret_key --hf-token tok_secret --tensor-parallel-size 2 --nnodes 2 --node-rank 0 --master-addr 10.0.0.5 --port 8000\n" +
		"501 1 llama-server -m /models/secret-path-segment.gguf --api-key cli_secret_key --port 8080\n"
	environ := map[int]string{
		500: "HF_TOKEN=hf_super_secret\x00AWS_SECRET_ACCESS_KEY=aws_super_secret\x00CUDA_VISIBLE_DEVICES=0,1\x00",
		501: "OPENAI_API_KEY=sk-openai-secret\x00LLAMA_ARG_RPC=10.0.0.6:50052\x00HF_TOKEN=hf_super_secret\x00",
	}
	s := newSchedulerWithBackends("v-test", fakeGPUCollector{}, fakeHostCollector{telemetry: &HostTelemetry{}},
		fakeRuntimeDetector{name: "vllm", url: "http://localhost:8000", port: 8000, found: true})
	s.hostAddrs = func() []string { return []string{"10.0.0.5"} }
	s.deploy = topoCollector(true, environ, nil)
	s.deploy.ps = func() (string, error) { return ps, nil }
	s.Seed()
	snap := s.Snapshot()

	var llama *DeploymentReport
	for i := range snap.Deployments {
		if snap.Deployments[i].Runtime == "llamacpp" {
			llama = &snap.Deployments[i]
		}
	}
	if llama == nil || llama.Topology == nil || !reflect.DeepEqual(llama.Topology.RPCServers, []string{"10.0.0.6:50052"}) {
		t.Fatalf("expected llama.cpp topology in %+v", snap.Deployments)
	}

	full, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	surfaces := map[string]string{
		"status":  string(full),
		"metrics": RenderPrometheus(snap),
		"logs":    logs.String(),
	}
	for name, text := range surfaces {
		for _, secret := range secrets {
			if strings.Contains(text, secret) {
				t.Fatalf("%s leaked %q", name, secret)
			}
		}
	}
	if strings.Contains(surfaces["metrics"], "10.0.0.6") || strings.Contains(surfaces["metrics"], "10.0.0.5") {
		t.Fatalf("topology and addresses must not be exported as metrics: %s", surfaces["metrics"])
	}
}
