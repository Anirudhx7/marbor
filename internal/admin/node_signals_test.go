package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

func TestNodeSignals_Table(t *testing.T) {
	warm := []router.ModelInfo{{Name: "llama3"}}
	tests := []struct {
		name string
		resp nodeResp
		want []string
	}{
		{"healthy idle", nodeResp{Health: "healthy"}, []string{"healthy"}},
		{"degraded", nodeResp{Health: "degraded"}, []string{"degraded"}},
		{"down", nodeResp{Health: "down"}, []string{"down"}},
		{"healthy and draining is draining only", nodeResp{Health: "healthy", Draining: true}, []string{"draining"}},
		{"degraded and draining is draining only", nodeResp{Health: "degraded", Draining: true}, []string{"draining"}},
		{"down and draining matches both", nodeResp{Health: "down", Draining: true}, []string{"draining", "down"}},
		{"agent present", nodeResp{Health: "healthy", AgentPresent: true}, []string{"healthy", "agent"}},
		{"stale agent is not present", nodeResp{Health: "healthy", AgentStale: true}, []string{"healthy"}},
		{"warm", nodeResp{Health: "healthy", LoadedModels: warm}, []string{"healthy", "warm"}},
		{"empty loaded models is not warm", nodeResp{Health: "healthy", LoadedModels: []router.ModelInfo{}}, []string{"healthy"}},
		{"pressure at 0.89", nodeResp{Health: "healthy", VRAMSource: "nvidia", VRAMTotalMB: 10000, VRAMUsedMB: 8900}, []string{"healthy"}},
		{"pressure at 0.90", nodeResp{Health: "healthy", VRAMSource: "nvidia", VRAMTotalMB: 10240, VRAMUsedMB: 9216}, []string{"healthy", "pressure"}},
		{"pressure at 0.91", nodeResp{Health: "healthy", VRAMSource: "nvidia", VRAMTotalMB: 10000, VRAMUsedMB: 9100}, []string{"healthy", "pressure"}},
		{"zero total never pressure", nodeResp{Health: "healthy", VRAMSource: "nvidia", VRAMTotalMB: 0, VRAMUsedMB: 500}, []string{"healthy"}},
		{"source none never pressure", nodeResp{Health: "healthy", VRAMSource: "none", VRAMTotalMB: 100, VRAMUsedMB: 100}, []string{"healthy"}},
		{"source empty counts like the dashboard", nodeResp{Health: "healthy", VRAMSource: "", VRAMTotalMB: 100, VRAMUsedMB: 95}, []string{"healthy", "pressure"}},
		{"source agent (any vendor)", nodeResp{Health: "healthy", VRAMSource: "agent", VRAMTotalMB: 196608, VRAMUsedMB: 190000}, []string{"healthy", "pressure"}},
		{"source declared", nodeResp{Health: "healthy", VRAMSource: "declared", VRAMTotalMB: 100, VRAMUsedMB: 99}, []string{"healthy", "pressure"}},
		{"source api", nodeResp{Health: "healthy", VRAMSource: "api", VRAMTotalMB: 100, VRAMUsedMB: 99}, []string{"healthy", "pressure"}},
		{
			"apple-style mlx node with no measured vram and an agent",
			nodeResp{Health: "healthy", Runtime: "mlx", VRAMSource: "none", AgentPresent: true, LoadedModels: warm},
			[]string{"healthy", "agent", "warm"},
		},
		{
			"everything at once keeps canonical order",
			nodeResp{Health: "down", Draining: true, AgentPresent: true, LoadedModels: warm, VRAMSource: "nvidia", VRAMTotalMB: 100, VRAMUsedMB: 100},
			[]string{"draining", "down", "agent", "warm", "pressure"},
		},
		{"agentless runtimes without telemetry", nodeResp{Health: "healthy", Runtime: "llamacpp"}, []string{"healthy"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nodeSignals(tt.resp)
			if got == nil {
				t.Fatal("nodeSignals returned nil, want a non-nil slice so JSON encodes []")
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("nodeSignals = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNodeSignals_NeverNilForUnknownHealth(t *testing.T) {
	got := nodeSignals(nodeResp{Health: "weird"})
	if got == nil || len(got) != 0 {
		t.Fatalf("got %#v, want empty non-nil slice", got)
	}
	b, _ := json.Marshal(got)
	if string(b) != "[]" {
		t.Fatalf("encoded as %s, want []", b)
	}
}

func TestNodeSignalIDs_CanonicalOrder(t *testing.T) {
	want := []string{"healthy", "degraded", "draining", "down", "agent", "warm", "pressure"}
	if !reflect.DeepEqual(NodeSignalIDs(), want) {
		t.Fatalf("NodeSignalIDs = %v, want %v", NodeSignalIDs(), want)
	}
}

// signalFleet builds a server over a small fleet with a spread of states:
//
//	a-healthy  healthy, warm, agent
//	b-degraded degraded
//	c-drain    healthy but draining
//	d-down     down (not draining)
//	e-pressure healthy at 95% vram
//	f-downdrain down and draining
func signalFleet(t *testing.T) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "signals.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "a-healthy", URL: "http://a:11434", Runtime: "ollama", GPUModel: "NVIDIA A100 80GB"},
		{Name: "b-degraded", URL: "http://b:11434", Runtime: "ollama", GPUModel: "NVIDIA RTX 4090"},
		{Name: "c-drain", URL: "http://c:11434", Runtime: "vllm", GPUModel: "NVIDIA H100"},
		{Name: "d-down", URL: "http://d:11434", Runtime: "mlx", GPUModel: "Apple M3 Max"},
		{Name: "e-pressure", URL: "http://e:11434", Runtime: "tgi", GPUModel: "AMD Instinct MI300X"},
		{Name: "f-downdrain", URL: "http://f:11434", Runtime: "llamacpp", GPUModel: "Intel Arc A770"},
	}, nil)
	set := func(name string, f func(n *router.NodeState)) {
		for _, n := range r.Nodes() {
			if n.Name == name {
				n.Lock()
				f(n)
				n.Unlock()
				return
			}
		}
		t.Fatalf("node %s not found", name)
	}
	for _, n := range r.Nodes() {
		n.Lock()
		n.Healthy = true
		n.Failures = 0
		n.VRAMSource = "nvidia"
		n.VRAMTotalMB = 10000
		n.VRAMUsedMB = 1000
		n.Unlock()
	}
	set("a-healthy", func(n *router.NodeState) {
		n.LoadedModels = []router.ModelInfo{{Name: "llama3"}}
		n.AgentPresent = true
	})
	set("b-degraded", func(n *router.NodeState) { n.Failures = 2 })
	set("c-drain", func(n *router.NodeState) { n.Draining = true })
	set("d-down", func(n *router.NodeState) { n.Healthy = false; n.VRAMSource = "none" })
	set("e-pressure", func(n *router.NodeState) { n.VRAMUsedMB = 9500 })
	set("f-downdrain", func(n *router.NodeState) { n.Healthy = false; n.Draining = true })
	return NewServer(r, nil, config.Config{}, st)
}

func getNodes(t *testing.T, s *Server, rawQuery string) (int, []byte) {
	t.Helper()
	target := "/admin/nodes"
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	rec := httptest.NewRecorder()
	s.handleNodes(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec.Code, rec.Body.Bytes()
}

func decodeNodes(t *testing.T, body []byte) []nodeResp {
	t.Helper()
	var out []nodeResp
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return out
}

func namesOf(ns []nodeResp) []string {
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		out = append(out, n.Name)
	}
	return out
}

func TestHandleNodes_NoParamsCarriesSignalsOnEveryNode(t *testing.T) {
	s := signalFleet(t)
	code, body := getNodes(t, s, "")
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
	}
	nodes := decodeNodes(t, body)
	if len(nodes) != 6 {
		t.Fatalf("got %d nodes, want 6", len(nodes))
	}
	want := map[string][]string{
		"a-healthy":   {"healthy", "agent", "warm"},
		"b-degraded":  {"degraded"},
		"c-drain":     {"draining"},
		"d-down":      {"down"},
		"e-pressure":  {"healthy", "pressure"},
		"f-downdrain": {"draining", "down"},
	}
	for _, n := range nodes {
		if !reflect.DeepEqual(n.Signals, want[n.Name]) {
			t.Errorf("%s signals = %v, want %v", n.Name, n.Signals, want[n.Name])
		}
	}
	if !strings.Contains(string(body), `"signals":[`) {
		t.Errorf("body missing signals array: %s", body)
	}
}

func TestHandleNodes_SignalFilter(t *testing.T) {
	s := signalFleet(t)
	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{"single", "signal=down", []string{"d-down", "f-downdrain"}},
		{"or across comma list", "signal=degraded,down", []string{"b-degraded", "d-down", "f-downdrain"}},
		{"repeated params", "signal=degraded&signal=draining", []string{"b-degraded", "c-drain", "f-downdrain"}},
		{"comma and repeated mixed", "signal=degraded,warm&signal=pressure", []string{"a-healthy", "b-degraded", "e-pressure"}},
		{"case insensitive", "signal=DOWN,Degraded", []string{"b-degraded", "d-down", "f-downdrain"}},
		{"whitespace trimmed", "signal=%20down%20,%20degraded", []string{"b-degraded", "d-down", "f-downdrain"}},
		{"empty elements dropped", "signal=,down,,", []string{"d-down", "f-downdrain"}},
		{"empty value is no filter", "signal=", []string{"a-healthy", "b-degraded", "c-drain", "d-down", "e-pressure", "f-downdrain"}},
		{"absent is no filter", "", []string{"a-healthy", "b-degraded", "c-drain", "d-down", "e-pressure", "f-downdrain"}},
		{"healthy excludes draining", "signal=healthy", []string{"a-healthy", "e-pressure"}},
		{"agent", "signal=agent", []string{"a-healthy"}},
		{"pressure", "signal=pressure", []string{"e-pressure"}},
		{"no match", "signal=agent&q=zzz", []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, body := getNodes(t, s, tt.query)
			if code != http.StatusOK {
				t.Fatalf("status %d: %s", code, body)
			}
			got := namesOf(decodeNodes(t, body))
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHandleNodes_UnknownSignalIs400ListingValidIDs(t *testing.T) {
	s := signalFleet(t)
	h := s.Handler()
	queries := []string{"signal=bogus", "signal=down,bogus", "signal=down&signal=nope", "signal=Bogus&signal=NOPE", "signal=DOWN,Bad,WORSE"}
	for _, base := range []string{"/admin/nodes", "/admin/v1/nodes"} {
		for _, q := range queries {
			req := httptest.NewRequest(http.MethodGet, base+"?"+q, nil)
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s?%s: status %d, want 400: %s", base, q, rec.Code, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("%s?%s: Content-Type %q, want JSON", base, q, ct)
			}
			var e map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
				t.Fatalf("%s?%s: error body is not the admin error JSON: %s", base, q, rec.Body.String())
			}
			msg := e["error"]
			for _, id := range NodeSignalIDs() {
				if !strings.Contains(msg, id) {
					t.Errorf("%s?%s: error %q does not list %q", base, q, msg, id)
				}
			}
		}
	}
}

func TestHandleNodes_UnknownSignalEchoIsTruncated(t *testing.T) {
	s := signalFleet(t)
	long := strings.Repeat("x", 5000)
	code, body := getNodes(t, s, "signal="+long)
	if code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", code)
	}
	if len(body) > 500 {
		t.Errorf("error body is %d bytes, want it capped well below the input size", len(body))
	}
	if !strings.Contains(string(body), "...") {
		t.Errorf("truncated echo should end in ...: %s", body)
	}
	for _, id := range NodeSignalIDs() {
		if !strings.Contains(string(body), id) {
			t.Errorf("error does not list %q: %s", id, body)
		}
	}
}

