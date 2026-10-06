package marboragent

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- docker socket connections are not left open between collections ----

func TestCollectFromDockerSocket_DoesNotLeaveConnectionsOpen(t *testing.T) {
	dir, err := os.MkdirTemp("", "dk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	id := strings.Repeat("a", 64)
	mux := http.NewServeMux()
	mux.HandleFunc("/containers/json", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `[{"Id":%q}]`, id)
	})
	mux.HandleFunc("/containers/"+id+"/json", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"State":{"Pid":0},"Config":{"Image":"vllm/vllm-openai","Cmd":["--port","8000"]},"NetworkSettings":{"Ports":{"8000/tcp":[{"HostPort":"8000"}]}}}`)
	})
	var mu sync.Mutex
	open := map[net.Conn]bool{}
	srv := &http.Server{Handler: mux, ConnState: func(c net.Conn, s http.ConnState) {
		mu.Lock()
		defer mu.Unlock()
		switch s {
		case http.StateNew:
			open[c] = true
		case http.StateClosed, http.StateHijacked:
			delete(open, c)
		}
	}}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	c := newDeploymentCollector(false)
	c.dockerSocket = sock
	c.ps = func() (string, error) { return "", nil }
	for i := 0; i < 5; i++ {
		if reps := c.Collect(nil); len(reps) != 1 {
			t.Fatalf("collect %d: want 1 report, got %v", i, reps)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(open)
		mu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d connections to the docker socket still open after collecting", n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ---- ps listing is bounded, stdout only ----

func TestPSListWith_TimesOutInsteadOfHanging(t *testing.T) {
	old := psTimeout
	psTimeout = 60 * time.Millisecond
	t.Cleanup(func() { psTimeout = old })
	calls := 0
	run := func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		calls++
		<-ctx.Done()
		return nil, ctx.Err()
	}
	start := time.Now()
	_, err := psListWith(run)
	if err == nil {
		t.Fatal("want an error from a hung ps")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("took %v", time.Since(start))
	}
	if calls != 1 {
		t.Fatalf("a timed-out ps must not be retried with the fallback command, got %d calls", calls)
	}
}

func TestPSListWith_FallsBackToSecondCommand(t *testing.T) {
	run := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if args[0] == "-eo" {
			return nil, fmt.Errorf("unsupported")
		}
		return []byte("1 0 init\n"), nil
	}
	out, err := psListWith(run)
	if err != nil || out != "1 0 init\n" {
		t.Fatalf("got %q %v", out, err)
	}
}

func TestLimitedBuffer_CapsAndNeverBlocksWriter(t *testing.T) {
	b := &limitedBuffer{max: 5}
	for _, chunk := range []string{"abc", "defgh", "ij"} {
		if n, err := b.Write([]byte(chunk)); n != len(chunk) || err != nil {
			t.Fatalf("write %q: %d %v", chunk, n, err)
		}
	}
	if string(b.buf) != "abcde" {
		t.Fatalf("got %q", b.buf)
	}
}

func TestPSHelperProcess(t *testing.T) {
	switch os.Getenv("MARBOR_PS_HELPER") {
	case "out":
		fmt.Fprintln(os.Stdout, "1 0 vllm serve m")
		fmt.Fprintln(os.Stderr, "ps: warning that must not be parsed")
		os.Exit(0)
	case "hang":
		time.Sleep(time.Minute)
		os.Exit(0)
	}
}

func TestRunPS_StdoutOnly(t *testing.T) {
	t.Setenv("MARBOR_PS_HELPER", "out")
	out, err := runPS(context.Background(), os.Args[0], "-test.run=^TestPSHelperProcess$")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "warning") || !strings.Contains(string(out), "vllm serve m") {
		t.Fatalf("got %q", out)
	}
}

func TestRunPS_KilledOnTimeout(t *testing.T) {
	t.Setenv("MARBOR_PS_HELPER", "hang")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := runPS(ctx, os.Args[0], "-test.run=^TestPSHelperProcess$"); err == nil {
		t.Fatal("want an error")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("took %v", time.Since(start))
	}
}

// ---- only real runtime processes are reported ----

func TestCollectFromPS_ClientCommandsNamingARuntimeAreIgnored(t *testing.T) {
	lines := []string{
		"10 1 tail -f /var/log/vllm.log",
		"11 1 docker logs vllm",
		"12 1 ssh gpu1 vllm serve foo --port 8000",
		"13 1 vim llama.cpp/README.md",
		"14 1 grep -r sglang /srv",
		"15 1 ollama runner --model x --port 40000",
		"16 1 /bin/bash -c vllm serve foo --port 8000",
	}
	det := []DetectedRuntime{{Name: "vllm", Port: 8000}}
	reps := fakeCollector(false, nil, nil).collectFromPS(strings.Join(lines, "\n")+"\n", det)
	if len(reps) != 0 {
		t.Fatalf("client commands must not become deployments: %+v", reps)
	}
}

func TestProcessRuntime_ServerProcessesStillRecognised(t *testing.T) {
	cases := map[string]string{
		"vllm serve foo": "vllm",
		"/opt/venv/bin/vllm serve foo --port 8000":                "vllm",
		"python3 -m vllm.entrypoints.openai.api_server --model m": "vllm",
		"/usr/bin/python3.11 -m sglang.launch_server --tp 2":      "sglang",
		"python /opt/vllm/serve.py --port 1":                      "",
		"text-generation-launcher --num-shard 2":                  "tgi",
		"llama-server -m x.gguf":                                  "llamacpp",
		"python -m llama_cpp.server --model x":                    "llamacpp",
		"python -m mlx_lm.server --model x":                       "mlx",
		"ollama serve":                                            "ollama",
		"ollama run llama3":                                       "",
		"tail -f vllm.log":                                        "",
	}
	for args, want := range cases {
		if got := processRuntime(strings.ToLower(args)); got != want {
			t.Errorf("%q: got %q want %q", args, got, want)
		}
	}
}

func TestCollectFromPS_LoneDetectedRuntimeOfAnotherKindIsNotLent(t *testing.T) {
	// A vllm process with no --port on a host whose only detected runtime is
	// ollama: it must not be reported as ollama on ollama's port.
	ps := "500 1 vllm serve foo\n"
	det := []DetectedRuntime{{Name: "ollama", Port: 11434}}
	reps := fakeCollector(false, nil, nil).collectFromPS(ps, det)
	if len(reps) != 1 || reps[0].Runtime != "vllm" || reps[0].Port != 0 {
		t.Fatalf("got %+v", reps)
	}
}

func TestExtractPortFromArgs_ShortFlagMustStartAToken(t *testing.T) {
	cases := []struct {
		args string
		want int
	}{
		{"llama-server --top-p 1 --min-p 5", 0},
		{"llama-server --top-p 1 --port 8080", 8080},
		{"server -p 9000", 9000},
		{"-p 9000 --model x", 9000},
		{"server --port=7000", 7000},
	}
	for _, c := range cases {
		if got := extractPortFromArgs(c.args); got != c.want {
			t.Errorf("%q: got %d want %d", c.args, got, c.want)
		}
	}
}

// ---- docker port selection ----

func dockerInspectJSON(cmd string, ports string, host string) string {
	return `{"State":{"Pid":0},"Config":{"Image":"vllm/vllm-openai:latest","Cmd":[` + cmd + `]},` +
		`"NetworkSettings":{"Ports":{` + ports + `}},"HostConfig":{` + host + `}}`
}

func TestParseDockerInspect_PortIsDeterministic(t *testing.T) {
	inspect := dockerInspectJSON(`"vllm","serve","m"`,
		`"9100/tcp":[{"HostPort":"9100"}],"8000/tcp":[{"HostPort":"8000"}],"7000/tcp":[{"HostPort":"7000"}]`, "")
	for i := 0; i < 50; i++ {
		d := parseDockerInspect(inspect)
		if d == nil || d.report.Port != 0 {
			t.Fatalf("several published ports and no --port is ambiguous, want 0: %+v", d)
		}
	}
}

func TestParseDockerInspect_ArgsPortMappedToHostPort(t *testing.T) {
	inspect := dockerInspectJSON(`"vllm","serve","m","--port","8000"`,
		`"9100/tcp":[{"HostPort":"9100"}],"8000/tcp":[{"HostPort":"18000"}]`, "")
	for i := 0; i < 50; i++ {
		if d := parseDockerInspect(inspect); d == nil || d.report.Port != 18000 {
			t.Fatalf("want the published host port 18000, got %+v", d)
		}
	}
}

func TestParseDockerInspect_UnpublishedArgsPortIsUnknown(t *testing.T) {
	inspect := dockerInspectJSON(`"vllm","serve","m","--port","8000"`, ``, `"NetworkMode":"bridge"`)
	if d := parseDockerInspect(inspect); d == nil || d.report.Port != 0 {
		t.Fatalf("a container-side port that is not published is not reachable on the host: %+v", d)
	}
	host := dockerInspectJSON(`"vllm","serve","m","--port","8000"`, ``, `"NetworkMode":"host"`)
	if d := parseDockerInspect(host); d == nil || d.report.Port != 8000 {
		t.Fatalf("host networking shares the port: %+v", d)
	}
}

func TestParseDockerInspect_SinglePublishedPortWithoutArgs(t *testing.T) {
	inspect := dockerInspectJSON(`"vllm","serve","m"`, `"8000/tcp":[{"HostPort":"8001"},{"HostPort":"8001"}]`, "")
	if d := parseDockerInspect(inspect); d == nil || d.report.Port != 8001 {
		t.Fatalf("got %+v", d)
	}
}

// ---- docker GPU scope ----

func TestParseDockerInspect_BakedNvidiaAllIsUnknownNotEveryGPU(t *testing.T) {
	inspect := `{"State":{"Pid":0},"Config":{"Image":"vllm/vllm-openai","Cmd":["vllm","serve","m"],"Env":["NVIDIA_VISIBLE_DEVICES=all","PATH=/usr/bin"]}}`
	d := parseDockerInspect(inspect)
	if d == nil || d.report.GPUScope == nil {
		t.Fatalf("got %+v", d)
	}
	sc := d.report.GPUScope
	if len(sc.Indices) != 0 || len(sc.UUIDs) != 0 || d.report.GPUGroup != nil {
		t.Fatalf("no devices may be claimed: %+v", sc)
	}
	if strings.Contains(sc.Note, "every GPU is visible") || !strings.Contains(sc.Note, "not known") {
		t.Fatalf("note must say unknown, got %q", sc.Note)
	}
}

func TestParseDockerInspect_GPUDeviceRequestWinsOverBakedAll(t *testing.T) {
	inspect := `{"State":{"Pid":0},"Config":{"Image":"vllm/vllm-openai","Cmd":["vllm","serve","m"],"Env":["NVIDIA_VISIBLE_DEVICES=all"]},` +
		`"HostConfig":{"DeviceRequests":[{"DeviceIDs":["0"]}]}}`
	d := parseDockerInspect(inspect)
	if d == nil || d.report.GPUScope == nil {
		t.Fatalf("got %+v", d)
	}
	if !reflect.DeepEqual(d.report.GPUScope.Indices, []int{0}) || !reflect.DeepEqual(d.report.GPUGroup, []int{0}) {
		t.Fatalf("want GPU 0, got %+v group %v", d.report.GPUScope, d.report.GPUGroup)
	}
}

func TestParseDockerInspect_ExplicitCudaIndicesStillWin(t *testing.T) {
	inspect := `{"State":{"Pid":0},"Config":{"Image":"vllm/vllm-openai","Cmd":["vllm","serve","m"],"Env":["CUDA_VISIBLE_DEVICES=2,3","NVIDIA_VISIBLE_DEVICES=all"]},` +
		`"HostConfig":{"DeviceRequests":[{"DeviceIDs":["0"]}]}}`
	d := parseDockerInspect(inspect)
	if d == nil || !reflect.DeepEqual(d.report.GPUScope.Indices, []int{2, 3}) {
		t.Fatalf("got %+v", d)
	}
}

// ---- note-only scopes are cross-checked ----

func TestApplyCrossCheck_NoteOnlyScopeGetsObservedGPUs(t *testing.T) {
	probe := &nvidiaProbe{
		pidUUIDs:  map[int][]string{500: {"GPU-bbbb1111"}},
		indexUUID: map[int]string{0: "GPU-aaaa0000", 1: "GPU-bbbb1111"},
	}
	sc := &GPUScope{Source: "docker-env:NVIDIA_VISIBLE_DEVICES", Raw: "all", Note: "unknown"}
	got, legacy := applyCrossCheck(sc, nil, probe, []int{500})
	if got == nil || !reflect.DeepEqual(got.Indices, []int{1}) || got.Source != "nvidia-compute-apps" || got.CrossChecked || legacy != nil {
		t.Fatalf("got %+v %v", got, legacy)
	}
	if !strings.Contains(got.Note, "unknown") {
		t.Fatalf("original note should be kept: %q", got.Note)
	}
	// Nothing observed: the note-only scope is left as it was.
	same, _ := applyCrossCheck(sc, nil, probe, []int{999})
	if same != sc {
		t.Fatalf("scope must be unchanged without observations: %+v", same)
	}
}
