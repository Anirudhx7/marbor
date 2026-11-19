package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

func TestNodeSignals_HeadlessWorker(t *testing.T) {
	warm := []router.ModelInfo{{Name: "m"}}
	tests := []struct {
		name string
		resp nodeResp
		want []string
	}{
		{"headless down emits no health signal", nodeResp{Health: "down", HeadlessWorker: true}, []string{}},
		{"headless degraded emits no health signal", nodeResp{Health: "degraded", HeadlessWorker: true}, []string{}},
		{"headless and draining is exactly draining", nodeResp{Health: "down", HeadlessWorker: true, Draining: true}, []string{"draining"}},
		{"headless keeps agent warm pressure", nodeResp{Health: "down", HeadlessWorker: true, AgentPresent: true, LoadedModels: warm, VRAMSource: "nvidia", VRAMTotalMB: 100, VRAMUsedMB: 100}, []string{"agent", "warm", "pressure"}},
		{"non headless down unchanged", nodeResp{Health: "down"}, []string{"down"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nodeSignals(tt.resp)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("nodeSignals = %v, want %v", got, tt.want)
			}
		})
	}
}

// workerServer builds head + w1 + w2 (group) + solo + orphan (unresolved).
func workerServer(t *testing.T) (*Server, *router.Router) {
	t.Helper()
	return workerServerWith(t, "vllm")
}

func workerServerWith(t *testing.T, runtime string) (*Server, *router.Router) {
	t.Helper()
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "head", URL: "http://head:8000", Runtime: runtime},
		{Name: "w1", URL: "http://w1:8000", Runtime: runtime},
		{Name: "w2", URL: "http://w2:8000", Runtime: runtime},
		{Name: "solo", URL: "http://solo:8000", Runtime: runtime},
		{Name: "orphan", URL: "http://orphan:8000", Runtime: runtime},
	}, nil)
	for _, n := range r.Nodes() {
		n.Lock()
		n.Healthy = true
		n.Failures = 0
		switch n.Name {
		case "head", "w1", "w2":
			n.ReplicaPeers = &store.ReplicaPeers{Members: []string{"head", "w1", "w2"}, Head: "head"}
		case "orphan":
			n.ReplicaPeers = &store.ReplicaPeers{Members: []string{"orphan", "ghost"}, Head: "orphan"}
		}
		n.Unlock()
	}
	return NewServer(r, nil, config.Config{}), r
}

func setNode(t *testing.T, r *router.Router, name string, f func(n *router.NodeState)) {
	t.Helper()
	for _, n := range r.Nodes() {
		if n.Name == name {
			n.Lock()
			f(n)
			n.Unlock()
			return
		}
	}
	t.Fatalf("node %s missing", name)
}

func failWorker(t *testing.T, r *router.Router, name string) {
	setNode(t, r, name, func(n *router.NodeState) { n.Healthy = false; n.Failures = 5 })
}

func TestHandleNodes_HeadlessWorkerField(t *testing.T) {
	s, r := workerServer(t)
	failWorker(t, r, "w1")
	setNode(t, r, "w2", func(n *router.NodeState) { n.Failures = 1 }) // Healthy=true, Failures=1
	failWorker(t, r, "solo")
	failWorker(t, r, "orphan")
	failWorker(t, r, "head")
	_, body := getNodes(t, s, "")
	byName := map[string]nodeResp{}
	for _, n := range decodeNodes(t, body) {
		byName[n.Name] = n
	}
	for name, want := range map[string]bool{"w1": true, "w2": true, "head": false, "solo": false, "orphan": false} {
		if got := byName[name].HeadlessWorker; got != want {
			t.Errorf("%s headlessWorker = %v, want %v", name, got, want)
		}
	}
	if byName["w1"].Health != "down" {
		t.Errorf("raw health for w1 = %q, want down", byName["w1"].Health)
	}
	if byName["w2"].Health != "degraded" {
		t.Errorf("raw health for w2 = %q, want degraded", byName["w2"].Health)
	}
	if len(byName["w1"].Signals) != 0 {
		t.Errorf("w1 signals = %v, want none", byName["w1"].Signals)
	}
	if !reflect.DeepEqual(byName["solo"].Signals, []string{"down"}) || !reflect.DeepEqual(byName["orphan"].Signals, []string{"down"}) {
		t.Errorf("solo/orphan must stay down: %v %v", byName["solo"].Signals, byName["orphan"].Signals)
	}
	if !strings.Contains(string(body), `"headlessWorker":true`) {
		t.Errorf("body missing headlessWorker: %s", body)
	}
}

