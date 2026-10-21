package admin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/marboragent"
	"github.com/Anirudhx7/marbor/internal/router"
)

// stubRoundTripper returns a canned response for every request, letting a
// test stand in for the real huggingface.co API without a network call.
type stubRoundTripper struct {
	body string
}

func (s stubRoundTripper) RoundTrip(_ *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(s.body)),
		Header:     make(http.Header),
	}, nil
}

// TestHandleModelRepo_ExcludesMmprojFiles is a regression test for the bug
// where a multimodal repo's mmproj-*.gguf vision-projector companion file
// (a few hundred MB, unrelated to the main model's actual size) got read as
// if it were a legitimate "F16"/"BF16"/"F32" quantization of the model
// itself - extractQuantization finds that substring in "mmproj-F16.gguf"
// same as it would in the real model's filename, with no filename prefix
// check to tell the two apart. Confirmed against unsloth/Qwen3.5-4B-GGUF on
// 2026-08-04: real Qwen3.5-4B-BF16.gguf is 8.42 GB; mmproj-F16.gguf is 641 MB
// but was shown as a "F16" variant of the 4B model.
func TestHandleModelRepo_ExcludesMmprojFiles(t *testing.T) {
	fakeHF := `{
		"id": "unsloth/Qwen3.5-4B-GGUF",
		"downloads": 1169980,
		"likes": 357,
		"tags": ["image-text-to-text"],
		"lastModified": "2026-01-01T00:00:00.000Z",
		"siblings": [
			{"rfilename": "Qwen3.5-4B-BF16.gguf", "size": 8424393632},
			{"rfilename": "Qwen3.5-4B-Q6_K.gguf", "size": 3525956768},
			{"rfilename": "mmproj-F16.gguf", "size": 672423616},
			{"rfilename": "mmproj-BF16.gguf", "size": 675569344},
			{"rfilename": "mmproj-F32.gguf", "size": 1334075072}
		]
	}`
	origTransport := hfHTTPClient.Transport
	hfHTTPClient.Transport = stubRoundTripper{body: fakeHF}
	defer func() { hfHTTPClient.Transport = origTransport }()

	ollama := mockOllamaServer(t)
	defer ollama.Close()
	s := newModelFitTestServer(ollama.URL)

	req := httptest.NewRequest(http.MethodGet, "/admin/models/repo?id=unsloth/Qwen3.5-4B-GGUF", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		Variants []ModelVariantFit `json:"variants"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	// The three mmproj-*.gguf siblings (641/644/1272 MB) must never appear at
	// all - checking by size (not just quant label) since the bug's whole
	// symptom was an mmproj file surfacing under a real-looking quant label.
	for _, v := range resp.Variants {
		if v.SizeMB < 2000 {
			t.Errorf("variant %q (quant %q, %d MB) is small enough to be an mmproj companion file mislabeled as a model variant - must be excluded", v.Tag, v.Quantization, v.SizeMB)
		}
	}

	// The two real main-model files must still appear, unaffected -
	// excluding mmproj must not over-filter and drop legitimate variants.
	foundRealBF16, foundRealQ6K := false, false
	for _, v := range resp.Variants {
		if strings.Contains(strings.ToLower(v.Tag), "bf16") && v.SizeMB > 7000 {
			foundRealBF16 = true
		}
		if strings.Contains(strings.ToLower(v.Tag), "q6_k") && v.SizeMB > 3000 {
			foundRealQ6K = true
		}
	}
	if !foundRealBF16 {
		t.Error("expected the real Qwen3.5-4B-BF16.gguf (~8 GB) to still appear as a variant - only the mmproj files should be excluded")
	}
	if !foundRealQ6K {
		t.Error("expected the real Qwen3.5-4B-Q6_K.gguf (~3.3 GB) to still appear as a variant - only the mmproj files should be excluded")
	}
	if len(resp.Variants) != 2 {
		t.Errorf("got %d variants, want exactly 2 (the real BF16 and Q6_K files) - the three mmproj files must all be filtered out", len(resp.Variants))
	}
}

// TestHandleModelRepo_OneVariantPerTag covers a repo where two files resolve to
// the same "repo:quant" tag: an mmproj projector kept in a subfolder, and an
// auxiliary file sharing the main weights' quant label. The list must hold one
// row per tag (the largest file), so the Pick badge, matched by tag, lands on
// a single row.
func TestHandleModelRepo_OneVariantPerTag(t *testing.T) {
	fakeHF := `{
		"id": "unsloth/Qwen3.8-27B-GGUF",
		"downloads": 1,
		"likes": 1,
		"tags": ["text-generation"],
		"lastModified": "2026-01-01T00:00:00.000Z",
		"siblings": [
			{"rfilename": "Qwen3.8-27B-aux-Q4_0.gguf", "size": 1395864371},
			{"rfilename": "Qwen3.8-27B-Q4_0.gguf", "size": 16106127360},
			{"rfilename": "BF16/mmproj-BF16.gguf", "size": 1395864371}
		]
	}`
	origTransport := hfHTTPClient.Transport
	hfHTTPClient.Transport = stubRoundTripper{body: fakeHF}
	defer func() { hfHTTPClient.Transport = origTransport }()

	ollama := mockOllamaServer(t)
	defer ollama.Close()
	s := newModelFitTestServer(ollama.URL)

	req := httptest.NewRequest(http.MethodGet, "/admin/models/repo?id=unsloth/Qwen3.8-27B-GGUF", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		Variants []ModelVariantFit `json:"variants"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Variants) != 1 || resp.Variants[0].SizeMB < 15000 {
		t.Fatalf("variants = %+v, want exactly one Q4_0 row for the ~15 GB main weights", resp.Variants)
	}
}

