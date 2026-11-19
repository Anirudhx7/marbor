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

// The rows differ only in the runtime string: the worker role is a
// declaration, so muting must not depend on which runtime the node runs.
func TestHandleNodes_Law5RuntimeTable(t *testing.T) {
	for _, rt := range []string{"ollama", "mlx", "vllm", "llamacpp"} {
		t.Run(rt, func(t *testing.T) {
			s, r := workerServerWith(t, rt)
			failWorker(t, r, "w1")
			_, body := getNodes(t, s, "")
			found := false
			for _, n := range decodeNodes(t, body) {
				if n.Name != "w1" {
					continue
				}
				found = true
				if !n.HeadlessWorker || len(n.Signals) != 0 {
					t.Errorf("%s worker: headless=%v signals=%v, want muted", rt, n.HeadlessWorker, n.Signals)
				}
			}
			if !found {
				t.Fatalf("%s: node w1 missing from the node list", rt)
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

// A fleet with nothing to count (here: no nodes at all) is ok, never degraded.
func TestHandleHealth_EmptyFleetIs200(t *testing.T) {
	r := router.New(config.RoutingConfig{}, nil, nil)
	s := NewServer(r, nil, config.Config{})
	code, h := getHealth(t, s)
	if code != http.StatusOK || h.Status != "ok" || h.Nodes.Total != 0 || h.Nodes.Healthy != 0 || h.Nodes.Workers != 0 {
		t.Fatalf("empty fleet = %d %+v, want 200 ok and zero counts", code, h)
	}
}

func TestFleetCounts(t *testing.T) {
	type nodeSpec struct {
		name     string
		healthy  bool
		failures int
	}
	w := router.RoleWorker
	cases := []struct {
		name                    string
		nodes                   []nodeSpec
		roles                   map[string]router.SchedulingRole
		total, healthy, workers int
		status                  string
	}{
		{"empty fleet", nil, map[string]router.SchedulingRole{}, 0, 0, 0, "ok"},
		{"nil roles count everything", []nodeSpec{{"a", true, 0}, {"b", false, 3}}, nil, 2, 1, 0, "ok"},
		{"node missing from roles is not a worker", []nodeSpec{{"a", false, 3}}, map[string]router.SchedulingRole{"zzz": w}, 1, 0, 0, "degraded"},
		{"all non-worker unhealthy", []nodeSpec{{"a", false, 3}, {"b", false, 3}}, map[string]router.SchedulingRole{"a": router.RoleHead, "b": router.RoleStandalone}, 2, 0, 0, "degraded"},
		{"failing worker excluded", []nodeSpec{{"head", true, 0}, {"w", false, 3}}, map[string]router.SchedulingRole{"head": router.RoleHead, "w": w}, 1, 1, 1, "ok"},
		{"healthy worker with one failure excluded", []nodeSpec{{"head", true, 0}, {"w", true, 1}}, map[string]router.SchedulingRole{"head": router.RoleHead, "w": w}, 1, 1, 1, "ok"},
		{"healthy worker counted", []nodeSpec{{"head", true, 0}, {"w", true, 0}}, map[string]router.SchedulingRole{"head": router.RoleHead, "w": w}, 2, 2, 0, "ok"},
		{"head down with failing worker", []nodeSpec{{"head", false, 3}, {"w", false, 3}}, map[string]router.SchedulingRole{"head": router.RoleHead, "w": w}, 1, 0, 1, "degraded"},
		{"all workers, nothing counted", []nodeSpec{{"w", false, 3}}, map[string]router.SchedulingRole{"w": w}, 0, 0, 1, "ok"},
		{"unresolved failing node still counted", []nodeSpec{{"o", false, 3}}, map[string]router.SchedulingRole{"o": router.RoleUnresolved}, 1, 0, 0, "degraded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfgs := make([]config.NodeConfig, len(tc.nodes))
			for i, n := range tc.nodes {
				cfgs[i] = config.NodeConfig{Name: n.name, URL: "http://" + n.name + ":8000", Runtime: "vllm"}
			}
			r := router.New(config.RoutingConfig{}, cfgs, nil)
			for _, n := range tc.nodes {
				setNode(t, r, n.name, func(ns *router.NodeState) { ns.Healthy = n.healthy; ns.Failures = n.failures })
			}
			total, healthy, workers := fleetCounts(r.Nodes(), tc.roles)
			if total != tc.total || healthy != tc.healthy || workers != tc.workers {
				t.Fatalf("fleetCounts = %d %d %d, want %d %d %d", total, healthy, workers, tc.total, tc.healthy, tc.workers)
			}
			if got := healthStatus(total, healthy); got != tc.status {
				t.Errorf("healthStatus(%d,%d) = %q, want %q", total, healthy, got, tc.status)
			}
		})
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
	failWorker(t, r, "orphan") // a non-worker failure: healthy count differs from the all-nodes count
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
	// top level: all 5 nodes minus the failing worker = 4; healthy minus the
	// failing worker = 2 (head, w2).
	if got.TotalNodes != 4 || got.HealthyNodes != 2 {
		t.Errorf("top level total=%d healthy=%d, want 4 and 2", got.TotalNodes, got.HealthyNodes)
	}
	// per-model total_nodes is the healthy count (healthy minus failing
	// workers), a different quantity from the top-level total_nodes.
	if len(got.Models) != 1 || got.Models[0].TotalNodes != 2 {
		t.Fatalf("per-model total_nodes = %+v, want 2", got.Models)
	}
	if got.Models[0].TotalNodes == got.TotalNodes {
		t.Errorf("per-model total_nodes (%d) must differ from top-level total_nodes (%d)", got.Models[0].TotalNodes, got.TotalNodes)
	}
	foundW1 := false
	for _, n := range got.Models[0].Nodes {
		if n.Name != "w1" {
			continue
		}
		foundW1 = true
		if n.Healthy {
			t.Error("per-node healthy for w1 must stay raw (false)")
		}
	}
	if !foundW1 {
		t.Fatalf("w1 missing from the model's node list: %+v", got.Models[0].Nodes)
	}
}

// groupedFleet builds n nodes as replica groups of one head and three workers,
// with every worker failing, so role resolution does real work.
func groupedFleet(tb testing.TB, n int) *Server {
	tb.Helper()
	cfgs := make([]config.NodeConfig, n)
	for i := range cfgs {
		cfgs[i] = config.NodeConfig{Name: fmt.Sprintf("n%03d", i), URL: fmt.Sprintf("http://n%03d:8000", i), Runtime: "vllm"}
	}
	r := router.New(config.RoutingConfig{}, cfgs, nil)
	nodes := r.Nodes()
	for g := 0; g+4 <= len(nodes); g += 4 {
		members := make([]string, 4)
		for k := range members {
			members[k] = nodes[g+k].Name
		}
		for k := 0; k < 4; k++ {
			nodes[g+k].Lock()
			nodes[g+k].ReplicaPeers = &store.ReplicaPeers{Members: members, Head: members[0]}
			nodes[g+k].Healthy = k == 0
			if k != 0 {
				nodes[g+k].Failures = 3
			}
			nodes[g+k].Unlock()
		}
	}
	return NewServer(r, nil, config.Config{})
}

func TestHealthSummaryCatalog_BoundUnderLoad(t *testing.T) {
	s := groupedFleet(t, 200)
	// The catalog does far more per node than the other two, so it gets fewer calls.
	calls := []struct {
		target string
		h      func(http.ResponseWriter, *http.Request)
		n      int
	}{
		{"/health", s.handleHealth, 200},
		{"/admin/summary", s.handleSummary, 200},
		{"/admin/models", s.handleModels, 10},
	}
	for _, c := range calls {
		start := time.Now()
		for i := 0; i < c.n; i++ {
			rec := httptest.NewRecorder()
			c.h(rec, httptest.NewRequest(http.MethodGet, c.target, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: status %d", c.target, rec.Code)
			}
		}
		// Generous wall-clock bound on the whole loop of calls for this handler,
		// not per call: it only has to catch a quadratic or lock-convoy regression.
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("%d %s calls over 200 grouped nodes took %v, want under 5s", c.n, c.target, d)
		}
	}
}

func benchHandler(b *testing.B, target string, pick func(*Server) func(http.ResponseWriter, *http.Request)) {
	s := groupedFleet(b, 20)
	h := pick(s)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, target, nil))
	}
}

func BenchmarkHandleHealth20Nodes(b *testing.B) {
	benchHandler(b, "/health", func(s *Server) func(http.ResponseWriter, *http.Request) { return s.handleHealth })
}

func BenchmarkHandleSummary20Nodes(b *testing.B) {
	benchHandler(b, "/admin/summary", func(s *Server) func(http.ResponseWriter, *http.Request) { return s.handleSummary })
}

func BenchmarkHandleModels20Nodes(b *testing.B) {
	benchHandler(b, "/admin/models", func(s *Server) func(http.ResponseWriter, *http.Request) { return s.handleModels })
}

func getSummary(t *testing.T, s *Server) map[string]float64 {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleSummary(rec, httptest.NewRequest(http.MethodGet, "/admin/summary", nil))
	var got map[string]float64
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("%v: %s", err, rec.Body.String())
	}
	return got
}

func TestHandleSummary_WorkerWithOneFailureAndNoWorkers(t *testing.T) {
	s, r := workerServer(t)
	got := getSummary(t, s)
	if v, ok := got["nodes_workers"]; !ok || v != 0 {
		t.Fatalf("nodes_workers = %v,%v with no failing worker, want present and 0", v, ok)
	}
	if got["total_nodes"] != 5 || got["nodes_online"] != 5 {
		t.Fatalf("summary = %v, want total 5 online 5", got)
	}
	setNode(t, r, "w2", func(n *router.NodeState) { n.Failures = 1 }) // Healthy=true, Failures=1
	setNode(t, r, "solo", func(n *router.NodeState) { n.Draining = true })
	got = getSummary(t, s)
	if got["nodes_workers"] != 1 || got["total_nodes"] != 4 || got["nodes_online"] != 4 {
		t.Fatalf("summary = %v, want workers 1 total 4 online 4", got)
	}
	if got["nodes_draining"] != 1 {
		t.Fatalf("nodes_draining = %v, want 1 (unaffected by worker muting)", got["nodes_draining"])
	}
}

// A draining worker is still a failing worker: exactly draining in the node
// list, excluded from the counts everywhere, and still counted as draining.
func TestHeadlessWorker_DrainingAcrossSurfaces(t *testing.T) {
	s, r := workerServer(t)
	failWorker(t, r, "w1")
	setNode(t, r, "w1", func(n *router.NodeState) { n.Draining = true })

	_, body := getNodes(t, s, "")
	foundW1 := false
	for _, n := range decodeNodes(t, body) {
		if n.Name != "w1" {
			continue
		}
		foundW1 = true
		if !n.HeadlessWorker || !reflect.DeepEqual(n.Signals, []string{"draining"}) {
			t.Errorf("w1 = headless %v signals %v, want true [draining]", n.HeadlessWorker, n.Signals)
		}
	}
	if !foundW1 {
		t.Fatal("node w1 missing from the node list")
	}
	code, h := getHealth(t, s)
	if code != http.StatusOK || h.Nodes.Total != 4 || h.Nodes.Workers != 1 {
		t.Errorf("health = %d %+v, want total 4 workers 1", code, h.Nodes)
	}
	sum := getSummary(t, s)
	if sum["total_nodes"] != 4 || sum["nodes_workers"] != 1 || sum["nodes_draining"] != 1 {
		t.Errorf("summary = %v, want total 4 workers 1 draining 1", sum)
	}
	rec := httptest.NewRecorder()
	s.handleModels(rec, httptest.NewRequest(http.MethodGet, "/admin/models", nil))
	var cat struct {
		TotalNodes int `json:"total_nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cat); err != nil || cat.TotalNodes != 4 {
		t.Errorf("catalog total_nodes = %d (%v), want 4", cat.TotalNodes, err)
	}
}

func getHeadlessNode(t *testing.T, s *Server, name string) (int, nodeResp) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/admin/nodes/"+name, nil)
	req.SetPathValue("name", name)
	rec := httptest.NewRecorder()
	s.handleNode(rec, req)
	var got nodeResp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return rec.Code, got
}

func TestHandleNode_HeadlessWorkerDownAndNonWorkerNegative(t *testing.T) {
	s, r := workerServer(t)
	failWorker(t, r, "w1")
	failWorker(t, r, "solo")
	failWorker(t, r, "orphan")
	code, got := getHeadlessNode(t, s, "w1")
	if code != http.StatusOK || !got.HeadlessWorker || got.Health != "down" {
		t.Errorf("w1 = %d headless %v health %q, want 200 true down", code, got.HeadlessWorker, got.Health)
	}
	for _, name := range []string{"solo", "orphan"} {
		code, got = getHeadlessNode(t, s, name)
		if code != http.StatusOK || got.HeadlessWorker || got.Health != "down" || !reflect.DeepEqual(got.Signals, []string{"down"}) {
			t.Errorf("%s = %d headless %v health %q signals %v, want 200 false down [down]", name, code, got.HeadlessWorker, got.Health, got.Signals)
		}
	}
	code, got = getHeadlessNode(t, s, "head")
	if code != http.StatusOK || got.HeadlessWorker {
		t.Errorf("healthy head = %d headless %v, want 200 false", code, got.HeadlessWorker)
	}
}