func TestHandleNodes_HeadlessWorkerOmittedWhenFalse(t *testing.T) {
	s, _ := workerServer(t)
	_, body := getNodes(t, s, "")
	if strings.Contains(string(body), "headlessWorker") {
		t.Errorf("headlessWorker must be omitted when false: %s", body)
	}
}

func TestHandleNodes_SignalDownExcludesHeadless(t *testing.T) {
	s, r := workerServer(t)
	failWorker(t, r, "w1")
	failWorker(t, r, "solo")
	_, body := getNodes(t, s, "signal=down")
	if got := namesOf(decodeNodes(t, body)); !reflect.DeepEqual(got, []string{"solo"}) {
		t.Fatalf("down filter = %v, want [solo]", got)
	}
}

func TestHandleNode_HeadlessWorker(t *testing.T) {
	s, r := workerServer(t)
	setNode(t, r, "w1", func(n *router.NodeState) { n.Failures = 1 })
	req := httptest.NewRequest(http.MethodGet, "/admin/nodes/w1", nil)
	req.SetPathValue("name", "w1")
	rec := httptest.NewRecorder()
	s.handleNode(rec, req)
	var got nodeResp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.HeadlessWorker || got.Health != "degraded" {
		t.Errorf("handleNode = headless %v health %q, want true degraded", got.HeadlessWorker, got.Health)
	}
}

func TestHandleNodes_Law5RuntimeTable(t *testing.T) {
	for _, rt := range []string{"ollama", "mlx", "vllm", "llamacpp"} {
		t.Run(rt, func(t *testing.T) {
			s, r := workerServerWith(t, rt)
			failWorker(t, r, "w1")
			_, body := getNodes(t, s, "")
			for _, n := range decodeNodes(t, body) {
				if n.Name == "w1" && (!n.HeadlessWorker || len(n.Signals) != 0) {
					t.Errorf("%s worker: headless=%v signals=%v, want muted", rt, n.HeadlessWorker, n.Signals)
				}
			}
			code, h := getHealth(t, s)
			if code != http.StatusOK || h.Nodes.Workers != 1 || h.Nodes.Total != 4 {
				t.Errorf("%s health = %d %+v", rt, code, h.Nodes)
			}
		})
	}
}

type healthBody struct {
	Status string `json:"status"`
	Nodes  struct {
		Total   int `json:"total"`
		Healthy int `json:"healthy"`
		Workers int `json:"workers"`
	} `json:"nodes"`
}

func getHealth(t *testing.T, s *Server) (int, healthBody) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleHealth(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	var h healthBody
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return rec.Code, h
}

func TestHandleHealth_ExcludesFailingWorkers(t *testing.T) {
	s, r := workerServer(t)
	code, h := getHealth(t, s)
	if code != 200 || h.Nodes.Total != 5 || h.Nodes.Healthy != 5 || h.Nodes.Workers != 0 {
		t.Fatalf("baseline = %d %+v", code, h.Nodes)
	}
	failWorker(t, r, "w1")
	setNode(t, r, "w2", func(n *router.NodeState) { n.Failures = 1 }) // still Healthy, counted once as worker
	code, h = getHealth(t, s)
	if code != 200 || h.Nodes.Total != 3 || h.Nodes.Healthy != 3 || h.Nodes.Workers != 2 {
		t.Fatalf("with failing workers = %d %+v, want total 3 healthy 3 workers 2", code, h.Nodes)
	}
}

func TestHandleHealth_HeadDownWithFailingWorkersIs503(t *testing.T) {
	s, r := workerServer(t)
	for _, n := range []string{"solo", "orphan", "head"} {
		failWorker(t, r, n)
	}
	failWorker(t, r, "w1")
	failWorker(t, r, "w2")
	code, h := getHealth(t, s)
	if code != http.StatusServiceUnavailable || h.Status != "degraded" || h.Nodes.Healthy != 0 || h.Nodes.Total != 3 || h.Nodes.Workers != 2 {
		t.Fatalf("got %d %+v", code, h)
	}
}