func TestHandleNodes_SearchFilter(t *testing.T) {
	s := signalFleet(t)
	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{"name substring", "q=drain", []string{"c-drain", "f-downdrain"}},
		{"gpu model substring case insensitive", "q=nvidia", []string{"a-healthy", "b-degraded", "c-drain"}},
		{"trimmed", "q=%20amd%20", []string{"e-pressure"}},
		{"uppercase q matches lowercase name", "q=D-DOWN", []string{"d-down"}},
		{"uppercase q matches mixed-case gpu model", "q=NVIDIA%20RTX", []string{"b-degraded"}},
		{"empty q is no filter", "q=", []string{"a-healthy", "b-degraded", "c-drain", "d-down", "e-pressure", "f-downdrain"}},
		{"q AND signal", "q=nvidia&signal=degraded,down", []string{"b-degraded"}},
		{"q matches nothing", "q=nothing-like-this", []string{}},
		{"host is not searched", "q=a:11434", []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, body := getNodes(t, s, tt.query)
			if code != http.StatusOK {
				t.Fatalf("status %d: %s", code, body)
			}
			got := namesOf(decodeNodes(t, body))
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHandleNodes_NoMatchEncodesEmptyArray(t *testing.T) {
	s := signalFleet(t)
	_, body := getNodes(t, s, "q=nothing-like-this")
	if strings.TrimSpace(string(body)) != "[]" {
		t.Fatalf("body = %q, want []", body)
	}
}

func TestHandleNodes_EmptyFleetEncodesEmptyArray(t *testing.T) {
	r := router.New(config.RoutingConfig{}, nil, nil)
	s := NewServer(r, nil, config.Config{})
	for _, q := range []string{"", "signal=down", "q=x"} {
		code, body := getNodes(t, s, q)
		if code != http.StatusOK || strings.TrimSpace(string(body)) != "[]" {
			t.Fatalf("%q: status %d body %q, want 200 []", q, code, body)
		}
	}
}

func TestHandleNodes_FilterKeepsOriginalIDsAndRoles(t *testing.T) {
	s := signalFleet(t)
	_, fullBody := getNodes(t, s, "")
	full := map[string]nodeResp{}
	for _, n := range decodeNodes(t, fullBody) {
		full[n.Name] = n
	}
	_, body := getNodes(t, s, "signal=down,degraded")
	filtered := decodeNodes(t, body)
	if len(filtered) != 3 {
		t.Fatalf("got %d filtered nodes: %v", len(filtered), namesOf(filtered))
	}
	for _, n := range filtered {
		f := full[n.Name]
		if n.ID != f.ID {
			t.Errorf("%s id = %s filtered, %s unfiltered; filtering must not re-index", n.Name, n.ID, f.ID)
		}
		if n.SchedulingRole != f.SchedulingRole || n.ReplicaHead != f.ReplicaHead {
			t.Errorf("%s role changed by filtering", n.Name)
		}
	}
	if full["d-down"].ID != "gpu-3" {
		t.Errorf("d-down id = %s, want gpu-3 (original index)", full["d-down"].ID)
	}
}

func TestHandleNodes_FilterDoesNotShiftReplicaRoles(t *testing.T) {
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "head", URL: "http://head:11434", Runtime: "vllm"},
		{Name: "worker", URL: "http://worker:11434", Runtime: "vllm"},
	}, nil)
	s := NewServer(r, nil, config.Config{})
	for _, name := range []string{"head", "worker"} {
		req := httptest.NewRequest(http.MethodPatch, "/admin/nodes/"+name,
			strings.NewReader(`{"replica_peers":{"members":["head","worker"],"head":"head"}}`))
		req.SetPathValue("name", name)
		rec := httptest.NewRecorder()
		s.handlePatchNode(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("patch %s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
	for _, n := range r.Nodes() {
		n.Lock()
		n.Healthy = n.Name == "head"
		n.Unlock()
	}
	_, body := getNodes(t, s, "signal=down")
	got := decodeNodes(t, body)
	if len(got) != 1 || got[0].Name != "worker" {
		t.Fatalf("got %v, want [worker]", namesOf(got))
	}
	if got[0].SchedulingRole != "worker" || got[0].ReplicaHead != "head" {
		t.Fatalf("worker role = %q head = %q, want worker/head (roles resolve fleet-wide before filtering)", got[0].SchedulingRole, got[0].ReplicaHead)
	}
}

func TestHandleNodes_AuthUnchangedWithParams(t *testing.T) {
	s := signalFleet(t)
	h := s.Handler()
	for _, target := range []string{"/admin/nodes?signal=down", "/admin/v1/nodes?signal=bogus&q=x"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401 for an unauthenticated request", target, rec.Code)
		}
	}
}

func TestHandleNode_SingleNodeCarriesSignals(t *testing.T) {
	s := signalFleet(t)
	req := httptest.NewRequest(http.MethodGet, "/admin/nodes/f-downdrain", nil)
	req.SetPathValue("name", "f-downdrain")
	rec := httptest.NewRecorder()
	s.handleNode(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var n nodeResp
	if err := json.Unmarshal(rec.Body.Bytes(), &n); err != nil {
		t.Fatal(err)
	}
	if want := []string{"draining", "down"}; !reflect.DeepEqual(n.Signals, want) {
		t.Fatalf("signals = %v, want %v", n.Signals, want)
	}
}

func TestHandleNodes_BothRoutesFilterAndCarrySignals(t *testing.T) {
	s := signalFleet(t)
	h := s.Handler()
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	for _, base := range []string{"/admin/nodes", "/admin/v1/nodes"} {
		rec := get(base + "?signal=degraded,down")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", base, rec.Code, rec.Body.String())
		}
		got := namesOf(decodeNodes(t, rec.Body.Bytes()))
		if want := []string{"b-degraded", "d-down", "f-downdrain"}; !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %v, want %v", base, got, want)
		}
		if rec := get(base + "?signal=bogus"); rec.Code != http.StatusBadRequest {
			t.Errorf("%s?signal=bogus: status %d, want 400", base, rec.Code)
		}
		one := get(base + "/f-downdrain")
		if one.Code != http.StatusOK || !strings.Contains(one.Body.String(), `"signals":["draining","down"]`) {
			t.Errorf("%s/f-downdrain: status %d body %s", base, one.Code, one.Body.String())
		}
	}
}

// TestHandleNodes_SignalsConcurrentReaders runs under -race: filtered and
// unfiltered list requests and single-node lookups run side by side, each
// computing signals inside the node read lock.
func TestHandleNodes_SignalsConcurrentReaders(t *testing.T) {
	s := signalFleet(t)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				query := []string{"", "signal=down,pressure,draining", "q=a", "signal=healthy&q=nvidia"}[(g+i)%4]
				target := "/admin/nodes"
				if query != "" {
					target += "?" + query
				}
				rec := httptest.NewRecorder()
				s.handleNodes(rec, httptest.NewRequest(http.MethodGet, target, nil))
				if rec.Code != http.StatusOK {
					t.Errorf("%s: status %d: %s", target, rec.Code, rec.Body.String())
					return
				}
				one := httptest.NewRequest(http.MethodGet, "/admin/nodes/a-healthy", nil)
				one.SetPathValue("name", "a-healthy")
				orec := httptest.NewRecorder()
				s.handleNode(orec, one)
				if orec.Code != http.StatusOK {
					t.Errorf("single node: status %d", orec.Code)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}