// TestHandleModelRepo_ExcludesShardedGGUF is a regression test for the bug
// where a sharded multi-part GGUF quant (confirmed against
// unsloth/Kimi-K2.5-GGUF on 2026-08-04 - "Q3_K_S/Kimi-K2.5-Q3_K_S-00001-of-
// 00010.gguf" through "...-00010-of-00010.gguf") got offered as 10 separate
// one-click "Pull" variants, each only a fraction of the real total size.
// Ollama's own manifest resolution explicitly refuses these
// (https://github.com/ollama/ollama/issues/5245), and even where that
// wouldn't apply, one Pull click can never fetch all required parts through
// this one-file-per-variant flow - the whole shard set must be excluded.
func TestHandleModelRepo_ExcludesShardedGGUF(t *testing.T) {
	fakeHF := `{
		"id": "unsloth/Kimi-K2.5-GGUF",
		"downloads": 50000,
		"likes": 100,
		"tags": ["text-generation"],
		"lastModified": "2026-01-01T00:00:00.000Z",
		"siblings": [
			{"rfilename": "Q3_K_S/Kimi-K2.5-Q3_K_S-00001-of-00010.gguf", "size": 5000000000},
			{"rfilename": "Q3_K_S/Kimi-K2.5-Q3_K_S-00002-of-00010.gguf", "size": 5000000000},
			{"rfilename": "Q3_K_S/Kimi-K2.5-Q3_K_S-00003-of-00010.gguf", "size": 5000000000},
			{"rfilename": "Kimi-K2.5-Q2_K.gguf", "size": 40000000000}
		]
	}`
	origTransport := hfHTTPClient.Transport
	hfHTTPClient.Transport = stubRoundTripper{body: fakeHF}
	defer func() { hfHTTPClient.Transport = origTransport }()

	ollama := mockOllamaServer(t)
	defer ollama.Close()
	s := newModelFitTestServer(ollama.URL)

	req := httptest.NewRequest(http.MethodGet, "/admin/models/repo?id=unsloth/Kimi-K2.5-GGUF", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		Variants []ModelVariantFit `json:"variants"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	for _, v := range resp.Variants {
		if strings.Contains(strings.ToLower(v.Tag), "q3_k_s") {
			t.Errorf("variant %q is a sharded multi-part file - one Pull click can never fetch all 10 parts, must be excluded entirely", v.Tag)
		}
	}
	if len(resp.Variants) != 1 {
		t.Errorf("got %d variants, want exactly 1 (the single-file Q2_K) - all 3 sharded Q3_K_S parts must be filtered out", len(resp.Variants))
	}
}

// catalogResponse mirrors the JSON shape returned by handleModelCatalog.
type catalogResponse struct {
	Catalog []CatalogModel     `json:"catalog"`
	Nodes   []catalogNodeEntry `json:"nodes"`
}

func TestCatalogHasAtLeast25Models(t *testing.T) {
	if len(catalogModels) < 25 {
		t.Fatalf("catalog has %d models, want >= 25", len(catalogModels))
	}
	// Every model needs at least one variant and a recommended one.
	for _, m := range catalogModels {
		if m.Name == "" || m.DisplayName == "" {
			t.Errorf("model %q missing name/display_name", m.Name)
		}
		if len(m.Variants) == 0 {
			t.Errorf("model %q has no variants", m.Name)
		}
		if len(m.Categories) == 0 {
			t.Errorf("model %q has no categories", m.Name)
		}
		hasRec := false
		for _, v := range m.Variants {
			if v.VRAMEstMB <= 0 {
				t.Errorf("model %q variant %q has non-positive VRAM estimate", m.Name, v.Tag)
			}
			if v.Recommended {
				hasRec = true
			}
		}
		if !hasRec {
			t.Errorf("model %q has no recommended variant", m.Name)
		}
	}
}

func TestHandleModelCatalog_HappyPath(t *testing.T) {
	ollama := mockOllamaServer(t)
	defer ollama.Close()

	s := newModelFitTestServer(ollama.URL)

	req := httptest.NewRequest(http.MethodGet, "/admin/models/catalog", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var resp catalogResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(resp.Catalog) < 25 {
		t.Errorf("catalog returned %d models, want >= 25", len(resp.Catalog))
	}
	if len(resp.Nodes) != 1 {
		t.Fatalf("got %d nodes, want 1", len(resp.Nodes))
	}

	node := resp.Nodes[0]
	if node.VRAMSource != "nvidia-smi" {
		t.Errorf("vram_source = %q, want nvidia-smi", node.VRAMSource)
	}
	// Node has 8 GB total, 4 GB loaded by llama3:8b -> ~4 GB free.
	if len(node.Models) != len(catalogModels) {
		t.Fatalf("node has %d model entries, want %d", len(node.Models), len(catalogModels))
	}

	// Find a small model (llama3.2:1b, ~1GB) -> should be green. A 70B model
	// (~40GB) -> red. Verify classification ran against real free VRAM.
	fitByName := map[string]string{}
	for _, m := range node.Models {
		// Use the recommended variant's fit.
		for _, v := range m.Variants {
			if v.Recommended {
				fitByName[m.Name] = v.Fit
			}
		}
	}
	if got := fitByName["llama3.2:1b"]; got != "green" {
		t.Errorf("llama3.2:1b fit = %q, want green (fits in ~4GB free)", got)
	}
	if got := fitByName["llama3.1:70b"]; got != "red" {
		t.Errorf("llama3.1:70b fit = %q, want red (needs 40GB)", got)
	}
	// Regression: node has 8GB total, only ~4GB currently free (4GB
	// loaded by llama3:8b). llama3.1:8b needs ~4.7GB - more than the transient
	// free VRAM, but well within the node's 8GB total capacity. Classifying
	// against free VRAM would wrongly report "red" here.
	if got := fitByName["llama3.1:8b"]; got != "green" {
		t.Errorf("llama3.1:8b fit = %q, want green (fits within 8GB total capacity, even though only ~4GB is free right now)", got)
	}
	if node.VRAMTotalBytes != 8*1024*1024*1024 {
		t.Errorf("vram_total_bytes = %d, want 8GB", node.VRAMTotalBytes)
	}
}

func TestHandleModelCatalog_Downloaded(t *testing.T) {
	ollama := mockOllamaServer(t)
	defer ollama.Close()

	s := newModelFitTestServer(ollama.URL)

	req := httptest.NewRequest(http.MethodGet, "/admin/models/catalog", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	var resp catalogResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// mockOllamaServer reports llama3:8b downloaded. The catalog has no
	// "llama3:8b" entry, but cross-referencing must not panic and embedding
	// models (not downloaded) should be flagged false.
	for _, m := range resp.Nodes[0].Models {
		if m.Name == "nomic-embed-text" && m.Downloaded {
			t.Errorf("nomic-embed-text incorrectly flagged downloaded")
		}
	}
}

// TestHandleModelCatalog_GPUCountUnknown verifies the GPU-count-unknown disclosure: a node with no
// agent (or a "declared" whole-node VRAM total, i.e. no per-device reading)
// must be flagged gpu_count_unknown, distinct from an agent-confirmed
// reading (even a confirmed single-GPU one), which must not be flagged.
func TestHandleModelCatalog_GPUCountUnknown(t *testing.T) {
	ollama := mockOllamaServer(t)
	defer ollama.Close()

	// newModelFitTestServer's fixture node has VRAMSource="nvidia" (a direct
	// marbor-host nvidia-smi reading) and no agent - AgentPresent stays false,
	// so this is the "no agent at all" case Gap D targets.
	s := newModelFitTestServer(ollama.URL)

	req := httptest.NewRequest(http.MethodGet, "/admin/models/catalog", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	var resp catalogResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Nodes) != 1 {
		t.Fatalf("got %d nodes, want 1", len(resp.Nodes))
	}
	if !resp.Nodes[0].GPUCountUnknown {
		t.Error("GPUCountUnknown = false for a no-agent node, want true")
	}

	// An agent-confirmed reading (even reporting exactly one device) must
	// not be flagged - it's a real reading, not an inferred/declared guess.
	nodes := s.router.Nodes()
	nodes[0].Lock()
	nodes[0].AgentPresent = true
	nodes[0].AgentGPUs = []marboragent.GPUInfo{{Index: 0, VRAMTotalMB: 8192}}
	nodes[0].Unlock()

	// A fresh response struct for this decode - gpu_count_unknown carries
	// "omitempty", so a false value is simply absent from the JSON, and
	// reusing the earlier `resp` (already Nodes[0].GPUCountUnknown==true from
	// the first decode) would leave that stale true in place: encoding/json
	// only overwrites fields present in the payload, it does not zero out a
	// slice element it reuses from existing capacity for fields the new
	// payload omits.
	var resp2 catalogResponse
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/admin/models/catalog", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	s.Handler().ServeHTTP(w, req)
	if err := json.Unmarshal(w.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp2.Nodes[0].GPUCountUnknown {
		t.Error("GPUCountUnknown = true for an agent-confirmed reading, want false")
	}
}

func TestHandleModelCatalog_V1Route(t *testing.T) {
	ollama := mockOllamaServer(t)
	defer ollama.Close()

	s := newModelFitTestServer(ollama.URL)

	req := httptest.NewRequest(http.MethodGet, "/admin/v1/models/catalog", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("v1 route status = %d, want 200", w.Code)
	}
}

func TestHandleModelCatalog_Unauthorized(t *testing.T) {
	s := newModelFitTestServer("http://127.0.0.1:1")

	req := httptest.NewRequest(http.MethodGet, "/admin/models/catalog", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestClassifyFit(t *testing.T) {
	const gb = int64(1024 * 1024 * 1024)
	cases := []struct {
		name      string
		estBytes  int64
		freeBytes int64
		source    string
		want      string
	}{
		{"comfortable", 2 * gb, 8 * gb, "nvidia-smi", "green"},
		{"tight", 7 * gb, 8 * gb, "nvidia-smi", "yellow"},
		{"too-large", 40 * gb, 8 * gb, "nvidia-smi", "red"},
		{"unknown-source", 2 * gb, 0, "unknown", "unknown"},
		{"inferred-source", 2 * gb, 4 * gb, "inferred", "unknown"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyFit(c.estBytes, c.freeBytes, c.source); got != c.want {
				t.Errorf("classifyFit = %q, want %q", got, c.want)
			}
		})
	}
}

func TestNodeVRAMCapacity(t *testing.T) {
	cases := []struct {
		name            string
		vramTotalMB     int64
		agentGPUs       []marboragent.GPUInfo
		runtime         string
		declaredIndices []int
		wantMB          int64
		wantUsedMB      int64
		wantCount       int
		wantBasis       string
	}{
		{"no agent GPUs falls back to aggregate", 24000, nil, "ollama", nil, 24000, -1, 0, ""},
		{"single agent GPU falls back to aggregate", 24000, []marboragent.GPUInfo{{VRAMTotalMB: 24000}}, "ollama", nil, 24000, -1, 1, ""},
		{"multi-GPU ollama sums across devices", 24000, []marboragent.GPUInfo{{VRAMTotalMB: 24000}, {VRAMTotalMB: 24000}}, "ollama", nil, 48000, -1, 2, "combined"},
		{"multi-GPU llamacpp sums across devices", 24000, []marboragent.GPUInfo{{VRAMTotalMB: 16000}, {VRAMTotalMB: 8000}}, "llamacpp", nil, 24000, -1, 2, "combined"},
		{"multi-GPU empty runtime treated as shardable (matches ggufOnlyRuntime)", 24000, []marboragent.GPUInfo{{VRAMTotalMB: 12000}, {VRAMTotalMB: 12000}}, "", nil, 24000, -1, 2, "combined"},
		{"multi-GPU vllm uses largest device only, never the sum", 24000, []marboragent.GPUInfo{{VRAMTotalMB: 24000, VRAMUsedMB: 6000}, {VRAMTotalMB: 8000, VRAMUsedMB: 8000}}, "vllm", nil, 24000, 6000, 2, "largest"},
		{"multi-GPU tgi uses largest device only", 24000, []marboragent.GPUInfo{{VRAMTotalMB: 8000, VRAMUsedMB: 8000}, {VRAMTotalMB: 24000, VRAMUsedMB: 6000}}, "tgi", nil, 24000, 6000, 2, "largest"},
		{"multi-GPU mlx uses largest device only", 24000, []marboragent.GPUInfo{{VRAMTotalMB: 16000, VRAMUsedMB: 4000}, {VRAMTotalMB: 8000, VRAMUsedMB: 8000}}, "mlx", nil, 16000, 4000, 2, "largest"},
		{"multi-GPU devices report zero VRAM falls back to aggregate", 24000, []marboragent.GPUInfo{{}, {}}, "ollama", nil, 24000, -1, 2, ""},

		// Gap B: two host-scoped ollama nodes, each pinned to one of the
		// host's two GPUs (index 0 and index 1 respectively) via a declared
		// scope. Without scoping, ggufOnlyRuntime("ollama") would sum both
		// physical GPUs for EACH node - double-counting one pool of VRAM.
		// Declared to a single device, each must be sized against that
		// device alone, never vramTotalMB (the whole host's aggregate).
		{"Gap B: ollama node declared to GPU 0 only sizes against that device alone", 48000,
			[]marboragent.GPUInfo{{Index: 0, VRAMTotalMB: 24000, VRAMUsedMB: 5000}, {Index: 1, VRAMTotalMB: 24000, VRAMUsedMB: 9000}},
			"ollama", []int{0}, 24000, 5000, 1, ""},
		{"Gap B: sibling node declared to GPU 1 only sizes against that device alone", 48000,
			[]marboragent.GPUInfo{{Index: 0, VRAMTotalMB: 24000, VRAMUsedMB: 5000}, {Index: 1, VRAMTotalMB: 24000, VRAMUsedMB: 9000}},
			"ollama", []int{1}, 24000, 9000, 1, ""},

		// Gap C: vLLM would default to "largest" (single-device) sizing
		// for any undeclared multi-GPU set, even when tensor-parallel is
		// actually configured. A declared multi-index scope is itself the
		// operator's confirmation that this instance spans those GPUs, so it
		// must switch to "combined", overriding the runtime-name heuristic.
		{"Gap C: vllm declared to 2 GPUs (tensor-parallel) sums instead of largest-only", 48000,
			[]marboragent.GPUInfo{{Index: 0, VRAMTotalMB: 24000}, {Index: 1, VRAMTotalMB: 24000}, {Index: 2, VRAMTotalMB: 24000}},
			"vllm", []int{0, 1}, 48000, -1, 2, "combined"},

		// A declaration that matches none of the currently-reported devices
		// (stale/misconfigured) must not zero out sizing - falls back to the
		// unscoped (undeclared) behavior for that runtime.
		{"stale declaration matching no reported device falls back to unscoped", 48000,
			[]marboragent.GPUInfo{{Index: 0, VRAMTotalMB: 24000}, {Index: 1, VRAMTotalMB: 24000}},
			"ollama", []int{7}, 48000, -1, 2, "combined"},

		// A declared multi-GPU scope whose devices transiently report zero
		// VRAM (an agent telemetry glitch, not "nothing declared") must
		// report unknown (capacityMB 0), never fall back to vramTotalMB - the
		// whole HOST's aggregate would silently reintroduce the Gap B/C
		// double-count this declared-scope fix exists to prevent.
		{"Gap B/C: declared multi-GPU scope with zero-VRAM telemetry glitch reports unknown, not the host aggregate", 96000,
			[]marboragent.GPUInfo{{Index: 0, VRAMTotalMB: 0}, {Index: 1, VRAMTotalMB: 0}, {Index: 2, VRAMTotalMB: 24000}, {Index: 3, VRAMTotalMB: 24000}},
			"vllm", []int{0, 1}, 0, -1, 2, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotMB, gotUsedMB, gotCount, gotBasis := nodeVRAMCapacity(c.vramTotalMB, c.agentGPUs, c.runtime, c.declaredIndices)
			if gotMB != c.wantMB || gotUsedMB != c.wantUsedMB || gotCount != c.wantCount || gotBasis != c.wantBasis {
				t.Errorf("nodeVRAMCapacity = (%d, %d, %d, %q), want (%d, %d, %d, %q)", gotMB, gotUsedMB, gotCount, gotBasis, c.wantMB, c.wantUsedMB, c.wantCount, c.wantBasis)
			}
		})
	}
}

func TestClassifyDiskFit(t *testing.T) {
	cases := []struct {
		name         string
		sizeMB       int64
		diskFreeGB   float64
		diskTotalGB  float64
		agentPresent bool
		want         string
	}{
		{"comfortable", 4700, 100, 500, true, "ok"},
		{"exact fit", 1000, 1000 * 1024 * 1024 / 1e9, 500, true, "ok"}, // ~1GB needed, ~1GB free
		{"too-large", 40000, 10, 500, true, "insufficient"},
		{"no agent", 4700, 100, 500, false, "unknown"},
		{"agent present but never reported disk telemetry (no build/no syscall yet)", 4700, 0, 0, true, "unknown"},
		// Regression: a genuinely-full disk (real Statfs reading, DiskFreeGB=0
		// legitimately) must hard-block, not skip as "unknown" - DiskTotalGB>0
		// is what proves this is real telemetry, not an unreported field.
		{"disk genuinely full", 4700, 0, 500, true, "insufficient"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyDiskFit(c.sizeMB, c.diskFreeGB, c.diskTotalGB, c.agentPresent); got != c.want {
				t.Errorf("classifyDiskFit(%d, %.4f, %.4f, %v) = %q, want %q", c.sizeMB, c.diskFreeGB, c.diskTotalGB, c.agentPresent, got, c.want)
			}
		})
	}
}

// TestClassifyUnknownSizeDiskFit covers the unknown-size disk-fit fix: a pull with no known
// download size (any tag outside the curated catalog) has its own
// classification path, since classifyDiskFit's size-vs-free-space test has
// nothing to compare against. It must mirror classifyDiskFit's
// telemetry-unknown handling exactly, but apply a conservative headroom
// floor (10% of total, 5GB absolute) instead of a size comparison when
// telemetry is available.
func TestClassifyUnknownSizeDiskFit(t *testing.T) {
	cases := []struct {
		name         string
		diskFreeGB   float64
		diskTotalGB  float64
		agentPresent bool
		want         string
	}{
		{"comfortable headroom", 500, 1000, true, "ok"},
		{"exactly at the fraction floor (10%) with plenty of absolute GB", 100, 1000, true, "ok"},
		{"just under the fraction floor", 99, 1000, true, "insufficient"},
		{"exactly at the absolute floor on a tiny disk", 5, 40, true, "ok"},
		{"under the absolute floor even though fraction would pass", 4, 20, true, "insufficient"},
		{"no agent", 500, 1000, false, "unknown"},
		{"agent present but never reported disk telemetry", 0, 0, true, "unknown"},
		// Regression: a genuinely-full disk (real Statfs reading, DiskFreeGB=0
		// legitimately) must hard-block, not skip as "unknown" - mirrors
		// classifyDiskFit's own regression case.
		{"disk genuinely full", 0, 500, true, "insufficient"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyUnknownSizeDiskFit(c.diskFreeGB, c.diskTotalGB, c.agentPresent); got != c.want {
				t.Errorf("classifyUnknownSizeDiskFit(%.4f, %.4f, %v) = %q, want %q", c.diskFreeGB, c.diskTotalGB, c.agentPresent, got, c.want)
			}
		})
	}
}

func TestFindCatalogVariantSizeMB(t *testing.T) {
	// llama3.2:1b has exactly one variant, tag == model name, SizeMB 1300.
	if size, ok := findCatalogVariantSizeMB("llama3.2:1b"); !ok || size != 1300 {
		t.Errorf("findCatalogVariantSizeMB(llama3.2:1b) = (%d, %v), want (1300, true)", size, ok)
	}
	// Exact variant tag match (not the bare model name).
	if size, ok := findCatalogVariantSizeMB("llama3.1:8b-instruct-q8_0"); !ok || size != 8500 {
		t.Errorf("findCatalogVariantSizeMB(llama3.1:8b-instruct-q8_0) = (%d, %v), want (8500, true)", size, ok)
	}
	// Bare catalog name with multiple variants resolves to the recommended one.
	if size, ok := findCatalogVariantSizeMB("llama3.1:8b"); !ok || size != 4700 {
		t.Errorf("findCatalogVariantSizeMB(llama3.1:8b) = (%d, %v), want (4700, true) [recommended variant]", size, ok)
	}
	// Not in the static catalog at all (HF tag, or an uncurated Ollama registry name).
	if _, ok := findCatalogVariantSizeMB("hf.co/someorg/somerepo:Q4_K_M"); ok {
		t.Error("findCatalogVariantSizeMB(hf.co/...) = ok=true, want false (unresolvable, must not guess)")
	}
	if _, ok := findCatalogVariantSizeMB("some-uncurated-model:latest"); ok {
		t.Error("findCatalogVariantSizeMB(uncurated model) = ok=true, want false")
	}
}

// TestGPUCountUnknown covers the GPU-count-unknown disclosure helper directly,
// including the case a code-review pass found missing: an agent that is
// present and polling successfully but has not (yet) reported any GPU
// devices must still be flagged unknown, not shown as a confirmed reading.
func TestGPUCountUnknown(t *testing.T) {
	cases := []struct {
		name         string
		agentPresent bool
		vramSource   string
		gpuCount     int
		want         bool
	}{
		{"no agent at all", false, "nvidia", 1, true},
		{"declared whole-node total, no per-device breakdown", true, "declared", 0, true},
		{"agent present but zero GPU devices reported", true, "agent", 0, true},
		{"agent-confirmed single GPU reading", true, "nvidia", 1, false},
		{"agent-confirmed multi-GPU reading", true, "agent", 2, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := gpuCountUnknown(c.agentPresent, c.vramSource, c.gpuCount); got != c.want {
				t.Errorf("gpuCountUnknown(%v, %q, %d) = %v, want %v", c.agentPresent, c.vramSource, c.gpuCount, got, c.want)
			}
		})
	}
}

func TestGgufOnlyRuntime(t *testing.T) {
	cases := []struct {
		runtime string
		want    bool
	}{
		{"", true},
		{"ollama", true},
		{"llamacpp", true},
		{"vllm", false},
		{"tgi", false},
		{"mlx", false},
		{"auto", false},
	}
	for _, c := range cases {
		if got := ggufOnlyRuntime(c.runtime); got != c.want {
			t.Errorf("ggufOnlyRuntime(%q) = %v, want %v", c.runtime, got, c.want)
		}
	}
}

func TestDetectSafetensorsQuant(t *testing.T) {
	cases := []struct {
		tags []string
		want string
	}{
		{[]string{"text-generation", "awq"}, "AWQ"},
		{[]string{"text-generation", "gptq"}, "GPTQ"},
		{[]string{"text-generation", "bitsandbytes"}, "BNB"},
		{[]string{"text-generation", "transformers"}, "FP16/BF16"},
		{[]string{"text-generation", "mlx"}, "MLX"},
	}
	for _, c := range cases {
		if got := detectSafetensorsQuant(c.tags); got != c.want {
			t.Errorf("detectSafetensorsQuant(%v) = %q, want %q", c.tags, got, c.want)
		}
	}
}

// TestHandleModelCatalog_Capabilities is a regression test for the Model
// Advisor Pull-button bug: the frontend used to hardcode
// actualRuntime === 'ollama' to decide whether to show "Pull", ignoring the
// Marbor Agent's real "models.pull" capability entirely for vllm/tgi/llamacpp/
// mlx nodes. The fix requires the catalog response to carry the node's
// effective capability list, computed exactly like handleNodePull's own
// direct-vs-agent dispatch decision - not the raw agent-advertised list -
// so a node whose agent is present but disabled in settings must report no
// capabilities, even though it last polled some.
func TestHandleModelCatalog_Capabilities(t *testing.T) {
	// Each node needs its own host:port - SetMarborAgent keys agent config by
	// host, so nodes sharing one URL would clobber each other's agent config.
	ollamaA := mockOllamaServer(t)
	defer ollamaA.Close()
	ollamaB := mockOllamaServer(t)
	defer ollamaB.Close()
	ollamaC := mockOllamaServer(t)
	defer ollamaC.Close()

	// hostOrDefault falls back to url.Hostname(), which strips the port - all
	// three mock servers bind to 127.0.0.1, so distinct Host values must be
	// set explicitly or SetMarborAgent (keyed by host) would clobber each
	// other's config despite each node having a different URL/port.
	r := router.New(config.RoutingConfig{Strategy: "warm-first", Fallback: "least-connections", PollIntervalMs: 60000}, []config.NodeConfig{
		{Name: "agent-enabled", URL: ollamaA.URL, Host: "host-a", Runtime: "vllm"},
		{Name: "agent-disabled", URL: ollamaB.URL, Host: "host-b", Runtime: "tgi"},
		{Name: "no-agent", URL: ollamaC.URL, Host: "host-c", Runtime: "ollama"},
	}, nil)

	enabledHost, _ := r.NodeHost("agent-enabled")
	r.SetMarborAgent(enabledHost, true, 9999, "token-a", "http")
	disabledHost, _ := r.NodeHost("agent-disabled")
	r.SetMarborAgent(disabledHost, false, 9999, "token-b", "http")

	for _, n := range r.Nodes() {
		n.Lock()
		switch n.Name {
		case "agent-enabled":
			n.AgentCapabilities = []string{"status", "models.pull", "models.list"}
		case "agent-disabled":
			// Agent last reported these capabilities before being disabled in
			// settings - the effective list must still come out empty.
			n.AgentCapabilities = []string{"status", "models.pull"}
		}
		n.Unlock()
	}

	s := NewServer(r, nil, config.Config{})

	req := httptest.NewRequest(http.MethodGet, "/admin/models/catalog", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	var resp catalogResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	byName := map[string]catalogNodeEntry{}
	for _, n := range resp.Nodes {
		byName[n.Name] = n
	}

	if got := byName["agent-enabled"]; got.Runtime != "vllm" {
		t.Errorf("agent-enabled runtime = %q, want vllm", got.Runtime)
	}
	hasPull := false
	for _, c := range byName["agent-enabled"].Capabilities {
		if c == "models.pull" {
			hasPull = true
		}
	}
	if !hasPull {
		t.Errorf("agent-enabled capabilities = %v, want to include models.pull", byName["agent-enabled"].Capabilities)
	}

	if caps := byName["agent-disabled"].Capabilities; len(caps) != 0 {
		t.Errorf("agent-disabled capabilities = %v, want empty (agent disabled in settings must not report last-known capabilities)", caps)
	}

	if caps := byName["no-agent"].Capabilities; len(caps) != 0 {
		t.Errorf("no-agent capabilities = %v, want empty", caps)
	}
}

func TestClassifyPullTagFormat(t *testing.T) {
	cases := []struct {
		model string
		want  string
	}{
		{"llama3.2:3b", "ollama-library"},
		{"qwen2.5-coder:32b", "ollama-library"},
		{"nomic-embed-text", "ollama-library"},
		{"hf.co/unsloth/Kimi-K2.5-GGUF:Q4_K_M", "gguf-hf"},
		{"hf.co/unsloth/Kimi-K2.5-GGUF", "gguf-hf"},
		{"some/repo/model.GGUF", "gguf-hf"},
		{"meta-llama/Llama-3.1-8B-Instruct", "hf-repo"},
		{"someuser/somemodel", "hf-repo"},
	}
	for _, c := range cases {
		if got := classifyPullTagFormat(c.model); got != c.want {
			t.Errorf("classifyPullTagFormat(%q) = %q, want %q", c.model, got, c.want)
		}
	}
}

func TestPullFormatIncompatible(t *testing.T) {
	cases := []struct {
		format  string
		runtime string
		want    bool
	}{
		{"ollama-library", "", false},
		{"ollama-library", "ollama", false},
		{"ollama-library", "llamacpp", true},
		{"ollama-library", "vllm", true},
		{"ollama-library", "tgi", true},
		{"ollama-library", "mlx", true},
		{"gguf-hf", "", false},
		{"gguf-hf", "ollama", false},
		{"gguf-hf", "llamacpp", false},
		{"gguf-hf", "vllm", true},
		{"gguf-hf", "tgi", true},
		{"gguf-hf", "mlx", true},
		{"hf-repo", "ollama", false},
		{"hf-repo", "vllm", false},
		{"hf-repo", "tgi", false},
		{"hf-repo", "llamacpp", false},
		{"hf-repo", "mlx", false},
	}
	for _, c := range cases {
		if got := pullFormatIncompatible(c.format, c.runtime); got != c.want {
			t.Errorf("pullFormatIncompatible(%q, %q) = %v, want %v", c.format, c.runtime, got, c.want)
		}
	}
}

// TestHandleModelCatalog_IncompatibleRuntime is a regression test for the
// runtime-compatibility gate:
// the compiled catalog's tags are all Ollama-library-format, so a node
// running any other runtime must see "incompatible" (not a capacity-based
// green/yellow/red) for every variant, regardless of how much VRAM it has.
func TestHandleModelCatalog_IncompatibleRuntime(t *testing.T) {
	ollama := mockOllamaServer(t)
	defer ollama.Close()

	r := router.New(config.RoutingConfig{Strategy: "warm-first", Fallback: "least-connections", PollIntervalMs: 60000}, []config.NodeConfig{
		{Name: "llamacpp-node", URL: ollama.URL, Runtime: "llamacpp"},
	}, nil)
	nodes := r.Nodes()
	nodes[0].Lock()
	nodes[0].Healthy = true
	nodes[0].VRAMTotalMB = 80 * 1024 // 80 GB - plenty of capacity for every catalog model
	nodes[0].VRAMSource = "declared"
	nodes[0].Unlock()

	s := NewServer(r, nil, config.Config{})

	req := httptest.NewRequest(http.MethodGet, "/admin/models/catalog", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}

	var resp catalogResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Nodes) != 1 {
		t.Fatalf("got %d nodes, want 1", len(resp.Nodes))
	}
	for _, m := range resp.Nodes[0].Models {
		for _, v := range m.Variants {
			if v.Fit != "incompatible" {
				t.Errorf("model %q variant %q fit = %q on a llamacpp node, want incompatible (ample VRAM must never mask a format mismatch)", m.Name, v.Tag, v.Fit)
			}
		}
	}
}

func TestExtractQuantization(t *testing.T) {
	cases := []struct {
		filename string
		want     string
	}{
		{"Llama-3.2-1B-Instruct-Q4_K_M.gguf", "Q4_K_M"},
		{"model-q8_0.gguf", "Q8_0"},
		{"phi3-fp16.gguf", "FP16"},
		{"custom_model-IQ4_XS.gguf", "IQ4_XS"},
		{"llama-3-8b.gguf", "GGUF"},
		{"some-other-quant-format-bf16.gguf", "BF16"},
		{"no_hyphen_q4_k_m.gguf", "Q4_K_M"},
		{"q4_k_m-in-middle-but-ends-with-something.gguf", "Q4_K_M"},
	}

	for _, c := range cases {
		if got := extractQuantization(c.filename); got != c.want {
			t.Errorf("extractQuantization(%q) = %q, want %q", c.filename, got, c.want)
		}
	}
}

// TestClassifyDiskFitOversizeClearedShapeIsUnknown pins how a node whose
// agent is present but whose live disk readings were cleared (an oversize
// status reply zeroes DiskFreeGB and DiskTotalGB) is read: "unknown", never a
// fabricated "insufficient" from comparing a model size to zero free space.
func TestClassifyDiskFitOversizeClearedShapeIsUnknown(t *testing.T) {
	if !diskTelemetryUnknown(0, true) {
		t.Error("diskTelemetryUnknown(0, true) = false, want true")
	}
	if got := classifyDiskFit(4096, 0, 0, true); got != "unknown" {
		t.Errorf("classifyDiskFit with cleared disk and agent present = %q, want unknown", got)
	}
}

// TestDiskFitOversizeAgentReplyReadsUnknownFromRealNode drives the disk-fit
// classification from a real node state: a node whose agent first reported
// disk figures and then went oversize must classify as unknown (never a
// fabricated insufficient), while the same node before going oversize
// classifies from its real figures.
func TestDiskFitOversizeAgentReplyReadsUnknownFromRealNode(t *testing.T) {
	var oversize atomic.Bool
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if oversize.Load() {
			_, _ = w.Write([]byte(`{"agent": {"version": "v1.0.0"}}` + strings.Repeat(" ", 8<<20)))
			return
		}
		_ = json.NewEncoder(w).Encode(marboragent.Telemetry{
			Agent: marboragent.Agent{Version: "v1.0.0", ProtocolVersion: 1, NodeID: "node-id-1"},
			Host:  &marboragent.HostTelemetry{DiskFreeGB: 100, DiskTotalGB: 500},
		})
	}))
	defer agent.Close()
	agentPort, err := strconv.Atoi(strings.TrimPrefix(agent.URL, "http://127.0.0.1:"))
	if err != nil {
		t.Fatalf("parse agent port from %q: %v", agent.URL, err)
	}

	r := router.New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 50}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://127.0.0.1:1"},
	}, nil)
	host, _ := r.NodeHost("gpu-0")
	r.SetMarborAgent(host, true, agentPort, "tok", "http")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Start(ctx) }()
	defer func() { cancel(); <-done }()

	waitDisk := func(wantUnknown bool) (free, total float64, present bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			free, total, present = nodeDiskState(r.Nodes(), "gpu-0")
			// Unknown disk telemetry reads as a zero total.
			if present {
				if wantUnknown && total == 0 {
					return
				}
				if !wantUnknown && total > 0 {
					return
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("node never reached disk state with unknown=%v (free=%v total=%v present=%v)", wantUnknown, free, total, present)
		return
	}

	free, total, present := waitDisk(false)
	if got := classifyDiskFit(4096, free, total, present); got != "ok" {
		t.Fatalf("setup: classifyDiskFit with real figures %v/%v = %q, want ok", free, total, got)
	}
	oversize.Store(true)
	free, total, present = waitDisk(true)
	if !present {
		t.Fatal("agent must stay present on an oversize reply")
	}
	if got := classifyDiskFit(4096, free, total, present); got != "unknown" {
		t.Errorf("classifyDiskFit after an oversize reply = %q, want unknown", got)
	}
	if got := classifyUnknownSizeDiskFit(free, total, present); got != "unknown" {
		t.Errorf("classifyUnknownSizeDiskFit after an oversize reply = %q, want unknown", got)
	}
}

// TestIsMediaPipelineTag pins the exact set of Hugging Face pipeline tags a
// GGUF runtime's search drops, and the tags it must keep. An unknown or new
// tag fails open (the row is shown) on purpose.
func TestIsMediaPipelineTag(t *testing.T) {
	dropped := []string{
		"text-to-speech", "text-to-audio", "automatic-speech-recognition",
		"audio-to-audio", "audio-classification", "voice-activity-detection",
		"text-to-image", "image-to-image", "image-to-video", "text-to-video",
		"image-text-to-video", "video-classification", "image-classification",
		"object-detection", "image-segmentation", "depth-estimation",
		"unconditional-image-generation", "image-to-3d", "text-to-3d",
		"zero-shot-image-classification", "zero-shot-object-detection",
		"mask-generation", "keypoint-detection", "video-to-video",
	}
	kept := []string{
		"", "text-generation", "image-text-to-text", "any-to-any",
		"audio-text-to-text", "image-to-text", "text-ranking",
		"feature-extraction", "sentence-similarity", "reinforcement-learning",
		"some-brand-new-tag",
	}
	for _, tag := range dropped {
		if !isMediaPipelineTag(tag) {
			t.Errorf("isMediaPipelineTag(%q) = false, want true", tag)
		}
	}
	for _, tag := range kept {
		if isMediaPipelineTag(tag) {
			t.Errorf("isMediaPipelineTag(%q) = true, want false", tag)
		}
	}
}

// recordingHFTransport records every request URL and answers with a canned body.
type recordingHFTransport struct {
	body string
	mu   sync.Mutex
	urls []string
}

func (rt *recordingHFTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	rt.urls = append(rt.urls, r.URL.String())
	rt.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(rt.body)),
		Header:     make(http.Header),
	}, nil
}

// calls returns a copy of the recorded request URLs, taken under the lock.
func (rt *recordingHFTransport) calls() []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return append([]string(nil), rt.urls...)
}

// Deliberately unsorted by downloads: the single-request GGUF path must keep
// the order Hugging Face returned, so a re-sort would change the result.
const mixedHFSearchBody = `[
	{"id":"org/untagged-llm-GGUF","downloads":800},
	{"id":"org/tts-GGUF","downloads":700,"pipeline_tag":"text-to-speech"},
	{"id":"org/real-llm-GGUF","downloads":100,"pipeline_tag":"text-generation"},
	{"id":"org/vlm-GGUF","downloads":600,"pipeline_tag":"image-text-to-text"},
	{"id":"org/image-gen-GGUF","downloads":500,"pipeline_tag":"text-to-image"},
	{"id":"org/embed-GGUF","downloads":900,"pipeline_tag":"feature-extraction"},
	{"id":"org/asr-GGUF","downloads":300,"pipeline_tag":"automatic-speech-recognition"}
]`

// searchWithTransport swaps the process-wide hfHTTPClient transport, so tests
// that use it must not call t.Parallel.
func searchWithTransport(t *testing.T, rt http.RoundTripper, path string) *httptest.ResponseRecorder {
	t.Helper()
	orig := hfHTTPClient.Transport
	hfHTTPClient.Transport = rt
	defer func() { hfHTTPClient.Transport = orig }()

	ollama := mockOllamaServer(t)
	defer ollama.Close()
	s := newModelFitTestServer(ollama.URL)

	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

func searchIDs(t *testing.T, w *httptest.ResponseRecorder) []string {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var models []HFModelInfo
	if err := json.Unmarshal(w.Body.Bytes(), &models); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	return ids
}

// hfQueryParam parses a recorded upstream URL and returns one query value.
func hfQueryParam(t *testing.T, raw, key string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Query().Get(key)
}

// numberedHFBody builds n rows org/m0..m(n-1) with descending downloads; the
// first mediaRows carry a media pipeline tag.
func numberedHFBody(n, mediaRows int) string {
	var sb strings.Builder
	sb.WriteString("[")
	for i := 0; i < n; i++ {
		tag := "text-generation"
		if i < mediaRows {
			tag = "text-to-speech"
		}
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"id":"org/m` + strconv.Itoa(i) + `","downloads":` + strconv.Itoa(1000-i) + `,"likes":` + strconv.Itoa(n-i) + `,"pipeline_tag":"` + tag + `"}`)
	}
	sb.WriteString("]")
	return sb.String()
}

