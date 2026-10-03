package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/marboragent"
	"github.com/Anirudhx7/marbor/internal/router"
)

func qc(tag string, est int64, fit, disk string, rec bool) quantCandidate {
	return quantCandidate{Tag: tag, Quantization: "Q-" + tag, VRAMEstMB: est, SizeMB: est, Fit: fit, DiskFit: disk, Recommended: rec}
}

func TestRecommendQuant(t *testing.T) {
	tests := []struct {
		name      string
		cands     []quantCandidate
		picked    bool
		tag       string
		tight     bool
		reason    string
		closestTg string
	}{
		{"recommended green beats larger green",
			[]quantCandidate{qc("q4", 100, "green", "ok", true), qc("q8", 200, "green", "ok", false)},
			true, "q4", false, "", ""},
		{"recommended red falls to largest green",
			[]quantCandidate{qc("q4", 100, "red", "ok", true), qc("q3", 50, "green", "ok", false), qc("q2", 70, "green", "ok", false)},
			true, "q2", false, "", ""},
		{"recommended disk-insufficient falls to next green",
			[]quantCandidate{qc("q4", 100, "green", "insufficient", true), qc("q3", 50, "green", "ok", false)},
			true, "q3", false, "", ""},
		{"unknown disk counts as ok",
			[]quantCandidate{qc("q4", 100, "green", "unknown", true)},
			true, "q4", false, "", ""},
		{"all green but disk insufficient",
			[]quantCandidate{qc("q4", 100, "green", "insufficient", true), qc("q8", 200, "yellow", "insufficient", false)},
			false, "", false, reasonDiskInsufficient, ""},
		{"recommended yellow with no green beats larger yellow",
			[]quantCandidate{qc("q8", 300, "yellow", "ok", false), qc("q4", 200, "yellow", "ok", true)},
			true, "q4", true, "", ""},
		{"no green and recommended red picks largest yellow",
			[]quantCandidate{qc("q4", 900, "red", "ok", true), qc("q3", 200, "yellow", "ok", false), qc("q2", 250, "yellow", "ok", false)},
			true, "q2", true, "", ""},
		{"repo style without recommended picks largest green",
			[]quantCandidate{qc("a", 100, "green", "ok", false), qc("b", 300, "green", "ok", false), qc("c", 500, "yellow", "ok", false)},
			true, "b", false, "", ""},
		{"repo style yellow fallback",
			[]quantCandidate{qc("a", 100, "red", "ok", false), qc("b", 300, "yellow", "ok", false)},
			true, "b", true, "", ""},
		{"all red is too large with smallest as closest",
			[]quantCandidate{qc("big", 900, "red", "ok", true), qc("small", 500, "red", "ok", false)},
			false, "", false, reasonTooLarge, "small"},
		{"all unknown fit is vram unknown",
			[]quantCandidate{qc("q4", 100, "unknown", "ok", true)},
			false, "", false, reasonVRAMUnknown, ""},
		{"unknown size never becomes a green pick",
			[]quantCandidate{qc("q4", 0, "green", "ok", true)},
			false, "", false, reasonVRAMUnknown, ""},
		{"all incompatible runtime",
			[]quantCandidate{qc("q4", 100, "incompatible", "ok", true), qc("q8", 200, "incompatible", "ok", false)},
			false, "", false, reasonIncompatibleRuntime, ""},
		{"no variants",
			nil,
			false, "", false, reasonNoVariants, ""},
		{"tie keeps list order",
			[]quantCandidate{qc("first", 100, "green", "ok", false), qc("second", 100, "green", "ok", false)},
			true, "first", false, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := recommendQuant(tt.cands)
			if got.Picked != tt.picked || got.Tag != tt.tag || got.Tight != tt.tight || got.Reason != tt.reason || got.ClosestTag != tt.closestTg {
				t.Fatalf("got %+v, want picked=%v tag=%q tight=%v reason=%q closest=%q", got, tt.picked, tt.tag, tt.tight, tt.reason, tt.closestTg)
			}
			if got.Picked && (got.Fit == "" || got.VRAMEstMB <= 0) {
				t.Errorf("picked recommendation missing fit or vram estimate: %+v", got)
			}
		})
	}
}