func TestHandleHealth_AllWorkersNoHeadDefensive200(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "w1", URL: "http://w1:8000", Runtime: "vllm"},
	}, nil)
	s := NewServer(r, nil, config.Config{})
	// Force the defensive case with a pure helper call: every node a worker.
	roles := map[string]router.SchedulingRole{"w1": router.RoleWorker}
	setNode(t, r, "w1", func(n *router.NodeState) { n.Healthy = false; n.Failures = 3 })
	total, healthy, workers := fleetCounts(r.Nodes(), roles)
	if total != 0 || healthy != 0 || workers != 1 {
		t.Fatalf("fleetCounts = %d %d %d", total, healthy, workers)
	}
	_ = s
	if status := healthStatus(total, healthy); status != "ok" {
		t.Errorf("all-workers fleet status = %q, want ok", status)
	}
}

func TestHandleHealth_DrainingHeadStaysCounted(t *testing.T) {
	s, r := workerServer(t)
	setNode(t, r, "head", func(n *router.NodeState) { n.Draining = true })
	_, h := getHealth(t, s)
	if h.Nodes.Total != 5 || h.Nodes.Healthy != 5 {
		t.Fatalf("draining head must stay counted: %+v", h.Nodes)
	}
}

func TestHandleSummary_ExcludesFailingWorkers(t *testing.T) {
	s, r := workerServer(t)
	failWorker(t, r, "w1")
	failWorker(t, r, "solo")
	rec := httptest.NewRecorder()
	s.handleSummary(rec, httptest.NewRequest(http.MethodGet, "/admin/summary", nil))
	var got map[string]float64
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["total_nodes"] != 4 || got["nodes_online"] != 3 || got["nodes_workers"] != 1 {
		t.Fatalf("summary = %v, want total 4 online 3 workers 1", got)
	}
}

func TestHandleModels_CountsExcludeFailingWorkersButPerNodeStaysRaw(t *testing.T) {
	s, r := workerServer(t)
	for _, name := range []string{"head", "w1"} {
		setNode(t, r, name, func(n *router.NodeState) { n.LoadedModels = []router.ModelInfo{{Name: "big"}} })
	}
	failWorker(t, r, "w1")
	failWorker(t, r, "solo")
	rec := httptest.NewRecorder()
	s.handleModels(rec, httptest.NewRequest(http.MethodGet, "/admin/models", nil))
	var got struct {
		TotalNodes   int `json:"total_nodes"`
		HealthyNodes int `json:"healthy_nodes"`
		Models       []struct {
			Name       string `json:"name"`
			TotalNodes int    `json:"total_nodes"`
			Nodes      []struct {
				Name    string `json:"name"`
				Healthy bool   `json:"healthy"`
			} `json:"nodes"`
		} `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("%v: %s", err, rec.Body.String())
	}
	// top level: all minus workers = 4; healthy minus failing workers = 3 (head, w2, orphan).
	if got.TotalNodes != 4 || got.HealthyNodes != 3 {
		t.Errorf("top level total=%d healthy=%d, want 4 and 3", got.TotalNodes, got.HealthyNodes)
	}
	if len(got.Models) != 1 || got.Models[0].TotalNodes != 3 {
		t.Fatalf("per-model total_nodes = %+v, want 3", got.Models)
	}
	for _, n := range got.Models[0].Nodes {
		if n.Name == "w1" && n.Healthy {
			t.Error("per-node healthy for w1 must stay raw (false)")
		}
	}
}

func TestHandleHealth_BoundUnderLoad(t *testing.T) {
	cfgs := make([]config.NodeConfig, 200)
	for i := range cfgs {
		cfgs[i] = config.NodeConfig{Name: fmt.Sprintf("n%03d", i), URL: fmt.Sprintf("http://n%03d:8000", i), Runtime: "vllm"}
	}
	r := router.New(config.RoutingConfig{}, cfgs, nil)
	s := NewServer(r, nil, config.Config{})
	start := time.Now()
	for i := 0; i < 200; i++ {
		rec := httptest.NewRecorder()
		s.handleHealth(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("200 health calls over 200 nodes took %v, want under 5s", d)
	}
}

func BenchmarkHandleHealth20Nodes(b *testing.B) {
	cfgs := make([]config.NodeConfig, 20)
	for i := range cfgs {
		cfgs[i] = config.NodeConfig{Name: fmt.Sprintf("n%02d", i), URL: fmt.Sprintf("http://n%02d:8000", i), Runtime: "vllm"}
	}
	r := router.New(config.RoutingConfig{}, cfgs, nil)
	s := NewServer(r, nil, config.Config{})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := httptest.NewRecorder()
		s.handleHealth(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	}
}