func TestHandleModelSearch_GGUFRuntimesDropMediaModels(t *testing.T) {
	// The runtime param omitted entirely behaves as a GGUF runtime too.
	paths := map[string]string{
		"omitted":  "/admin/models/search?q=x",
		"empty":    "/admin/models/search?q=x&runtime=",
		"ollama":   "/admin/models/search?q=x&runtime=ollama",
		"llamacpp": "/admin/models/search?q=x&runtime=llamacpp",
	}
	for name, path := range paths {
		t.Run(name, func(t *testing.T) {
			rt := &recordingHFTransport{body: mixedHFSearchBody}
			ids := searchIDs(t, searchWithTransport(t, rt, path))

			// HF order is kept: the input is not sorted by downloads.
			want := []string{"org/untagged-llm-GGUF", "org/real-llm-GGUF", "org/vlm-GGUF", "org/embed-GGUF"}
			if strings.Join(ids, ",") != strings.Join(want, ",") {
				t.Errorf("got %v, want %v", ids, want)
			}
			urls := rt.calls()
			if len(urls) != 1 {
				t.Fatalf("%d upstream requests, want exactly 1", len(urls))
			}
			if hfQueryParam(t, urls[0], "pipeline_tag") != "" {
				t.Errorf("upstream URL must not carry pipeline_tag: %s", urls[0])
			}
			if got := hfQueryParam(t, urls[0], "filter"); got != "gguf" {
				t.Errorf("filter = %q, want gguf: %s", got, urls[0])
			}
			if got := hfQueryParam(t, urls[0], "limit"); got != "50" {
				t.Errorf("limit = %q, want 50: %s", got, urls[0])
			}
		})
	}
}