func catalogFor(t *testing.T, s *Server) catalogResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/admin/models/catalog", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp catalogResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

func recFor(t *testing.T, n catalogNodeEntry, name string) quantRecommendation {
	t.Helper()
	for _, m := range n.Models {
		if m.Name == name {
			return m.Recommendation
		}
	}
	t.Fatalf("model %q not in node entry", name)
	return quantRecommendation{}
}

func singleNodeServer(t *testing.T, url, runtime string, mutate func(n *router.NodeState)) *Server {
	t.Helper()
	r := router.New(config.RoutingConfig{Strategy: "warm-first", Fallback: "least-connections", PollIntervalMs: 60000},
		[]config.NodeConfig{{Name: "n1", URL: url, Runtime: runtime}}, nil)
	n := r.Nodes()[0]
	n.Lock()
	n.Healthy = true
	mutate(n)
	n.Unlock()
	return NewServer(r, nil, config.Config{})
}

func TestHandleModelCatalog_Recommendation(t *testing.T) {
	ollama := mockOllamaServer(t)
	defer ollama.Close()

	// 8 GB node: the recommended Q4_K_M of the 8B model fits green and wins over
	// the larger Q8 (which is red); the 70B has nothing that fits.
	resp := catalogFor(t, newModelFitTestServer(ollama.URL))
	n := resp.Nodes[0]
	if got := recFor(t, n, "llama3.1:8b"); !got.Picked || got.Tag != "llama3.1:8b" || got.Tight || got.Fit != "green" {
		t.Errorf("llama3.1:8b recommendation = %+v, want picked recommended Q4 green", got)
	}
	if got := recFor(t, n, "llama3.1:70b"); got.Picked || got.Reason != reasonTooLarge || got.ClosestTag != "llama3.1:70b" {
		t.Errorf("llama3.1:70b recommendation = %+v, want too_large with closest llama3.1:70b", got)
	}
}

func TestHandleModelCatalog_RecommendationUnknownVRAM(t *testing.T) {
	ollama := mockOllamaServer(t)
	defer ollama.Close()
	s := singleNodeServer(t, ollama.URL, "ollama", func(n *router.NodeState) {})
	n := catalogFor(t, s).Nodes[0]
	if got := recFor(t, n, "llama3.2:1b"); got.Picked || got.Reason != reasonVRAMUnknown {
		t.Errorf("recommendation on a node with no VRAM reading = %+v, want vram_unknown", got)
	}
}

func TestHandleModelCatalog_RecommendationCombinedBasis(t *testing.T) {
	ollama := mockOllamaServer(t)
	defer ollama.Close()
	s := singleNodeServer(t, ollama.URL, "ollama", func(n *router.NodeState) {
		n.AgentPresent = true
		n.VRAMSource = "agent"
		n.VRAMTotalMB = 2 * 24576
		n.AgentGPUs = []marboragent.GPUInfo{{Index: 0, VRAMTotalMB: 24576}, {Index: 1, VRAMTotalMB: 24576}}
	})
	n := catalogFor(t, s).Nodes[0]
	if n.VRAMFitBasis != "combined" {
		t.Fatalf("vram_fit_basis = %q, want combined", n.VRAMFitBasis)
	}
	// 70B Q4 (40960 MB) only fits the summed 48 GB, not one 24 GB card.
	if got := recFor(t, n, "llama3.1:70b"); !got.Picked || got.Tag != "llama3.1:70b" {
		t.Errorf("70B recommendation on 2x24GB ollama node = %+v, want picked", got)
	}
}

