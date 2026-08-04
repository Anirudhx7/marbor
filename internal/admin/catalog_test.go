package admin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	// Regression for P47: node has 8GB total, only ~4GB currently free (4GB
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