func TestHandleModelSearch_GGUFTrimsToPageSizeAfterFilter(t *testing.T) {
	// 10 media rows first, then 40 text rows: 25 text rows must survive.
	w := searchWithTransport(t, &recordingHFTransport{body: numberedHFBody(50, 10)}, "/admin/models/search?runtime=ollama")
	ids := searchIDs(t, w)
	if len(ids) != 25 {
		t.Fatalf("got %d rows, want 25 (filter first, then trim)", len(ids))
	}
	if ids[0] != "org/m10" {
		t.Errorf("first row = %q, want org/m10 (media rows removed, order kept)", ids[0])
	}
	if ids[24] != "org/m34" {
		t.Errorf("last row = %q, want org/m34", ids[24])
	}
}

// The min_* filters must run on all fetched rows before the trim, so a
// filtered GGUF search still returns a full page of qualifying rows.
func TestHandleModelSearch_GGUFMinDownloadsFiltersBeforeTrim(t *testing.T) {
	// 50 fetched rows: m0..m29 are popular-less (10 downloads), m30..m49 have
	// 1000. With min_downloads=500 only m30..m49 qualify. A trim to 25 before
	// the filter would keep m0..m24, none of which qualify, and return nothing.
	var sb strings.Builder
	sb.WriteString("[")
	for i := 0; i < 50; i++ {
		d := 10
		if i >= 30 {
			d = 1000
		}
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"id":"org/m` + strconv.Itoa(i) + `","downloads":` + strconv.Itoa(d) + `,"likes":1,"pipeline_tag":"text-generation"}`)
	}
	sb.WriteString("]")
	w := searchWithTransport(t, &recordingHFTransport{body: sb.String()},
		"/admin/models/search?runtime=ollama&min_downloads=500")
	ids := searchIDs(t, w)
	if len(ids) != 20 {
		t.Fatalf("got %d rows, want the 20 qualifying rows out of the 50 fetched", len(ids))
	}
	if ids[0] != "org/m30" || ids[19] != "org/m49" {
		t.Errorf("rows = %s..%s, want org/m30..org/m49", ids[0], ids[19])
	}
}

func TestHandleModelSearch_MultiTagTrimsToPageSize(t *testing.T) {
	for _, runtime := range []string{"vllm", "tgi", "mlx"} {
		t.Run(runtime, func(t *testing.T) {
			rt := &recordingHFTransport{body: numberedHFBody(30, 0)}
			ids := searchIDs(t, searchWithTransport(t, rt, "/admin/models/search?runtime="+runtime))
			if len(ids) != 25 {
				t.Fatalf("got %d rows, want 25", len(ids))
			}
			if ids[24] != "org/m24" {
				t.Errorf("last row = %q, want org/m24", ids[24])
			}
		})
	}
}

func TestHandleModelSearch_NonGGUFRuntimesDoNotApplyMediaFilter(t *testing.T) {
	filters := map[string]string{"vllm": "safetensors", "tgi": "safetensors", "mlx": "mlx"}
	for runtime, filter := range filters {
		t.Run(runtime, func(t *testing.T) {
			rt := &recordingHFTransport{body: mixedHFSearchBody}
			ids := searchIDs(t, searchWithTransport(t, rt, "/admin/models/search?runtime="+runtime))
			urls := rt.calls()
			if len(urls) == 0 {
				t.Fatal("no upstream request")
			}
			for _, u := range urls {
				if hfQueryParam(t, u, "pipeline_tag") == "" || hfQueryParam(t, u, "filter") != filter {
					t.Errorf("upstream URL must carry filter=%s and a pipeline_tag: %s", filter, u)
				}
				if got := hfQueryParam(t, u, "limit"); got != "25" {
					t.Errorf("limit = %q, want 25: %s", got, u)
				}
			}
			found := false
			for _, id := range ids {
				if id == "org/tts-GGUF" {
					found = true
				}
			}
			if !found {
				t.Errorf("results must pass through unfiltered by the media denylist, got %v", ids)
			}
		})
	}
}

func TestDropMediaModels(t *testing.T) {
	t.Run("keeps order and does not mutate input", func(t *testing.T) {
		in := []HFModelInfo{
			{ID: "a", PipelineTag: "text-generation"},
			{ID: "b", PipelineTag: "text-to-speech"},
			{ID: "c"},
		}
		out := dropMediaModels(in)
		if len(out) != 2 || out[0].ID != "a" || out[1].ID != "c" {
			t.Errorf("got %+v, want a then c", out)
		}
		if len(in) != 3 || in[1].ID != "b" || in[2].ID != "c" {
			t.Errorf("input was mutated: %+v", in)
		}
	})
	t.Run("empty input", func(t *testing.T) {
		if out := dropMediaModels(nil); len(out) != 0 {
			t.Errorf("got %+v, want empty", out)
		}
	})
	t.Run("all media", func(t *testing.T) {
		out := dropMediaModels([]HFModelInfo{{ID: "a", PipelineTag: "text-to-image"}, {ID: "b", PipelineTag: "video-to-video"}})
		if len(out) != 0 {
			t.Errorf("got %+v, want empty", out)
		}
	})
}

type errHFTransport struct{}

func (errHFTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusInternalServerError,
		Body:       io.NopCloser(strings.NewReader("boom")),
		Header:     make(http.Header),
	}, nil
}

func TestHandleModelSearch_GGUFErrorStaysCorrelated(t *testing.T) {
	w := searchWithTransport(t, errHFTransport{}, "/admin/models/search?runtime=ollama")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "failed to fetch Hugging Face model list") {
		t.Errorf("error body should be the correlated generic message, got %s", body)
	}
	if strings.Contains(body, "status 500") || strings.Contains(body, "boom") {
		t.Errorf("upstream detail must not leak to the client: %s", body)
	}
}

func TestHandleModelSearch_GGUFMalformedJSONStaysCorrelated(t *testing.T) {
	w := searchWithTransport(t, &recordingHFTransport{body: `{"leaky":"detail"`}, "/admin/models/search?runtime=ollama")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "failed to fetch Hugging Face model list") {
		t.Errorf("error body should be the correlated generic message, got %s", body)
	}
	if strings.Contains(body, "leaky") || strings.Contains(body, "decode") {
		t.Errorf("decode detail must not leak to the client: %s", body)
	}
}

// An oversized Hugging Face reply must produce an error, never a partial list.
func TestFetchHFModelList_OversizedBodyIsAnError(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("[")
	for sb.Len() < maxHFListBytes+1024 {
		sb.WriteString(`{"id":"org/m","downloads":1},`)
	}
	sb.WriteString(`{"id":"org/last"}]`)

	orig := hfHTTPClient.Transport
	hfHTTPClient.Transport = &recordingHFTransport{body: sb.String()}
	defer func() { hfHTTPClient.Transport = orig }()

	s := newModelFitTestServer("http://127.0.0.1:1")
	models, err := s.fetchHFModelList(context.Background(), "https://huggingface.co/api/models")
	if err == nil {
		t.Fatalf("got %d models and no error, want an error for an oversized body", len(models))
	}
	if models != nil {
		t.Errorf("models = %d rows, want nil on error", len(models))
	}
	if !strings.Contains(err.Error(), "exceeded 8 MiB") {
		t.Errorf("error %q should name the 8 MiB cap", err)
	}
}

// A runtime outside the known lists (here sglang) gets exactly one request
// with no format filter, no pipeline_tag and no media dropping: the page makes
// no claim for it either.
func TestHandleModelSearch_UnlistedRuntimeIsUnfiltered(t *testing.T) {
	if f, tags := hfSearchFilters("sglang"); f != "" || tags != nil {
		t.Fatalf("hfSearchFilters(sglang) = %q, %v, want no filters", f, tags)
	}
	rt := &recordingHFTransport{body: mixedHFSearchBody}
	ids := searchIDs(t, searchWithTransport(t, rt, "/admin/models/search?q=x&runtime=sglang"))
	urls := rt.calls()
	if len(urls) != 1 {
		t.Fatalf("%d upstream requests, want exactly 1", len(urls))
	}
	for _, p := range []string{"filter", "pipeline_tag"} {
		if got := hfQueryParam(t, urls[0], p); got != "" {
			t.Errorf("%s = %q, want unset: %s", p, got, urls[0])
		}
	}
	if !slices.Contains(ids, "org/tts-GGUF") {
		t.Errorf("sglang search must not drop media-tagged rows, got %v", ids)
	}
}

// The advisor page note reads "Fit cannot be computed" for a node whose VRAM
// total is unknown or only inferred. The UI helper nodeVramUnknown mirrors
// these cases, so this pins the server side of that pairing: a node with no
// total or an inferred total always classifies as unknown, and a node with a
// declared or detected total never does.
func TestClassifyFit_UnknownCasesMirrorUINodeVramUnknown(t *testing.T) {
	const gib = int64(1) << 30
	cases := []struct {
		name     string
		est, cap int64
		source   string
		want     string
	}{
		{"unknown source", 4 * gib, 24 * gib, "unknown", "unknown"},
		{"inferred source", 4 * gib, 24 * gib, "inferred", "unknown"},
		{"zero total with unknown source", 4 * gib, 0, "unknown", "unknown"},
		{"zero total with inferred source", 4 * gib, 0, "inferred", "unknown"},
		// Known divergence, documented rather than hidden: the UI helper
		// treats a zero total as unknown whatever the source, but classifyFit
		// only looks at the source and returns "red" for zero capacity. The
		// catalog never produces this input (it keeps source "unknown" when
		// there is no capacity), so the two agree on every reachable node.
		{"zero total with declared source", 4 * gib, 0, "declared", "red"},
		{"zero total with detected source", 4 * gib, 0, "nvidia-smi", "red"},
		{"declared total", 4 * gib, 24 * gib, "declared", "green"},
		{"agent total", 4 * gib, 24 * gib, "agent", "green"},
		{"detected total", 4 * gib, 24 * gib, "nvidia-smi", "green"},
		{"api total", 4 * gib, 24 * gib, "api", "green"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyFit(c.est, c.cap, c.source); got != c.want {
				t.Errorf("classifyFit(%d, %d, %q) = %q, want %q", c.est, c.cap, c.source, got, c.want)
			}
		})
	}
}