func TestHandleModelCatalog_RecommendationAppleUnified(t *testing.T) {
	ollama := mockOllamaServer(t)
	defer ollama.Close()
	// Apple Silicon reports unified memory as a single VRAMTotalMB figure with
	// no per-device breakdown; that must size the verdicts like any other node.
	s := singleNodeServer(t, ollama.URL, "ollama", func(n *router.NodeState) {
		n.AgentPresent = true
		n.VRAMSource = "agent"
		n.VRAMTotalMB = 65536
	})
	n := catalogFor(t, s).Nodes[0]
	if got := recFor(t, n, "llama3.1:70b"); !got.Picked || got.Tag != "llama3.1:70b" {
		t.Errorf("70B recommendation on a 64 GB unified-memory node = %+v, want picked", got)
	}
}

func TestHandleModelCatalog_RecommendationIncompatibleRuntime(t *testing.T) {
	ollama := mockOllamaServer(t)
	defer ollama.Close()
	s := singleNodeServer(t, ollama.URL, "llamacpp", func(n *router.NodeState) {
		n.VRAMTotalMB = 80 * 1024
		n.VRAMSource = "declared"
	})
	n := catalogFor(t, s).Nodes[0]
	if got := recFor(t, n, "llama3.1:8b"); got.Picked || got.Reason != reasonIncompatibleRuntime {
		t.Errorf("recommendation on a llamacpp node = %+v, want incompatible_runtime", got)
	}
}

func repoFor(t *testing.T, s *Server, hfBody, query string) map[string]json.RawMessage {
	t.Helper()
	orig := hfHTTPClient.Transport
	hfHTTPClient.Transport = stubRoundTripper{body: hfBody}
	defer func() { hfHTTPClient.Transport = orig }()
	req := httptest.NewRequest(http.MethodGet, "/admin/models/repo?"+query, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func TestHandleModelRepo_Recommendation(t *testing.T) {
	ollama := mockOllamaServer(t)
	defer ollama.Close()
	s := newModelFitTestServer(ollama.URL)

	// 8 GB node. Q4 (~4.4 GB file) fits; Q8 (~8.4 GB file) does not.
	gguf := `{"id":"o/m-GGUF","tags":[],"siblings":[
		{"rfilename":"m-Q4_K_M.gguf","size":4294967296},
		{"rfilename":"m-Q8_0.gguf","size":8589934592}]}`
	out := repoFor(t, s, gguf, "id=o/m-GGUF&node=test-node")
	var rec quantRecommendation
	if err := json.Unmarshal(out["recommendation"], &rec); err != nil {
		t.Fatalf("recommendation missing or malformed: %v (%s)", err, out["recommendation"])
	}
	if !rec.Picked || rec.Quantization != "Q4_K_M" {
		t.Errorf("recommendation = %+v, want picked Q4_K_M", rec)
	}
}

func TestHandleModelRepo_RecommendationLargestBasis(t *testing.T) {
	ollama := mockOllamaServer(t)
	defer ollama.Close()
	// vLLM pins a model to one card: a 30 GB safetensors repo does not fit one
	// 24 GB card even though the two cards together hold 48 GB.
	s := singleNodeServer(t, ollama.URL, "vllm", func(n *router.NodeState) {
		n.AgentPresent = true
		n.VRAMSource = "agent"
		n.VRAMTotalMB = 2 * 24576
		n.AgentGPUs = []marboragent.GPUInfo{{Index: 0, VRAMTotalMB: 24576}, {Index: 1, VRAMTotalMB: 24576}}
	})
	st := `{"id":"o/m","tags":[],"siblings":[{"rfilename":"model.safetensors","size":32212254720}]}`
	out := repoFor(t, s, st, "id=o/m&node=n1&runtime=vllm")
	var basis string
	_ = json.Unmarshal(out["vram_fit_basis"], &basis)
	var rec quantRecommendation
	if err := json.Unmarshal(out["recommendation"], &rec); err != nil {
		t.Fatalf("decode recommendation: %v", err)
	}
	if basis != "largest" || rec.Picked || rec.Reason != reasonTooLarge {
		t.Errorf("basis=%q rec=%+v, want largest basis and too_large", basis, rec)
	}
}
